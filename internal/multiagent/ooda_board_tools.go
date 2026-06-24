package multiagent

import (
	"encoding/json"
	"fmt"
	"strings"

	"cyberstrike-ai/internal/agent"
	"cyberstrike-ai/internal/blackboard"
	"cyberstrike-ai/internal/database"
)

// 黑板合成工具名（非 MCP，由 OODA worker 直接拦截并写入 Board）。
const (
	boardToolRecordFact   = "record_fact"
	boardToolDeclareIntent = "declare_intent"
	boardToolGoalComplete = "mark_goal_complete"
)

// boardToolNames 用于在 worker 循环中快速判断某次工具调用是否为合成黑板工具。
var boardToolNames = map[string]struct{}{
	boardToolRecordFact:    {},
	boardToolDeclareIntent: {},
	boardToolGoalComplete:  {},
}

func isBoardTool(name string) bool {
	_, ok := boardToolNames[strings.TrimSpace(name)]
	return ok
}

// recordFactTool 把一条确认的发现写入黑板。
func recordFactTool() agent.Tool {
	return agent.Tool{
		Type: "function",
		Function: agent.FunctionDefinition{
			Name:        boardToolRecordFact,
			Description: "把一条已确认/已观察到的发现作为事实写入黑板（Fact）。其他 worker 只能通过黑板看到你的发现，因此关键结论必须在此登记。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"fact_key": map[string]interface{}{
						"type":        "string",
						"description": "事实的项目内唯一标识，小写字母/数字及 . _ / -，如 host.web1 / port.web1.443 / vuln.sqli.login",
					},
					"category": map[string]interface{}{
						"type":        "string",
						"description": "事实分类，如 host / port / service / cred / vuln / note",
					},
					"summary": map[string]interface{}{
						"type":        "string",
						"description": "一句话摘要（其他 worker 读板时看到的内容）",
					},
					"body": map[string]interface{}{
						"type":        "string",
						"description": "详细内容（证据、原始输出片段、利用链等），可选",
					},
					"confidence": map[string]interface{}{
						"type":        "string",
						"description": "confirmed（已验证）| tentative（待验证），默认 tentative",
					},
				},
				"required": []string{"fact_key", "summary"},
			},
		},
	}
}

// declareIntentTool 声明一个尚未执行的探索方向。
func declareIntentTool() agent.Tool {
	return agent.Tool{
		Type: "function",
		Function: agent.FunctionDefinition{
			Name:        boardToolDeclareIntent,
			Description: "声明一个尚未执行的探索方向（Intent），写入黑板供 Explore worker 认领执行。把大目标拆成可独立执行的小步骤。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"title": map[string]interface{}{
						"type":        "string",
						"description": "意图标题（要做什么，一句话）",
					},
					"body": map[string]interface{}{
						"type":        "string",
						"description": "执行细节、建议手段、关注点（可选）",
					},
					"parent_fact_key": map[string]interface{}{
						"type":        "string",
						"description": "本意图由哪条事实派生（可选，用于追溯探索图）",
					},
					"priority": map[string]interface{}{
						"type":        "integer",
						"description": "优先级，越大越先被认领，默认 0",
					},
				},
				"required": []string{"title"},
			},
		},
	}
}

// goalCompleteTool 声明目标已达成，结束 OODA 循环。
func goalCompleteTool() agent.Tool {
	return agent.Tool{
		Type: "function",
		Function: agent.FunctionDefinition{
			Name:        boardToolGoalComplete,
			Description: "当且仅当用户的总目标已经达成时调用，给出最终结论。调用后 OODA 循环结束。",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"summary": map[string]interface{}{
						"type":        "string",
						"description": "面向用户的最终结论 / 答复全文",
					},
				},
				"required": []string{"summary"},
			},
		},
	}
}

// boardToolOutcome 是合成工具执行后的副作用信号，回传给 worker 循环。
type boardToolOutcome struct {
	GoalComplete   bool
	GoalSummary    string
	ResultFactKey  string // record_fact 产出的 key（Explore 用于关联 Intent）
	ResultSummary  string
}

// applyBoardTool 执行一次合成黑板工具调用，直接写入 Board，返回给模型的观察文本 + 副作用。
// maxSummaryRunes > 0 时，record_fact 的 summary 超长会被按 rune 截断（与 MCP upsert_project_fact 路径一致，
// 避免单条事实摘要把每轮重发的黑板撑大）。
func applyBoardTool(b *blackboard.Board, name, argsJSON string, maxSummaryRunes int) (observation string, outcome boardToolOutcome, err error) {
	var args map[string]interface{}
	if strings.TrimSpace(argsJSON) != "" && argsJSON != "null" {
		if e := json.Unmarshal([]byte(argsJSON), &args); e != nil {
			return "工具参数 JSON 解析失败，请重试并确保是合法 JSON 对象。", boardToolOutcome{}, nil
		}
	}
	if args == nil {
		args = map[string]interface{}{}
	}
	getStr := func(k string) string { return strings.TrimSpace(asString(args[k])) }

	switch name {
	case boardToolRecordFact:
		summary := getStr("summary")
		if maxSummaryRunes > 0 && len([]rune(summary)) > maxSummaryRunes {
			summary = string([]rune(summary)[:maxSummaryRunes]) + "…"
		}
		f := &database.ProjectFact{
			FactKey:    getStr("fact_key"),
			Category:   getStr("category"),
			Summary:    summary,
			Body:       getStr("body"),
			Confidence: getStr("confidence"),
		}
		if f.Category == "" {
			f.Category = "note"
		}
		saved, e := b.AddFact(f)
		if e != nil {
			return "写入事实失败：" + e.Error(), boardToolOutcome{}, nil
		}
		return "已登记事实 " + saved.FactKey, boardToolOutcome{ResultFactKey: saved.FactKey, ResultSummary: saved.Summary}, nil

	case boardToolDeclareIntent:
		prio := asInt(args["priority"])
		in, e := b.AddIntent(getStr("title"), getStr("body"), getStr("parent_fact_key"), prio)
		if e != nil {
			return "声明意图失败：" + e.Error(), boardToolOutcome{}, nil
		}
		return "已声明意图：" + in.Title, boardToolOutcome{}, nil

	case boardToolGoalComplete:
		return "目标已标记完成。", boardToolOutcome{GoalComplete: true, GoalSummary: getStr("summary")}, nil
	}
	return "", boardToolOutcome{}, fmt.Errorf("未知黑板工具: %s", name)
}

func asString(v interface{}) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", s)
	}
}

func asInt(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}
