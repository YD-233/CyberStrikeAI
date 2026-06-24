package handler

import (
	"net/http"
	"strings"

	"cyberstrike-ai/internal/database"

	"github.com/gin-gonic/gin"
)

// 黑板 Intent / Hint REST 端点（Cairn 三原语中的 Intent 与 Hint）。
// Fact 复用既有 /projects/:id/facts*；此处补齐意图列表与人类提示注入。

// ListIntents GET /api/projects/:id/intents?status=open|claimed|done|dropped
func (h *ProjectHandler) ListIntents(c *gin.Context) {
	projectID := c.Param("id")
	if _, err := h.db.GetProject(projectID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "项目不存在"})
		return
	}
	status := strings.TrimSpace(c.Query("status"))
	list, err := h.db.ListProjectIntents(projectID, status)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if list == nil {
		list = []*database.ProjectIntent{}
	}
	c.JSON(http.StatusOK, gin.H{"intents": list})
}

type createIntentRequest struct {
	Title         string `json:"title" binding:"required"`
	Body          string `json:"body"`
	ParentFactKey string `json:"parent_fact_key"`
	Priority      int    `json:"priority"`
}

// CreateIntent POST /api/projects/:id/intents — 人类手动声明一个探索方向。
func (h *ProjectHandler) CreateIntent(c *gin.Context) {
	projectID := c.Param("id")
	if _, err := h.db.GetProject(projectID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "项目不存在"})
		return
	}
	var req createIntentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	in, err := h.db.CreateProjectIntent(&database.ProjectIntent{
		ProjectID:     projectID,
		Title:         strings.TrimSpace(req.Title),
		Body:          strings.TrimSpace(req.Body),
		ParentFactKey: strings.TrimSpace(req.ParentFactKey),
		Priority:      req.Priority,
		Status:        database.IntentStatusOpen,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, in)
}

// ListHints GET /api/projects/:id/hints?status=active|archived
func (h *ProjectHandler) ListHints(c *gin.Context) {
	projectID := c.Param("id")
	if _, err := h.db.GetProject(projectID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "项目不存在"})
		return
	}
	status := strings.TrimSpace(c.Query("status"))
	list, err := h.db.ListProjectHints(projectID, status)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if list == nil {
		list = []*database.ProjectHint{}
	}
	c.JSON(http.StatusOK, gin.H{"hints": list})
}

type createHintRequest struct {
	Content        string `json:"content" binding:"required"`
	ConversationID string `json:"conversation_id"`
}

// CreateHint POST /api/projects/:id/hints — 注入人类判断（OODA worker 下一次读板时吸收）。
func (h *ProjectHandler) CreateHint(c *gin.Context) {
	projectID := c.Param("id")
	if _, err := h.db.GetProject(projectID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "项目不存在"})
		return
	}
	var req createHintRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	hint, err := h.db.CreateProjectHint(&database.ProjectHint{
		ProjectID:      projectID,
		ConversationID: strings.TrimSpace(req.ConversationID),
		Content:        strings.TrimSpace(req.Content),
		Status:         database.HintStatusActive,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, hint)
}

// ArchiveHint POST /api/projects/:id/hints/:hintId/archive — 归档提示（不再参与读板）。
func (h *ProjectHandler) ArchiveHint(c *gin.Context) {
	if _, err := h.db.GetProject(c.Param("id")); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "项目不存在"})
		return
	}
	if err := h.db.ArchiveProjectHint(c.Param("hintId")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true})
}
