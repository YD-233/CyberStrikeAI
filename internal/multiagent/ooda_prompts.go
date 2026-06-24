package multiagent

import (
	"encoding/json"
	"fmt"
	"strings"

	"cyberstrike-ai/internal/blackboard"
)

// parseToolArgs 宽松解析工具调用参数 JSON 为 map（失败返回空 map，让模型可自我纠正）。
func parseToolArgs(argsJSON string) map[string]interface{} {
	var args map[string]interface{}
	s := strings.TrimSpace(argsJSON)
	if s != "" && s != "null" {
		_ = json.Unmarshal([]byte(s), &args)
	}
	if args == nil {
		args = map[string]interface{}{}
	}
	return args
}

// oodaBootstrapSystemPrompt Bootstrap 阶段系统提示：尝试直接解决，否则把问题拆成事实与意图。
func oodaBootstrapSystemPrompt(goal, extra string) string {
	var b strings.Builder
	b.WriteString("你是一个安全测试黑板系统（Cairn 风格）的引导 worker（Bootstrap）。\n")
	b.WriteString("你的工作方式是协调而非独占：发现写成事实(Fact)、探索方向写成意图(Intent)，都登记到共享黑板，其他 worker 通过黑板看到你的产出。\n\n")
	b.WriteString("本阶段任务：针对用户的总目标，先快速判断能否一步直接达成。\n")
	b.WriteString("- 如果用很少的工具调用就能直接得到最终结论 → 调用 mark_goal_complete 给出答复。\n")
	b.WriteString("- 否则：用 record_fact 登记你已知/已探明的关键事实；用 declare_intent 把后续要做的探索拆成多个可独立执行的意图。\n")
	b.WriteString("- 至少声明 1 个 declare_intent，除非目标已完成。意图要具体、可执行、彼此独立。\n")
	if strings.TrimSpace(extra) != "" {
		b.WriteString("\n# 项目背景与既有黑板\n")
		b.WriteString(strings.TrimSpace(extra))
		b.WriteString("\n")
	}
	return b.String()
}

// oodaReasonSystemPrompt Reason 阶段系统提示：读全图、判断完成、生成下一批意图。
func oodaReasonSystemPrompt() string {
	var b strings.Builder
	b.WriteString("你是安全测试黑板系统的推理 worker（Reason）。你能看到黑板全图（事实 / 待探索意图 / 执行中意图 / 已完成意图 / 人类提示）。\n\n")
	b.WriteString("基于全图判断：\n")
	b.WriteString("1) 用户总目标是否已达成？若已达成 → 调用 mark_goal_complete 给出面向用户的最终结论（综合所有事实）。\n")
	b.WriteString("2) 若未达成：根据已有事实推进，用 declare_intent 声明下一批最有价值的探索方向（基于新事实的纵深推进 / 横向扩展）。避免与已有意图重复。\n")
	b.WriteString("3) 必须采纳『人类提示 Hints』——它们是高优先级判断。\n")
	b.WriteString("只调用 declare_intent 或 mark_goal_complete；不要直接执行工具。若暂时无新方向且目标未达成，可不声明（循环将在无待探索意图时收敛）。\n")
	return b.String()
}

// oodaExploreSystemPrompt Explore 阶段系统提示：认领单个意图、用真实工具执行、写回一条事实。
func oodaExploreSystemPrompt() string {
	var b strings.Builder
	b.WriteString("你是安全测试黑板系统的探索 worker（Explore）。你被分配了一个具体意图(Intent)，请执行它。\n\n")
	b.WriteString("工作流程：\n")
	b.WriteString("- 使用可用的真实工具（如端口扫描、漏洞利用、信息收集等）执行该意图。\n")
	b.WriteString("- 执行完成后，用 record_fact 把你的发现写回黑板（这是其他 worker 看到你成果的唯一途径）。\n")
	b.WriteString("- 只聚焦当前这一个意图，不要发散到其他方向（其他方向应由 Reason 统一规划）。\n")
	b.WriteString("- 完成后用一段话总结你做了什么、得到什么结论。\n")
	return b.String()
}

// renderGoalAndBoard 组合「用户目标 + 当前黑板快照」作为 worker 的 user 输入。
func renderGoalAndBoard(goal string, snap *blackboard.Snapshot) string {
	var b strings.Builder
	b.WriteString("# 用户总目标\n")
	b.WriteString(strings.TrimSpace(goal))
	b.WriteString("\n\n")
	if snap != nil {
		b.WriteString(snap.RenderForModel())
	}
	return b.String()
}

// renderExploreTask 组合 Explore worker 的输入：目标 + 当前意图 + 黑板快照（供其参考已知事实）。
func renderExploreTask(goal, intentTitle, intentBody string, snap *blackboard.Snapshot) string {
	var b strings.Builder
	b.WriteString("# 用户总目标\n")
	b.WriteString(strings.TrimSpace(goal))
	b.WriteString("\n\n# 你被分配的意图（请执行它）\n")
	b.WriteString("标题: " + strings.TrimSpace(intentTitle) + "\n")
	if strings.TrimSpace(intentBody) != "" {
		b.WriteString("细节: " + strings.TrimSpace(intentBody) + "\n")
	}
	b.WriteString("\n")
	if snap != nil {
		b.WriteString("# 当前黑板（供参考，避免重复已知事实）\n")
		b.WriteString(snap.RenderForModel())
	}
	return b.String()
}

// boardChangeToProgress 把黑板变更投射为 SSE 进度事件类型与数据。
// 数据字段尽量自洽，便于前端黑板面板直接增量渲染，无需额外回查。
func boardChangeToProgress(c blackboard.Change) (eventType string, data map[string]interface{}) {
	data = map[string]interface{}{"summary": c.Summary}
	switch c.Kind {
	case blackboard.ChangeFactAdded:
		if c.Fact != nil {
			data["factKey"] = c.Fact.FactKey
			data["category"] = c.Fact.Category
			data["confidence"] = c.Fact.Confidence
			data["factSummary"] = c.Fact.Summary
			data["pinned"] = c.Fact.Pinned
		}
	case blackboard.ChangeIntentAdded, blackboard.ChangeIntentClaimed, blackboard.ChangeIntentDone, blackboard.ChangeIntentDropped:
		if c.Intent != nil {
			data["intentId"] = c.Intent.ID
			data["title"] = c.Intent.Title
			data["body"] = c.Intent.Body
			data["priority"] = c.Intent.Priority
			data["claimedBy"] = c.Intent.ClaimedBy
			if c.Intent.ResultFactKey != "" {
				data["resultFactKey"] = c.Intent.ResultFactKey
			}
			if c.Intent.ResultSummary != "" {
				data["resultSummary"] = c.Intent.ResultSummary
			}
		}
	case blackboard.ChangeHintAdded:
		if c.Hint != nil {
			data["hintId"] = c.Hint.ID
			data["content"] = c.Hint.Content
		}
	}
	return string(c.Kind), data
}

// quiescenceMessage 收敛但目标未显式完成时，基于黑板拼一个兜底回复。
func quiescenceMessage(snap *blackboard.Snapshot) string {
	if snap == nil {
		return "本轮探索已收敛，但未能确定目标已达成。"
	}
	f, _, _, _, _ := snap.Counts()
	return fmt.Sprintf("探索已收敛：黑板上已积累 %d 条事实，但没有新的探索方向，且目标未被显式标记完成。以下是已确认的发现：\n\n%s",
		f, snap.RenderForModel())
}
