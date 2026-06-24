package multiagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"cyberstrike-ai/internal/agent"
	"cyberstrike-ai/internal/blackboard"
	"cyberstrike-ai/internal/config"
	"cyberstrike-ai/internal/database"
	"cyberstrike-ai/internal/reasoning"

	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
)

// ensureProjectForConversation 确保对话绑定了一个项目（黑板的 Fact/Intent/Hint 均按项目隔离）。
// 已绑定 → 直接返回；未绑定 → 为该对话创建一个临时项目并绑定，返回新项目 ID。
func ensureProjectForConversation(db *database.DB, conversationID, projectID string) (string, error) {
	if pid := strings.TrimSpace(projectID); pid != "" {
		if _, err := db.GetProject(pid); err == nil {
			return pid, nil
		}
	}
	if strings.TrimSpace(conversationID) != "" {
		if pid, err := db.GetConversationProjectID(conversationID); err == nil && strings.TrimSpace(pid) != "" {
			return strings.TrimSpace(pid), nil
		}
	}
	// 创建临时项目并绑定到对话。
	name := "黑板会话 " + time.Now().Format("2006-01-02 15:04")
	p, err := db.CreateProject(&database.Project{
		Name:        name,
		Description: "由 OODA 黑板编排自动创建（conversation=" + conversationID + "）",
		Status:      "active",
	})
	if err != nil {
		return "", fmt.Errorf("自动创建黑板项目失败: %w", err)
	}
	if strings.TrimSpace(conversationID) != "" {
		if err := db.SetConversationProjectID(conversationID, p.ID); err != nil {
			// 非致命：项目已建，绑定失败仅影响后续自动复用。
			return p.ID, nil
		}
	}
	return p.ID, nil
}

// RunBlackboardAgent 以 Cairn 风格的黑板 + OODA 循环执行一轮对话。
// 签名与 RunDeepAgent 对齐，作为 orchestration == "blackboard" 的实现，复用上层 SSE / 存储收尾。
//
// 流程：Bootstrap（high，直解或拆解为 Fact/Intent）→ 循环 { Reason（high，读全图、判完成、生成意图）
// → Explore（low，逐个认领意图、用真实工具执行、写回 Fact）} → 收敛或显式完成 → 综合答复。
func RunBlackboardAgent(
	ctx context.Context,
	appCfg *config.Config,
	ma *config.MultiAgentConfig,
	ag *agent.Agent,
	db *database.DB,
	logger *zap.Logger,
	conversationID string,
	projectID string,
	userMessage string,
	history []agent.ChatMessage,
	roleTools []string,
	progress func(eventType, message string, data interface{}),
	reasoningClient *reasoning.ClientIntent,
	systemPromptExtra string,
) (*RunResult, error) {
	if appCfg == nil || ma == nil || ag == nil || db == nil {
		return nil, fmt.Errorf("blackboard: 配置 / Agent / DB 为空")
	}
	goal := strings.TrimSpace(userMessage)
	if goal == "" {
		return nil, fmt.Errorf("blackboard: 用户目标为空")
	}
	if progress == nil {
		progress = func(string, string, interface{}) {}
	}

	pid, err := ensureProjectForConversation(db, conversationID, projectID)
	if err != nil {
		return nil, err
	}

	// 双模型分层：Bootstrap/Reason→high，Explore→low。models 未配置时两档都回退到 openai。
	tm := prepareTierModels(appCfg, reasoningClient, logger)
	if logger != nil {
		logger.Info("黑板 OODA 编排启动",
			zap.String("project_id", pid),
			zap.String("conversation_id", conversationID),
			zap.String("high_model", tm.highCfg.Model),
			zap.String("low_model", tm.lowCfg.Model))
	}

	board := blackboard.New(db, pid, conversationID)
	board.OnChange(func(c blackboard.Change) {
		et, data := boardChangeToProgress(c)
		data["conversationId"] = conversationID
		data["projectId"] = pid
		progress(et, c.Summary, data)
	})

	// 工具集：真实 MCP 工具（按角色过滤）+ 合成黑板工具。
	realDefs := ag.ToolsForRole(roleTools)
	realInfos, realSet, err := buildToolInfos(realDefs)
	if err != nil {
		return nil, err
	}
	writeBoardInfos, _, err := buildToolInfos([]agent.Tool{recordFactTool(), declareIntentTool(), goalCompleteTool()})
	if err != nil {
		return nil, err
	}
	// Reason 只用「声明意图 / 完成目标」，不直接执行真实工具。
	reasonBoardInfos, _, err := buildToolInfos([]agent.Tool{declareIntentTool(), goalCompleteTool()})
	if err != nil {
		return nil, err
	}

	eng := &oodaEngine{
		ctx: ctx, appCfg: appCfg, ag: ag, db: db, logger: logger,
		conversationID: conversationID, projectID: pid, goal: goal,
		progress: progress, board: board, tm: tm,
		realInfos: realInfos, realSet: realSet,
		writeBoardInfos: writeBoardInfos, reasonBoardInfos: reasonBoardInfos,
		systemPromptExtra:   systemPromptExtra,
		factSummaryMaxRunes: appCfg.Project.FactSummaryMaxRunesEffective(),
		toolOutputMaxRunes:  oodaToolOutputMaxRunes(ma),
	}
	return eng.run()
}

// oodaToolOutputMaxRunes 复用 reduction 截断阈值作为 OODA worker 单次工具输出的 rune 预算。
// reduction 阈值是字节口径（默认 12000）；OODA 这里按 rune 口径取同一数值，
// 对中文输出更宽松一点（12000 rune ≈ 多得多的字节），但仍能挡住多 MB 的扫描结果。
func oodaToolOutputMaxRunes(ma *config.MultiAgentConfig) int {
	if ma == nil {
		return 12000
	}
	v := ma.EinoMiddleware.ReductionMaxLengthForTruncEffective()
	if v <= 0 {
		return 12000
	}
	return v
}

// oodaEngine 持有一轮黑板编排的全部运行态。
type oodaEngine struct {
	ctx            context.Context
	appCfg         *config.Config
	ag             *agent.Agent
	db             *database.DB
	logger         *zap.Logger
	conversationID string
	projectID      string
	goal           string
	progress       func(eventType, message string, data interface{})
	board          *blackboard.Board
	tm             *tierModels

	realInfos        []*schema.ToolInfo
	realSet          map[string]struct{}
	writeBoardInfos  []*schema.ToolInfo // record_fact + declare_intent + mark_goal_complete
	reasonBoardInfos []*schema.ToolInfo // declare_intent + mark_goal_complete

	systemPromptExtra string
	execIDs           []string

	// 上下文纪律预算（见 oodaWorker 字段说明）。
	factSummaryMaxRunes int
	toolOutputMaxRunes  int
}
