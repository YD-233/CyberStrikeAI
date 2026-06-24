package multiagent

import (
	"fmt"
	"strings"

	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
)

// oodaRoundCap OODA 外层循环（Reason→Explore）的最大轮次，防止发散不收敛。
const oodaRoundCap = 8

// exploresPerRound 每轮 Reason 后最多认领并执行的意图数（顺序执行，避免 SSE 回调竞态）。
const exploresPerRound = 4

// run 执行完整 OODA 编排，返回与单 Agent 循环对齐的 RunResult。
func (e *oodaEngine) run() (*RunResult, error) {
	// ---- Bootstrap：尝试直解，否则拆解为 Fact/Intent ----
	e.emitPhase("bootstrap", 0, "引导：尝试直接解决或拆解目标")
	bootW := e.newWorker(e.writeBoardInfos, true, 14)
	bootRes, err := bootW.run(e.tm.cfgForTier("high"),
		oodaBootstrapSystemPrompt(e.goal, e.systemPromptExtra),
		"# 用户总目标\n"+e.goal)
	if err != nil {
		return nil, fmt.Errorf("bootstrap 失败: %w", err)
	}
	e.collectExecIDs(bootRes.ExecutionIDs)
	if bootRes.GoalComplete {
		return e.finish(bootRes.GoalSummary), nil
	}

	// ---- 主循环：Reason（high）→ Explore（low）×N ----
	for round := 1; round <= oodaRoundCap; round++ {
		snap, serr := e.board.Snapshot()
		if serr != nil {
			return nil, fmt.Errorf("读取黑板失败: %w", serr)
		}

		// Reason：读全图，判完成 / 生成下一批意图。
		e.emitPhase("reason", round, fmt.Sprintf("推理：第 %d 轮，评估全局并规划", round))
		reasonW := e.newWorker(e.reasonBoardInfos, false, 6)
		reasonRes, rerr := reasonW.run(e.tm.cfgForTier("high"),
			oodaReasonSystemPrompt(), renderGoalAndBoard(e.goal, snap))
		if rerr != nil {
			return nil, fmt.Errorf("reason（第 %d 轮）失败: %w", round, rerr)
		}
		e.collectExecIDs(reasonRes.ExecutionIDs)
		if reasonRes.GoalComplete {
			return e.finish(reasonRes.GoalSummary), nil
		}

		// Explore：逐个认领 open 意图并执行（low 档）。
		executed := 0
		for executed < exploresPerRound {
			intent, cerr := e.board.ClaimIntent(fmt.Sprintf("explore-r%d-%d", round, executed+1))
			if cerr != nil {
				return nil, fmt.Errorf("认领意图失败: %w", cerr)
			}
			if intent == nil {
				break // 无可认领意图
			}
			e.emitPhase("explore", round, "探索："+intent.Title)
			esnap, _ := e.board.Snapshot()
			expW := e.newWorker(e.writeBoardInfos, true, 12)
			expRes, eerr := expW.run(e.tm.cfgForTier("low"),
				oodaExploreSystemPrompt(),
				renderExploreTask(e.goal, intent.Title, intent.Body, esnap))
			if eerr != nil {
				// 探索失败：放弃该意图，继续其他意图（不终止整轮）。
				_ = e.board.DropIntent(intent.ID, "执行失败: "+eerr.Error())
				if e.logger != nil {
					e.logger.Warn("explore 执行失败，放弃该意图", zap.String("intent", intent.Title), zap.Error(eerr))
				}
				executed++
				continue
			}
			e.collectExecIDs(expRes.ExecutionIDs)
			// 关联意图与其产出事实（若 worker 写了 fact）。
			if expRes.LastFactKey != "" {
				_ = e.board.CompleteIntent(intent.ID, expRes.LastFactKey, expRes.LastFactSummary)
			} else {
				_ = e.board.CompleteIntent(intent.ID, "", strings.TrimSpace(expRes.FinalText))
			}
			if expRes.GoalComplete {
				return e.finish(expRes.GoalSummary), nil
			}
			executed++
		}

		if executed == 0 {
			// 本轮无任何可认领意图 → 收敛。
			break
		}
	}

	// ---- 收敛：目标未被显式标记完成，让 high 档基于全图给最终结论 ----
	return e.synthesizeFinal()
}

// synthesizeFinal 在循环收敛后，用 high 档读全图综合出面向用户的最终回复。
func (e *oodaEngine) synthesizeFinal() (*RunResult, error) {
	snap, err := e.board.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("读取黑板失败: %w", err)
	}
	e.emitPhase("synthesize", oodaRoundCap, "综合：基于黑板形成最终结论")
	// 用 Reason 提示但要求必须产出结论（无工具，直接出文本）。
	finalW := e.newWorker(nil, false, 2)
	sys := "你是安全测试黑板系统的总结 worker。基于下面黑板上的全部事实，针对用户总目标给出清晰、面向用户的最终结论与建议。直接输出结论正文，不要调用任何工具。"
	res, gerr := finalW.run(e.tm.cfgForTier("high"), sys, renderGoalAndBoard(e.goal, snap))
	if gerr != nil || res == nil || strings.TrimSpace(res.FinalText) == "" {
		// 兜底：用黑板快照拼装。
		return e.finish(quiescenceMessage(snap)), nil
	}
	e.collectExecIDs(res.ExecutionIDs)
	return e.finish(res.FinalText), nil
}

// newWorker 构造一个 OODA worker。boardInfos 决定它能用哪些合成黑板工具；
// withRealTools 决定是否绑定真实 MCP 工具（Reason/总结为 false）。
func (e *oodaEngine) newWorker(boardInfos []*schema.ToolInfo, withRealTools bool, maxSteps int) *oodaWorker {
	w := &oodaWorker{
		ctx:            e.ctx,
		ag:             e.ag,
		board:          e.board,
		conversationID: e.conversationID,
		progress:       e.progress,
		boardToolInfos: boardInfos,
		maxSteps:       maxSteps,

		factSummaryMaxRunes: e.factSummaryMaxRunes,
		toolOutputMaxRunes:  e.toolOutputMaxRunes,
	}
	if withRealTools {
		w.realToolInfos = e.realInfos
		w.realToolSet = e.realSet
	} else {
		w.realToolSet = map[string]struct{}{}
	}
	return w
}

func (e *oodaEngine) emitPhase(phase string, round int, message string) {
	e.progress("ooda_phase", message, map[string]interface{}{
		"phase":          phase,
		"round":          round,
		"conversationId": e.conversationID,
		"projectId":      e.projectID,
	})
}

func (e *oodaEngine) collectExecIDs(ids []string) {
	for _, id := range ids {
		if strings.TrimSpace(id) != "" {
			e.execIDs = append(e.execIDs, id)
		}
	}
}

// finish 组装 RunResult。response 为最终对外文本；同时把黑板快照序列化进 trace 便于续跑/审计。
func (e *oodaEngine) finish(response string) *RunResult {
	response = strings.TrimSpace(response)
	if response == "" {
		response = "本轮黑板编排未产生明确结论。"
	}
	snap, _ := e.board.Snapshot()
	traceOut := response
	var traceIn string
	if snap != nil {
		traceIn = snap.RenderForModel()
	}
	return &RunResult{
		Response:             response,
		MCPExecutionIDs:      e.execIDs,
		LastAgentTraceInput:  traceIn,
		LastAgentTraceOutput: traceOut,
	}
}
