package handler

import (
	"fmt"
	"strings"
	"time"

	"cyberstrike-ai/internal/agent"
	"cyberstrike-ai/internal/database"
	"cyberstrike-ai/internal/mcp/builtin"

	"go.uber.org/zap"
)

// multiAgentPrepared 多代理请求在调用 Eino 前的会话与消息准备结果。
type multiAgentPrepared struct {
	ConversationID     string
	CreatedNew         bool
	History            []agent.ChatMessage
	FinalMessage       string
	RoleTools          []string
	AssistantMessageID string
	UserMessageID      string
}

func (h *AgentHandler) prepareMultiAgentSession(req *ChatRequest) (*multiAgentPrepared, error) {
	if len(req.Attachments) > maxAttachments {
		return nil, fmt.Errorf("附件最多 %d 个", maxAttachments)
	}

	conversationID := strings.TrimSpace(req.ConversationID)
	createdNew := false
	if conversationID == "" {
		title := safeTruncateString(req.Message, 50)
		var conv *database.Conversation
		var err error
		if strings.TrimSpace(req.WebShellConnectionID) != "" {
			conv, err = h.db.CreateConversationWithWebshell(strings.TrimSpace(req.WebShellConnectionID), title)
		} else {
			conv, err = h.db.CreateConversation(title)
		}
		if err != nil {
			return nil, fmt.Errorf("创建对话失败: %w", err)
		}
		conversationID = conv.ID
		createdNew = true
	} else {
		if _, err := h.db.GetConversation(conversationID); err != nil {
			return nil, fmt.Errorf("对话不存在")
		}
	}

	agentHistoryMessages, err := h.loadHistoryFromAgentTrace(conversationID)
	if err != nil {
		historyMessages, getErr := h.db.GetMessages(conversationID)
		if getErr != nil {
			agentHistoryMessages = []agent.ChatMessage{}
		} else {
			agentHistoryMessages = make([]agent.ChatMessage, 0, len(historyMessages))
			for _, msg := range historyMessages {
				agentHistoryMessages = append(agentHistoryMessages, agent.ChatMessage{
					Role:    msg.Role,
					Content: msg.Content,
				})
			}
		}
	}

	finalMessage := req.Message
	var roleTools []string
	if req.WebShellConnectionID != "" {
		conn, errConn := h.db.GetWebshellConnection(strings.TrimSpace(req.WebShellConnectionID))
		if errConn != nil || conn == nil {
			h.logger.Warn("WebShell AI 助手：未找到连接", zap.String("id", req.WebShellConnectionID), zap.Error(errConn))
			return nil, fmt.Errorf("未找到该 WebShell 连接")
		}
		webshellContext := BuildWebshellAssistantContext(conn, WebshellSkillHintMultiAgent, req.Message)
		// WebShell 模式下如果同时指定了角色，追加角色 user_prompt（工具集仍仅限 webshell 专用工具）
		if req.Role != "" && req.Role != "默认" && h.config != nil && h.config.Roles != nil {
			if role, exists := h.config.Roles[req.Role]; exists && role.Enabled && role.UserPrompt != "" {
				finalMessage = role.UserPrompt + "\n\n" + webshellContext
				h.logger.Info("WebShell + 角色: 应用角色提示词（多代理）", zap.String("role", req.Role))
			} else {
				finalMessage = webshellContext
			}
		} else {
			finalMessage = webshellContext
		}
		roleTools = []string{
			builtin.ToolWebshellExec,
			builtin.ToolWebshellFileList,
			builtin.ToolWebshellFileRead,
			builtin.ToolWebshellFileWrite,
			builtin.ToolRecordVulnerability,
			builtin.ToolListKnowledgeRiskTypes,
			builtin.ToolSearchKnowledgeBase,
		}
	} else if req.Role != "" && req.Role != "默认" && h.config != nil && h.config.Roles != nil {
		if role, exists := h.config.Roles[req.Role]; exists && role.Enabled {
			if role.UserPrompt != "" {
				finalMessage = role.UserPrompt + "\n\n" + req.Message
			}
			roleTools = role.Tools
		}
	}

	var savedPaths []string
	if len(req.Attachments) > 0 {
		var aerr error
		savedPaths, aerr = saveAttachmentsToDateAndConversationDir(req.Attachments, conversationID, h.logger)
		if aerr != nil {
			return nil, fmt.Errorf("保存上传文件失败: %w", aerr)
		}
	}
	finalMessage = appendAttachmentsToMessage(finalMessage, req.Attachments, savedPaths)

	userContent := userMessageContentForStorage(req.Message, req.Attachments, savedPaths)
	userMsgRow, uerr := h.db.AddMessage(conversationID, "user", userContent, nil)
	if uerr != nil {
		h.logger.Error("保存用户消息失败", zap.Error(uerr))
		return nil, fmt.Errorf("保存用户消息失败: %w", uerr)
	}
	userMessageID := ""
	if userMsgRow != nil {
		userMessageID = userMsgRow.ID
	}

	assistantMsg, aerr := h.db.AddMessage(conversationID, "assistant", "处理中...", nil)
	var assistantMessageID string
	if aerr != nil {
		h.logger.Warn("创建助手消息占位失败", zap.Error(aerr))
	} else if assistantMsg != nil {
		assistantMessageID = assistantMsg.ID
	}

	return &multiAgentPrepared{
		ConversationID:     conversationID,
		CreatedNew:         createdNew,
		History:            agentHistoryMessages,
		FinalMessage:       finalMessage,
		RoleTools:          roleTools,
		AssistantMessageID: assistantMessageID,
		UserMessageID:      userMessageID,
	}, nil
}

