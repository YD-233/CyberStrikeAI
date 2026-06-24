package multiagent

import (
	"context"
	"fmt"
	"strings"

	"cyberstrike-ai/internal/agent"
	"cyberstrike-ai/internal/blackboard"
	"cyberstrike-ai/internal/einomcp"

	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

// oodaWorker 封装一次 OODA 任务（Bootstrap / Reason / Explore）的内层 ReAct 循环：
// 驱动某一档模型，绑定「真实 MCP 工具 + 合成黑板工具」，反复 Generate→执行工具→观察，
// 直到模型不再调用工具，或调用了 mark_goal_complete，或触达迭代上限。
type oodaWorker struct {
	ctx            context.Context
	ag             *agent.Agent
	board          *blackboard.Board
	conversationID string
	progress       func(eventType, message string, data interface{})

	// 工具：realTools 为该 worker 可用的真实 MCP 工具定义；boardTools 为合成黑板工具。
	realToolInfos  []*schema.ToolInfo
	boardToolInfos []*schema.ToolInfo
	realToolSet    map[string]struct{}

	maxSteps int

	// 上下文纪律：
	// factSummaryMaxRunes —— record_fact 摘要写板前的 rune 上限（0 表示不限）。
	// toolOutputMaxRunes  —— 单次真实工具输出回灌进 msgs 前的 rune 上限（0 表示不限）；
	//   防止多 MB 扫描结果 verbatim 进上下文、并在后续每步 ReAct 反复重发。
	factSummaryMaxRunes int
	toolOutputMaxRunes  int
}

// oodaWorkerResult 内层循环产出。
type oodaWorkerResult struct {
	FinalText     string
	GoalComplete  bool
	GoalSummary   string
	LastFactKey   string // 最后一条 record_fact 的 key（Explore 用于关联 Intent）
	LastFactSummary string
	ExecutionIDs  []string
}

// buildToolInfos 把一组 agent.Tool 定义转成 *schema.ToolInfo（复用 einomcp 的单一转换实现）。
func buildToolInfos(defs []agent.Tool) ([]*schema.ToolInfo, map[string]struct{}, error) {
	infos := make([]*schema.ToolInfo, 0, len(defs))
	set := make(map[string]struct{}, len(defs))
	for _, d := range defs {
		if d.Type != "function" || strings.TrimSpace(d.Function.Name) == "" {
			continue
		}
		info, err := einomcp.ToolInfoFromDefinition(d)
		if err != nil {
			return nil, nil, fmt.Errorf("工具 %q 转换失败: %w", d.Function.Name, err)
		}
		infos = append(infos, info)
		set[d.Function.Name] = struct{}{}
	}
	return infos, set, nil
}

// run 驱动模型完成一次任务。modelCfg 决定档位（high/low）。
// systemPrompt + userPrompt 组成初始消息；boardTools 决定本 worker 能否写板/声明意图/完成目标。
func (w *oodaWorker) run(modelCfg *einoopenai.ChatModelConfig, systemPrompt, userPrompt string) (*oodaWorkerResult, error) {
	cm, err := einoopenai.NewChatModel(w.ctx, modelCfg)
	if err != nil {
		return nil, fmt.Errorf("ooda worker 模型构建失败: %w", err)
	}
	allInfos := append(append([]*schema.ToolInfo{}, w.realToolInfos...), w.boardToolInfos...)
	var tcm model.ToolCallingChatModel = cm
	if len(allInfos) > 0 {
		bound, berr := cm.WithTools(allInfos)
		if berr != nil {
			return nil, fmt.Errorf("ooda worker 绑定工具失败: %w", berr)
		}
		tcm = bound
	}

	msgs := []*schema.Message{}
	if strings.TrimSpace(systemPrompt) != "" {
		msgs = append(msgs, schema.SystemMessage(systemPrompt))
	}
	msgs = append(msgs, schema.UserMessage(userPrompt))

	res := &oodaWorkerResult{}
	steps := w.maxSteps
	if steps <= 0 {
		steps = 12
	}

	for i := 0; i < steps; i++ {
		out, gerr := tcm.Generate(w.ctx, msgs)
		if gerr != nil {
			return nil, fmt.Errorf("ooda worker 生成失败: %w", gerr)
		}
		if out == nil {
			break
		}
		msgs = append(msgs, out)

		if len(out.ToolCalls) == 0 {
			// 无工具调用：把这段文本视为该 worker 的最终输出。
			res.FinalText = strings.TrimSpace(out.Content)
			return res, nil
		}

		// 逐个执行工具调用，把结果作为 tool 消息回灌。
		for _, tc := range out.ToolCalls {
			name := strings.TrimSpace(tc.Function.Name)
			argsJSON := tc.Function.Arguments

			if isBoardTool(name) {
				obs, outcome, aerr := applyBoardTool(w.board, name, argsJSON, w.factSummaryMaxRunes)
				if aerr != nil {
					obs = "工具错误：" + aerr.Error()
				}
				if outcome.ResultFactKey != "" {
					res.LastFactKey = outcome.ResultFactKey
					res.LastFactSummary = outcome.ResultSummary
				}
				if outcome.GoalComplete {
					res.GoalComplete = true
					res.GoalSummary = outcome.GoalSummary
					res.FinalText = outcome.GoalSummary
					return res, nil
				}
				msgs = append(msgs, schema.ToolMessage(obs, tc.ID))
				continue
			}

			// 真实 MCP 工具：走框架中立的 Agent 执行路径。
			obs, execID := w.execRealTool(name, argsJSON)
			if execID != "" {
				res.ExecutionIDs = append(res.ExecutionIDs, execID)
			}
			msgs = append(msgs, schema.ToolMessage(obs, tc.ID))
		}
	}

	// 达到步数上限：用最后一条助手内容兜底。
	if res.FinalText == "" && len(msgs) > 0 {
		last := msgs[len(msgs)-1]
		res.FinalText = strings.TrimSpace(last.Content)
	}
	return res, nil
}

// execRealTool 执行一次真实 MCP 工具，返回给模型的观察文本与 executionID。
func (w *oodaWorker) execRealTool(name, argsJSON string) (string, string) {
	if _, ok := w.realToolSet[name]; !ok {
		return "工具不可用或未授权：" + name, ""
	}
	args := parseToolArgs(argsJSON)
	if w.progress != nil {
		w.progress("tool_call", "执行工具 "+name, map[string]interface{}{
			"toolName":       name,
			"conversationId": w.conversationID,
		})
	}
	res, err := w.ag.ExecuteMCPToolForConversation(w.ctx, w.conversationID, name, args)
	if err != nil {
		return "工具执行错误：" + err.Error(), ""
	}
	if res == nil {
		return "（无输出）", ""
	}
	body := res.Result
	if strings.TrimSpace(body) == "" {
		body = "（无输出）"
	}
	if w.progress != nil {
		w.progress("tool_result", "工具 "+name+" 完成", map[string]interface{}{
			"toolName":       name,
			"isError":        res.IsError,
			"executionId":    res.ExecutionID,
			"conversationId": w.conversationID,
		})
	}
	if res.IsError {
		return "工具返回错误：" + w.capToolOutput(body), res.ExecutionID
	}
	return w.capToolOutput(body), res.ExecutionID
}

// capToolOutput 把单次真实工具输出按 rune 预算截断（保留头尾，中间折叠），
// 防止多 MB 扫描结果 verbatim 进 worker 上下文并在后续每步 ReAct 反复重发。
// toolOutputMaxRunes <= 0 时不截断。
func (w *oodaWorker) capToolOutput(body string) string {
	if w.toolOutputMaxRunes <= 0 {
		return body
	}
	rs := []rune(body)
	if len(rs) <= w.toolOutputMaxRunes {
		return body
	}
	const notice = "\n…[工具输出已截断，超出部分未进入上下文 / tool output truncated]…\n"
	keep := w.toolOutputMaxRunes - len([]rune(notice))
	if keep < 200 {
		keep = w.toolOutputMaxRunes * 2 / 3
	}
	head := keep * 70 / 100
	tail := keep - head
	return string(rs[:head]) + notice + string(rs[len(rs)-tail:])
}
