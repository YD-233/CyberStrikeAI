package blackboard

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"cyberstrike-ai/internal/database"
)

// 渲染上限：防止黑板随轮次膨胀后，每轮 Reason/Explore 把整块板重发导致上下文爆掉。
// 这些上限给出一个可预测的总量边界：facts ≤ renderMaxFacts 行、各类 intent ≤ renderMaxIntentsPerGroup 行，
// 每行 ≤ oneLineMaxRunes 个 rune。超出部分折叠为「还有 N 条未显示」。
const (
	renderMaxFacts           = 80
	renderMaxIntentsPerGroup = 40
	oneLineMaxRunes          = 240
)

// RenderForModel 把黑板快照渲染成模型可读的文本（Observe → Orient 的输入）。
// 结构清晰、稳定排序，便于模型在 Reason/Explore 时定位。
func (s *Snapshot) RenderForModel() string {
	var b strings.Builder

	b.WriteString("# 黑板状态（Blackboard）\n\n")

	// Facts —— pinned / confirmed 优先，超过上限折叠剩余条数。
	b.WriteString(fmt.Sprintf("## 已确认事实 Facts（%d）\n", len(s.Facts)))
	if len(s.Facts) == 0 {
		b.WriteString("（暂无事实）\n")
	} else {
		facts := append([]*database.ProjectFact(nil), s.Facts...)
		sort.SliceStable(facts, func(i, j int) bool {
			// pinned 置顶，其次 confirmed，再按 category/key 稳定排序。
			if facts[i].Pinned != facts[j].Pinned {
				return facts[i].Pinned
			}
			if ci, cj := factConfRank(facts[i].Confidence), factConfRank(facts[j].Confidence); ci != cj {
				return ci < cj
			}
			if facts[i].Category != facts[j].Category {
				return facts[i].Category < facts[j].Category
			}
			return facts[i].FactKey < facts[j].FactKey
		})
		shown := facts
		if len(shown) > renderMaxFacts {
			shown = shown[:renderMaxFacts]
		}
		for _, f := range shown {
			conf := f.Confidence
			if conf == "" {
				conf = "tentative"
			}
			pin := ""
			if f.Pinned {
				pin = "📌"
			}
			b.WriteString(fmt.Sprintf("- %s[%s/%s] `%s`: %s\n", pin, f.Category, conf, f.FactKey, oneLine(f.Summary)))
		}
		if len(facts) > renderMaxFacts {
			b.WriteString(fmt.Sprintf("- …（还有 %d 条事实未显示，必要时用 record_fact 的 fact_key 精确引用）\n", len(facts)-renderMaxFacts))
		}
	}
	b.WriteString("\n")

	// Open intents
	b.WriteString(fmt.Sprintf("## 待探索意图 Open Intents（%d）\n", len(s.OpenIntents)))
	if len(s.OpenIntents) == 0 {
		b.WriteString("（暂无待探索意图）\n")
	} else {
		writeCapped(&b, len(s.OpenIntents), func(i int) string {
			in := s.OpenIntents[i]
			return fmt.Sprintf("- (id=%s, prio=%d) %s\n", shortID(in.ID), in.Priority, oneLine(in.Title))
		})
	}
	b.WriteString("\n")

	// Claimed (in-flight)
	if len(s.ClaimedIntents) > 0 {
		b.WriteString(fmt.Sprintf("## 执行中意图 Claimed（%d）\n", len(s.ClaimedIntents)))
		writeCapped(&b, len(s.ClaimedIntents), func(i int) string {
			in := s.ClaimedIntents[i]
			return fmt.Sprintf("- (id=%s, by=%s) %s\n", shortID(in.ID), in.ClaimedBy, oneLine(in.Title))
		})
		b.WriteString("\n")
	}

	// Done
	if len(s.DoneIntents) > 0 {
		b.WriteString(fmt.Sprintf("## 已完成意图 Done（%d）\n", len(s.DoneIntents)))
		writeCapped(&b, len(s.DoneIntents), func(i int) string {
			in := s.DoneIntents[i]
			line := oneLine(in.Title)
			if in.ResultSummary != "" {
				line += " → " + oneLine(in.ResultSummary)
			}
			return fmt.Sprintf("- %s\n", line)
		})
		b.WriteString("\n")
	}

	// Hints (human judgment) —— 人类提示不折叠（数量天然有限且优先级最高）。
	if len(s.Hints) > 0 {
		b.WriteString(fmt.Sprintf("## 人类提示 Hints（%d）— 高优先级，须采纳\n", len(s.Hints)))
		for _, h := range s.Hints {
			b.WriteString(fmt.Sprintf("- %s\n", oneLine(h.Content)))
		}
		b.WriteString("\n")
	}

	return b.String()
}

// writeCapped 按 renderMaxIntentsPerGroup 渲染前 N 行，超出折叠为一行提示。
func writeCapped(b *strings.Builder, total int, line func(i int) string) {
	limit := total
	if limit > renderMaxIntentsPerGroup {
		limit = renderMaxIntentsPerGroup
	}
	for i := 0; i < limit; i++ {
		b.WriteString(line(i))
	}
	if total > limit {
		b.WriteString(fmt.Sprintf("- …（还有 %d 条未显示）\n", total-limit))
	}
}

// factConfRank 让 confirmed 事实在裁剪时优先于 tentative 保留。
func factConfRank(conf string) int {
	switch strings.ToLower(strings.TrimSpace(conf)) {
	case "confirmed":
		return 0
	default:
		return 1
	}
}

// Counts 返回各原语计数，用于日志/进度。
func (s *Snapshot) Counts() (facts, open, claimed, done, hints int) {
	return len(s.Facts), len(s.OpenIntents), len(s.ClaimedIntents), len(s.DoneIntents), len(s.Hints)
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	// 按 rune 截断，避免把中文等多字节字符切在半个字符上。
	if utf8.RuneCountInString(s) > oneLineMaxRunes {
		return string([]rune(s)[:oneLineMaxRunes]) + "…"
	}
	return s
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