// prepareSessionAfterUserInterrupt 用户「中断并说明」后：结束当前助手占位、写入用户说明、新建助手占位，并生成下一轮 Run 所需的 History + FinalMessage。
func (h *AgentHandler) prepareSessionAfterUserInterrupt(conversationID, prevAssistantMessageID, reason string, roleTools []string) (*multiAgentPrepared, error) {
	if strings.TrimSpace(conversationID) == "" {
		return nil, fmt.Errorf("conversationId 为空")
	}
	if _, err := h.db.GetConversation(conversationID); err != nil {
		return nil, fmt.Errorf("对话不存在")
	}
	note := "（已根据用户说明中断当前步骤，正在继续迭代。）"
	if prevAssistantMessageID != "" {
		if _, err := h.db.Exec("UPDATE messages SET content = ?, updated_at = ? WHERE id = ?", note, time.Now(), prevAssistantMessageID); err != nil {
			return nil, fmt.Errorf("更新助手消息失败: %w", err)
		}
		r := strings.TrimSpace(reason)
		detail := "用户中断并说明"
		if r != "" {
			detail += "：" + r
		}
		_ = h.db.AddProcessDetail(prevAssistantMessageID, conversationID, "user_interrupt", detail, map[string]interface{}{
			"reason": r,
		})
	}
	userContent := fmt.Sprintf("【用户中断说明】%s\n\n请根据以上说明调整并继续任务。", strings.TrimSpace(reason))
	if strings.TrimSpace(reason) == "" {
		userContent = "【用户中断说明】（未填写具体原因）\n\n请根据情况调整并继续任务。"
	}
	userMsgRow, err := h.db.AddMessage(conversationID, "user", userContent, nil)
	if err != nil {
		return nil, fmt.Errorf("保存用户消息失败: %w", err)
	}
	assistantMsg, err := h.db.AddMessage(conversationID, "assistant", "处理中...", nil)
	if err != nil || assistantMsg == nil {
		return nil, fmt.Errorf("创建助手占位失败: %w", err)
	}
	msgs, err := h.db.GetMessages(conversationID)
	if err != nil || len(msgs) < 2 {
		return nil, fmt.Errorf("读取消息历史失败或消息不足")
	}
	histMsgs := msgs[:len(msgs)-2]
	agentHistory := make([]agent.ChatMessage, 0, len(histMsgs))
	for _, msg := range histMsgs {
		agentHistory = append(agentHistory, agent.ChatMessage{
			Role:    msg.Role,
			Content: msg.Content,
		})
	}
	userMessageID := ""
	if userMsgRow != nil {
		userMessageID = userMsgRow.ID
	}
	return &multiAgentPrepared{
		ConversationID:     conversationID,
		CreatedNew:         false,
		History:            agentHistory,
		FinalMessage:       userContent,
		RoleTools:          roleTools,
		AssistantMessageID: assistantMsg.ID,
		UserMessageID:      userMessageID,
	}, nil
}
