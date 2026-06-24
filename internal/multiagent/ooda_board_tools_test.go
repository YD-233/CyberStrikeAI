package multiagent

import (
	"strings"
	"testing"

	"cyberstrike-ai/internal/agent"
	"cyberstrike-ai/internal/blackboard"
	"cyberstrike-ai/internal/database"
)

// oodaFakeStore 是 blackboard.Persister 的内存替身，用于在不触碰真实 DB 的情况下
// 测试 OODA 合成黑板工具（record_fact / declare_intent / mark_goal_complete）。
type oodaFakeStore struct {
	facts   map[string]*database.ProjectFact
	intents []*database.ProjectIntent
	hints   []*database.ProjectHint
	seq     int
}

func newOODAFakeStore() *oodaFakeStore {
	return &oodaFakeStore{facts: map[string]*database.ProjectFact{}}
}

func (s *oodaFakeStore) UpsertProjectFact(f *database.ProjectFact) (*database.ProjectFact, error) {
	if f.ID == "" {
		s.seq++
		f.ID = "f" + string(rune('0'+s.seq))
	}
	cp := *f
	s.facts[f.FactKey] = &cp
	return &cp, nil
}

func (s *oodaFakeStore) ListProjectFactsForIndex(projectID string, includeDeprecated bool) ([]*database.ProjectFact, error) {
	var out []*database.ProjectFact
	for _, f := range s.facts {
		out = append(out, f)
	}
	return out, nil
}

func (s *oodaFakeStore) CreateProjectIntent(in *database.ProjectIntent) (*database.ProjectIntent, error) {
	s.seq++
	in.ID = "i" + string(rune('0'+s.seq))
	if in.Status == "" {
		in.Status = database.IntentStatusOpen
	}
	cp := *in
	s.intents = append(s.intents, &cp)
	return &cp, nil
}

func (s *oodaFakeStore) ListProjectIntents(projectID, status string) ([]*database.ProjectIntent, error) {
	var out []*database.ProjectIntent
	for _, in := range s.intents {
		if status == "" || in.Status == status {
			out = append(out, in)
		}
	}
	return out, nil
}

func (s *oodaFakeStore) ClaimNextOpenIntent(projectID, claimedBy string) (*database.ProjectIntent, error) {
	for _, in := range s.intents {
		if in.Status == database.IntentStatusOpen {
			in.Status = database.IntentStatusClaimed
			in.ClaimedBy = claimedBy
			cp := *in
			return &cp, nil
		}
	}
	return nil, nil
}

func (s *oodaFakeStore) CompleteIntent(id, resultFactKey, resultSummary string) error {
	for _, in := range s.intents {
		if in.ID == id {
			in.Status = database.IntentStatusDone
			in.ResultFactKey = resultFactKey
			in.ResultSummary = resultSummary
		}
	}
	return nil
}

func (s *oodaFakeStore) UpdateIntentStatus(id, status string) error {
	for _, in := range s.intents {
		if in.ID == id {
			in.Status = status
		}
	}
	return nil
}

func (s *oodaFakeStore) CreateProjectHint(h *database.ProjectHint) (*database.ProjectHint, error) {
	s.seq++
	h.ID = "h" + string(rune('0'+s.seq))
	if h.Status == "" {
		h.Status = database.HintStatusActive
	}
	cp := *h
	s.hints = append(s.hints, &cp)
	return &cp, nil
}

func (s *oodaFakeStore) ListProjectHints(projectID, status string) ([]*database.ProjectHint, error) {
	var out []*database.ProjectHint
	for _, h := range s.hints {
		if h.Status == status {
			out = append(out, h)
		}
	}
	return out, nil
}

func (s *oodaFakeStore) ArchiveProjectHint(id string) error { return nil }

func TestIsBoardToolAndParseArgs(t *testing.T) {
	for _, name := range []string{boardToolRecordFact, boardToolDeclareIntent, boardToolGoalComplete} {
		if !isBoardTool(name) {
			t.Errorf("isBoardTool(%q) = false, want true", name)
		}
	}
	if isBoardTool("nmap_scan") {
		t.Error("isBoardTool(nmap_scan) = true, want false")
	}

	args := parseToolArgs(`{"a":"x","n":3}`)
	if asString(args["a"]) != "x" || asInt(args["n"]) != 3 {
		t.Errorf("parseToolArgs mismatch: %+v", args)
	}
	// 非法 JSON 返回空 map，而非 panic
	if got := parseToolArgs("not json"); got == nil || len(got) != 0 {
		t.Errorf("parseToolArgs(invalid) = %+v, want empty map", got)
	}
}

