package blackboard

import (
	"testing"

	"cyberstrike-ai/internal/database"
)

// fakeStore 是 Persister 的内存替身，用于单测（不触碰真实 DB）。
type fakeStore struct {
	facts   map[string]*database.ProjectFact   // fact_key -> fact
	intents map[string]*database.ProjectIntent // id -> intent
	hints   map[string]*database.ProjectHint   // id -> hint
	seq     int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		facts:   map[string]*database.ProjectFact{},
		intents: map[string]*database.ProjectIntent{},
		hints:   map[string]*database.ProjectHint{},
	}
}

func (s *fakeStore) nextID(p string) string {
	s.seq++
	return p + "-" + string(rune('a'+s.seq))
}

func (s *fakeStore) UpsertProjectFact(f *database.ProjectFact) (*database.ProjectFact, error) {
	if f.ID == "" {
		f.ID = s.nextID("fact")
	}
	cp := *f
	s.facts[f.FactKey] = &cp
	return &cp, nil
}

func (s *fakeStore) ListProjectFactsForIndex(projectID string, includeDeprecated bool) ([]*database.ProjectFact, error) {
	var out []*database.ProjectFact
	for _, f := range s.facts {
		if f.ProjectID != projectID {
			continue
		}
		if !includeDeprecated && f.Confidence == "deprecated" {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

func (s *fakeStore) CreateProjectIntent(in *database.ProjectIntent) (*database.ProjectIntent, error) {
	if in.ID == "" {
		in.ID = s.nextID("intent")
	}
	if in.Status == "" {
		in.Status = database.IntentStatusOpen
	}
	cp := *in
	s.intents[in.ID] = &cp
	return &cp, nil
}

func (s *fakeStore) ListProjectIntents(projectID, status string) ([]*database.ProjectIntent, error) {
	var out []*database.ProjectIntent
	for _, in := range s.intents {
		if in.ProjectID != projectID {
			continue
		}
		if status != "" && in.Status != status {
			continue
		}
		out = append(out, in)
	}
	return out, nil
}

func (s *fakeStore) ClaimNextOpenIntent(projectID, claimedBy string) (*database.ProjectIntent, error) {
	var best *database.ProjectIntent
	for _, in := range s.intents {
		if in.ProjectID != projectID || in.Status != database.IntentStatusOpen {
			continue
		}
		if best == nil || in.Priority > best.Priority {
			best = in
		}
	}
	if best == nil {
		return nil, nil
	}
	best.Status = database.IntentStatusClaimed
	best.ClaimedBy = claimedBy
	cp := *best
	return &cp, nil
}

func (s *fakeStore) CompleteIntent(id, resultFactKey, resultSummary string) error {
	in, ok := s.intents[id]
	if !ok {
		return nil
	}
	in.Status = database.IntentStatusDone
	in.ResultFactKey = resultFactKey
	in.ResultSummary = resultSummary
	return nil
}

func (s *fakeStore) UpdateIntentStatus(id, status string) error {
	if in, ok := s.intents[id]; ok {
		in.Status = status
	}
	return nil
}

func (s *fakeStore) CreateProjectHint(h *database.ProjectHint) (*database.ProjectHint, error) {
	if h.ID == "" {
		h.ID = s.nextID("hint")
	}
	if h.Status == "" {
		h.Status = database.HintStatusActive
	}
	cp := *h
	s.hints[h.ID] = &cp
	return &cp, nil
}

func (s *fakeStore) ListProjectHints(projectID, status string) ([]*database.ProjectHint, error) {
	var out []*database.ProjectHint
	for _, h := range s.hints {
		if h.ProjectID == projectID && h.Status == status {
			out = append(out, h)
		}
	}
	return out, nil
}

func (s *fakeStore) ArchiveProjectHint(id string) error {
	if h, ok := s.hints[id]; ok {
		h.Status = database.HintStatusArchived
	}
	return nil
}

func TestBoardWriteReadSnapshot(t *testing.T) {
	b := New(newFakeStore(), "proj1", "conv1")
	var changes []Change
	b.OnChange(func(c Change) { changes = append(changes, c) })

	if _, err := b.AddFact(&database.ProjectFact{FactKey: "host.web1", Category: "host", Summary: "web server", Confidence: "confirmed"}); err != nil {
		t.Fatalf("AddFact: %v", err)
	}
	if _, err := b.AddIntent("scan ports on web1", "nmap -sV", "host.web1", 5); err != nil {
		t.Fatalf("AddIntent: %v", err)
	}
	if _, err := b.AddHint("focus on the login endpoint"); err != nil {
		t.Fatalf("AddHint: %v", err)
	}

	snap, err := b.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if f, op, _, _, h := snap.Counts(); f != 1 || op != 1 || h != 1 {
		t.Fatalf("counts = facts %d open %d hints %d, want 1/1/1", f, op, h)
	}

	rendered := snap.RenderForModel()
	for _, want := range []string{"host.web1", "scan ports on web1", "focus on the login endpoint"} {
		if !contains(rendered, want) {
			t.Errorf("rendered snapshot missing %q\n%s", want, rendered)
		}
	}

	// 写入应触发三次 onChange
	if len(changes) != 3 {
		t.Errorf("onChange fired %d times, want 3", len(changes))
	}
}

func TestBoardClaimIsAtomicAndPriorityOrdered(t *testing.T) {
	b := New(newFakeStore(), "proj1", "conv1")
	if _, err := b.AddIntent("low prio", "", "", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddIntent("high prio", "", "", 9); err != nil {
		t.Fatal(err)
	}

	first, err := b.ClaimIntent("worker-1")
	if err != nil || first == nil {
		t.Fatalf("claim 1: %v, %v", first, err)
	}
	if first.Title != "high prio" {
		t.Errorf("claimed %q, want high prio first", first.Title)
	}
	if first.Status != database.IntentStatusClaimed {
		t.Errorf("status = %q, want claimed", first.Status)
	}

	second, err := b.ClaimIntent("worker-2")
	if err != nil || second == nil {
		t.Fatalf("claim 2: %v, %v", second, err)
	}
	if second.Title != "low prio" {
		t.Errorf("claimed %q, want low prio second", second.Title)
	}

	// 无剩余 open 意图
	third, err := b.ClaimIntent("worker-3")
	if err != nil {
		t.Fatal(err)
	}
	if third != nil {
		t.Errorf("expected nil claim when no open intents, got %v", third)
	}
}

func TestBoardCompleteIntentRemovesFromOpen(t *testing.T) {
	b := New(newFakeStore(), "proj1", "")
	in, _ := b.AddIntent("do thing", "", "", 0)
	claimed, _ := b.ClaimIntent("w1")
	if claimed == nil {
		t.Fatal("expected claim")
	}
	if err := b.CompleteIntent(in.ID, "result.fact", "found it"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	snap, _ := b.Snapshot()
	if _, open, _, done, _ := snap.Counts(); open != 0 || done != 1 {
		t.Errorf("after complete: open %d done %d, want 0/1", open, done)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (indexOf(haystack, needle) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
