// Package blackboard 实现 Cairn 风格的黑板协调层：Fact / Intent / Hint 三原语 + Stigmergy。
//
// 设计要点：
//   - 黑板是 worker 之间唯一的协调媒介（间接协调 / Stigmergy），worker 之间不直接通信。
//   - Fact 复用既有 project_facts + project_fact_edges（DB 持久化的事实图）。
//   - Intent / Hint 持久化在 project_intents / project_hints。
//   - Board 在内存中维护一致视图并提供原子认领；所有写入同步落库，进程重启可恢复。
//   - onChange 回调用于把黑板事件投射成 SSE（fact_added / intent_added / intent_claimed 等）。
package blackboard

import (
	"strings"
	"sync"
	"time"

	"cyberstrike-ai/internal/database"
)

// Persister 是 Board 依赖的持久化接口；*database.DB 结构上即满足，便于测试替身。
type Persister interface {
	UpsertProjectFact(f *database.ProjectFact) (*database.ProjectFact, error)
	ListProjectFactsForIndex(projectID string, includeDeprecated bool) ([]*database.ProjectFact, error)

	CreateProjectIntent(in *database.ProjectIntent) (*database.ProjectIntent, error)
	ListProjectIntents(projectID, status string) ([]*database.ProjectIntent, error)
	ClaimNextOpenIntent(projectID, claimedBy string) (*database.ProjectIntent, error)
	CompleteIntent(id, resultFactKey, resultSummary string) error
	UpdateIntentStatus(id, status string) error

	CreateProjectHint(h *database.ProjectHint) (*database.ProjectHint, error)
	ListProjectHints(projectID, status string) ([]*database.ProjectHint, error)
	ArchiveProjectHint(id string) error
}

// ChangeKind 黑板变更类型（用于 SSE 投射）。
type ChangeKind string

const (
	ChangeFactAdded     ChangeKind = "fact_added"
	ChangeIntentAdded   ChangeKind = "intent_added"
	ChangeIntentClaimed ChangeKind = "intent_claimed"
	ChangeIntentDone    ChangeKind = "intent_done"
	ChangeIntentDropped ChangeKind = "intent_dropped"
	ChangeHintAdded     ChangeKind = "hint_added"
)

// Change 一次黑板变更事件。
type Change struct {
	Kind    ChangeKind
	Summary string                  // 人类可读摘要
	Fact    *database.ProjectFact   // ChangeFact* 时填充
	Intent  *database.ProjectIntent // ChangeIntent* 时填充
	Hint    *database.ProjectHint   // ChangeHintAdded 时填充
}

// Snapshot 黑板只读快照：worker 的 Observe 阶段读取它。
type Snapshot struct {
	Facts         []*database.ProjectFact
	OpenIntents   []*database.ProjectIntent
	ClaimedIntents []*database.ProjectIntent
	DoneIntents   []*database.ProjectIntent
	Hints         []*database.ProjectHint
	TakenAt       time.Time
}

// Board 黑板：单项目、单次运行的协调中枢。并发安全。
type Board struct {
	mu        sync.Mutex
	store     Persister
	projectID string
	convID    string

	onChange func(Change)
}

// New 创建 Board。projectID 必填（Fact/Intent/Hint 均按项目隔离）。
func New(store Persister, projectID, conversationID string) *Board {
	return &Board{
		store:     store,
		projectID: strings.TrimSpace(projectID),
		convID:    strings.TrimSpace(conversationID),
	}
}

// ProjectID 返回黑板绑定的项目 ID。
func (b *Board) ProjectID() string { return b.projectID }

// OnChange 注册变更回调（用于 SSE 投射）。非并发安全，应在运行前设置一次。
func (b *Board) OnChange(fn func(Change)) { b.onChange = fn }

func (b *Board) emit(c Change) {
	if b.onChange != nil {
		b.onChange(c)
	}
}