func TestBuildToolInfosForBoardTools(t *testing.T) {
	infos, set, err := buildToolInfos([]agent.Tool{recordFactTool(), declareIntentTool(), goalCompleteTool()})
	if err != nil {
		t.Fatalf("buildToolInfos: %v", err)
	}
	if len(infos) != 3 {
		t.Fatalf("got %d tool infos, want 3", len(infos))
	}
	for _, want := range []string{boardToolRecordFact, boardToolDeclareIntent, boardToolGoalComplete} {
		if _, ok := set[want]; !ok {
			t.Errorf("tool set missing %q", want)
		}
	}
}

func TestApplyBoardToolRecordFact(t *testing.T) {
	b := blackboard.New(newOODAFakeStore(), "proj1", "conv1")
	obs, outcome, err := applyBoardTool(b, boardToolRecordFact,
		`{"fact_key":"host.web1","category":"host","summary":"web server","confidence":"confirmed"}`, 0)
	if err != nil {
		t.Fatalf("applyBoardTool: %v", err)
	}
	if outcome.ResultFactKey != "host.web1" {
		t.Errorf("ResultFactKey = %q, want host.web1", outcome.ResultFactKey)
	}
	if outcome.GoalComplete {
		t.Error("record_fact should not complete goal")
	}
	if obs == "" {
		t.Error("expected non-empty observation")
	}

	snap, _ := b.Snapshot()
	if f, _, _, _, _ := snap.Counts(); f != 1 {
		t.Errorf("board has %d facts, want 1", f)
	}
}

func TestApplyBoardToolRecordFactSummaryCapped(t *testing.T) {
	b := blackboard.New(newOODAFakeStore(), "proj1", "conv1")
	long := strings.Repeat("漏", 500) // 500 个多字节 rune
	args := `{"fact_key":"vuln.long","category":"vuln","summary":"` + long + `"}`
	if _, _, err := applyBoardTool(b, boardToolRecordFact, args, 200); err != nil {
		t.Fatalf("applyBoardTool: %v", err)
	}
	snap, _ := b.Snapshot()
	if len(snap.Facts) != 1 {
		t.Fatalf("want 1 fact, got %d", len(snap.Facts))
	}
	// 200 rune + 省略号；不能按字节超出，也不能切碎多字节字符。
	got := []rune(snap.Facts[0].Summary)
	if len(got) != 201 { // 200 + "…"
		t.Errorf("summary rune len = %d, want 201 (200 capped + ellipsis)", len(got))
	}
	if got[len(got)-1] != '…' {
		t.Errorf("capped summary should end with ellipsis, got %q", string(got[len(got)-1]))
	}
}

func TestApplyBoardToolDeclareIntentAndComplete(t *testing.T) {
	b := blackboard.New(newOODAFakeStore(), "proj1", "conv1")

	if _, _, err := applyBoardTool(b, boardToolDeclareIntent, `{"title":"scan ports","priority":5}`, 0); err != nil {
		t.Fatalf("declare_intent: %v", err)
	}
	snap, _ := b.Snapshot()
	if _, open, _, _, _ := snap.Counts(); open != 1 {
		t.Errorf("open intents = %d, want 1", open)
	}

	_, outcome, err := applyBoardTool(b, boardToolGoalComplete, `{"summary":"all done, target compromised"}`, 0)
	if err != nil {
		t.Fatalf("mark_goal_complete: %v", err)
	}
	if !outcome.GoalComplete || outcome.GoalSummary != "all done, target compromised" {
		t.Errorf("goal outcome = %+v, want complete with summary", outcome)
	}
}

func TestBoardChangeToProgressMapping(t *testing.T) {
	et, data := boardChangeToProgress(blackboard.Change{
		Kind:    blackboard.ChangeFactAdded,
		Summary: "事实",
		Fact:    &database.ProjectFact{FactKey: "k1", Category: "host", Confidence: "confirmed"},
	})
	if et != "fact_added" {
		t.Errorf("event type = %q, want fact_added", et)
	}
	if data["factKey"] != "k1" || data["category"] != "host" {
		t.Errorf("data mapping wrong: %+v", data)
	}
}
