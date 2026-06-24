package database

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Intent / Hint 状态常量（Cairn 黑板模型）。
const (
	IntentStatusOpen    = "open"    // 待认领
	IntentStatusClaimed = "claimed" // 已认领，正在被某 worker 执行
	IntentStatusDone    = "done"    // 已完成，产出 result_fact_key
	IntentStatusDropped = "dropped" // 放弃（去重 / 无价值）

	HintStatusActive   = "active"
	HintStatusArchived = "archived"
)

// ProjectIntent 黑板意图：声明但尚未执行的探索方向（Cairn Intent）。
type ProjectIntent struct {
	ID             string    `json:"id"`
	ProjectID      string    `json:"project_id"`
	ConversationID string    `json:"conversation_id,omitempty"`
	Title          string    `json:"title"`
	Body           string    `json:"body,omitempty"`
	Status         string    `json:"status"`
	Priority       int       `json:"priority"`
	ClaimedBy      string    `json:"claimed_by,omitempty"`
	ParentFactKey  string    `json:"parent_fact_key,omitempty"`
	ResultFactKey  string    `json:"result_fact_key,omitempty"`
	ResultSummary  string    `json:"result_summary,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ProjectHint 黑板提示：人类随时注入的判断（Cairn Hint）。
type ProjectHint struct {
	ID             string    `json:"id"`
	ProjectID      string    `json:"project_id"`
	ConversationID string    `json:"conversation_id,omitempty"`
	Content        string    `json:"content"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// CreateProjectIntent 创建意图。空 status 默认 open。
func (db *DB) CreateProjectIntent(in *ProjectIntent) (*ProjectIntent, error) {
	if strings.TrimSpace(in.ProjectID) == "" {
		return nil, fmt.Errorf("project_id 不能为空")
	}
	if in.ID == "" {
		in.ID = uuid.New().String()
	}
	if strings.TrimSpace(in.Status) == "" {
		in.Status = IntentStatusOpen
	}
	now := time.Now()
	if in.CreatedAt.IsZero() {
		in.CreatedAt = now
	}
	in.UpdatedAt = now
	_, err := db.Exec(
		`INSERT INTO project_intents (
			id, project_id, conversation_id, title, body, status, priority,
			claimed_by, parent_fact_key, result_fact_key, result_summary, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.ID, in.ProjectID, nullIfEmpty(in.ConversationID), in.Title, in.Body, in.Status, in.Priority,
		nullIfEmpty(in.ClaimedBy), nullIfEmpty(in.ParentFactKey), nullIfEmpty(in.ResultFactKey),
		nullIfEmpty(in.ResultSummary), in.CreatedAt, in.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("创建意图失败: %w", err)
	}
	return in, nil
}

// ListProjectIntents 列出意图；status 为空则全部，否则按状态过滤。按优先级降序、创建时间升序。
func (db *DB) ListProjectIntents(projectID, status string) ([]*ProjectIntent, error) {
	query := `SELECT id, project_id, COALESCE(conversation_id,''), title, COALESCE(body,''), status, priority,
		COALESCE(claimed_by,''), COALESCE(parent_fact_key,''), COALESCE(result_fact_key,''),
		COALESCE(result_summary,''), created_at, updated_at
		FROM project_intents WHERE project_id = ?`
	args := []interface{}{projectID}
	if s := strings.TrimSpace(status); s != "" {
		query += " AND status = ?"
		args = append(args, s)
	}
	query += " ORDER BY priority DESC, created_at ASC"
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanProjectIntents(rows)
}

// ClaimNextOpenIntent 原子认领一条 open 意图（按优先级），置为 claimed 并记录 claimedBy。
// 返回 nil,nil 表示当前无可认领意图。
func (db *DB) ClaimNextOpenIntent(projectID, claimedBy string) (*ProjectIntent, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var in ProjectIntent
	var convID, body, claimed, parent, resultKey, resultSummary string
	var createdAt, updatedAt string
	err = tx.QueryRow(
		`SELECT id, project_id, COALESCE(conversation_id,''), title, COALESCE(body,''), status, priority,
			COALESCE(claimed_by,''), COALESCE(parent_fact_key,''), COALESCE(result_fact_key,''),
			COALESCE(result_summary,''), created_at, updated_at
		 FROM project_intents WHERE project_id = ? AND status = 'open'
		 ORDER BY priority DESC, created_at ASC LIMIT 1`,
		projectID,
	).Scan(&in.ID, &in.ProjectID, &convID, &in.Title, &body, &in.Status, &in.Priority,
		&claimed, &parent, &resultKey, &resultSummary, &createdAt, &updatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	now := time.Now()
	if _, err := tx.Exec(
		`UPDATE project_intents SET status = 'claimed', claimed_by = ?, updated_at = ? WHERE id = ?`,
		claimedBy, now, in.ID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	in.ConversationID = convID
	in.Body = body
	in.Status = IntentStatusClaimed
	in.ClaimedBy = claimedBy
	in.ParentFactKey = parent
	in.ResultFactKey = resultKey
	in.ResultSummary = resultSummary
	in.CreatedAt = parseDBTime(createdAt)
	in.UpdatedAt = now
	return &in, nil
}

// CompleteIntent 标记意图完成，记录产出事实 key 与摘要。
func (db *DB) CompleteIntent(id, resultFactKey, resultSummary string) error {
	res, err := db.Exec(
		`UPDATE project_intents SET status = 'done', result_fact_key = ?, result_summary = ?, updated_at = ? WHERE id = ?`,
		nullIfEmpty(resultFactKey), nullIfEmpty(resultSummary), time.Now(), id,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("意图不存在")
	}
	return nil
}

// UpdateIntentStatus 更新意图状态（用于 dropped / 重新 open 等）。
func (db *DB) UpdateIntentStatus(id, status string) error {
	res, err := db.Exec(
		`UPDATE project_intents SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now(), id,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("意图不存在")
	}
	return nil
}

func scanProjectIntents(rows *sql.Rows) ([]*ProjectIntent, error) {
	var out []*ProjectIntent
	for rows.Next() {
		var in ProjectIntent
		var convID, body, claimed, parent, resultKey, resultSummary string
		var createdAt, updatedAt string
		if err := rows.Scan(&in.ID, &in.ProjectID, &convID, &in.Title, &body, &in.Status, &in.Priority,
			&claimed, &parent, &resultKey, &resultSummary, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		in.ConversationID = convID
		in.Body = body
		in.ClaimedBy = claimed
		in.ParentFactKey = parent
		in.ResultFactKey = resultKey
		in.ResultSummary = resultSummary
		in.CreatedAt = parseDBTime(createdAt)
		in.UpdatedAt = parseDBTime(updatedAt)
		out = append(out, &in)
	}
	return out, rows.Err()
}

// CreateProjectHint 创建提示（人类判断）。空 status 默认 active。
func (db *DB) CreateProjectHint(h *ProjectHint) (*ProjectHint, error) {
	if strings.TrimSpace(h.ProjectID) == "" {
		return nil, fmt.Errorf("project_id 不能为空")
	}
	if h.ID == "" {
		h.ID = uuid.New().String()
	}
	if strings.TrimSpace(h.Status) == "" {
		h.Status = HintStatusActive
	}
	now := time.Now()
	if h.CreatedAt.IsZero() {
		h.CreatedAt = now
	}
	h.UpdatedAt = now
	_, err := db.Exec(
		`INSERT INTO project_hints (id, project_id, conversation_id, content, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		h.ID, h.ProjectID, nullIfEmpty(h.ConversationID), h.Content, h.Status, h.CreatedAt, h.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("创建提示失败: %w", err)
	}
	return h, nil
}

// ListProjectHints 列出提示；status 为空默认仅 active。
func (db *DB) ListProjectHints(projectID, status string) ([]*ProjectHint, error) {
	if strings.TrimSpace(status) == "" {
		status = HintStatusActive
	}
	rows, err := db.Query(
		`SELECT id, project_id, COALESCE(conversation_id,''), content, status, created_at, updated_at
		 FROM project_hints WHERE project_id = ? AND status = ? ORDER BY created_at ASC`,
		projectID, status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ProjectHint
	for rows.Next() {
		var h ProjectHint
		var convID, createdAt, updatedAt string
		if err := rows.Scan(&h.ID, &h.ProjectID, &convID, &h.Content, &h.Status, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		h.ConversationID = convID
		h.CreatedAt = parseDBTime(createdAt)
		h.UpdatedAt = parseDBTime(updatedAt)
		out = append(out, &h)
	}
	return out, rows.Err()
}

// ArchiveProjectHint 归档提示（吸收后不再参与读板）。
func (db *DB) ArchiveProjectHint(id string) error {
	_, err := db.Exec(`UPDATE project_hints SET status = 'archived', updated_at = ? WHERE id = ?`, time.Now(), id)
	return err
}
