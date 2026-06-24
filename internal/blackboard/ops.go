package blackboard

import (
	"fmt"
	"strings"
	"time"

	"cyberstrike-ai/internal/database"
)

// Snapshot 读取黑板当前一致视图（Observe 阶段）。Facts 排除 deprecated。
func (b *Board) Snapshot() (*Snapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	facts, err := b.store.ListProjectFactsForIndex(b.projectID, false)
	if err != nil {
		return nil, fmt.Errorf("读取事实失败: %w", err)
	}
	intents, err := b.store.ListProjectIntents(b.projectID, "")
	if err != nil {
		return nil, fmt.Errorf("读取意图失败: %w", err)
	}
	hints, err := b.store.ListProjectHints(b.projectID, database.HintStatusActive)
	if err != nil {
		return nil, fmt.Errorf("读取提示失败: %w", err)
	}

	snap := &Snapshot{Facts: facts, Hints: hints, TakenAt: time.Now()}
	for _, in := range intents {
		switch in.Status {
		case database.IntentStatusOpen:
			snap.OpenIntents = append(snap.OpenIntents, in)
		case database.IntentStatusClaimed:
			snap.ClaimedIntents = append(snap.ClaimedIntents, in)
		case database.IntentStatusDone:
			snap.DoneIntents = append(snap.DoneIntents, in)
		}
	}
	return snap, nil
}

// AddFact 写入/更新一条事实（Explore / Bootstrap 的产出）。
func (b *Board) AddFact(f *database.ProjectFact) (*database.ProjectFact, error) {
	if f.ProjectID == "" {
		f.ProjectID = b.projectID
	}
	if f.SourceConversationID == "" {
		f.SourceConversationID = b.convID
	}
	b.mu.Lock()
	saved, err := b.store.UpsertProjectFact(f)
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	b.emit(Change{Kind: ChangeFactAdded, Summary: factSummaryLine(saved), Fact: saved})
	return saved, nil
}

// AddIntent 声明一个新的探索方向（Reason / Bootstrap 的产出）。
func (b *Board) AddIntent(title, body, parentFactKey string, priority int) (*database.ProjectIntent, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, fmt.Errorf("intent title 不能为空")
	}
	in := &database.ProjectIntent{
		ProjectID:      b.projectID,
		ConversationID: b.convID,
		Title:          title,
		Body:           strings.TrimSpace(body),
		ParentFactKey:  strings.TrimSpace(parentFactKey),
		Priority:       priority,
		Status:         database.IntentStatusOpen,
	}
	b.mu.Lock()
	saved, err := b.store.CreateProjectIntent(in)
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	b.emit(Change{Kind: ChangeIntentAdded, Summary: "意图: " + title, Intent: saved})
	return saved, nil
}

// ClaimIntent 原子认领下一条 open 意图。返回 nil,nil 表示无可认领。
func (b *Board) ClaimIntent(workerID string) (*database.ProjectIntent, error) {
	b.mu.Lock()
	in, err := b.store.ClaimNextOpenIntent(b.projectID, workerID)
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if in == nil {
		return nil, nil
	}
	b.emit(Change{Kind: ChangeIntentClaimed, Summary: workerID + " 认领: " + in.Title, Intent: in})
	return in, nil
}

// CompleteIntent 标记意图完成并关联产出事实。
func (b *Board) CompleteIntent(intentID, resultFactKey, resultSummary string) error {
	b.mu.Lock()
	err := b.store.CompleteIntent(intentID, resultFactKey, resultSummary)
	b.mu.Unlock()
	if err != nil {
		return err
	}
	b.emit(Change{Kind: ChangeIntentDone, Summary: "完成意图 → " + resultFactKey,
		Intent: &database.ProjectIntent{ID: intentID, ResultFactKey: resultFactKey, ResultSummary: resultSummary}})
	return nil
}

// DropIntent 放弃意图（去重 / 无价值 / 执行失败不再重试）。
func (b *Board) DropIntent(intentID, reason string) error {
	b.mu.Lock()
	err := b.store.UpdateIntentStatus(intentID, database.IntentStatusDropped)
	b.mu.Unlock()
	if err != nil {
		return err
	}
	b.emit(Change{Kind: ChangeIntentDropped, Summary: "放弃意图: " + reason,
		Intent: &database.ProjectIntent{ID: intentID}})
	return nil
}

// AddHint 注入人类判断（Hint），worker 下一次读板时吸收。
func (b *Board) AddHint(content string) (*database.ProjectHint, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, fmt.Errorf("hint 内容不能为空")
	}
	h := &database.ProjectHint{
		ProjectID:      b.projectID,
		ConversationID: b.convID,
		Content:        content,
		Status:         database.HintStatusActive,
	}
	b.mu.Lock()
	saved, err := b.store.CreateProjectHint(h)
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	b.emit(Change{Kind: ChangeHintAdded, Summary: "提示: " + content, Hint: saved})
	return saved, nil
}

func factSummaryLine(f *database.ProjectFact) string {
	if f == nil {
		return ""
	}
	s := f.Summary
	if s == "" {
		s = f.FactKey
	}
	return "事实[" + f.Category + "] " + f.FactKey + ": " + s
}
