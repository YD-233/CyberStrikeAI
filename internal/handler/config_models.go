package handler

import (
	"strings"

	"cyberstrike-ai/internal/config"
)

// maskedSecretPlaceholder 是 GET /api/config 对已设置密钥的回显占位符。
// 前端把它原样回传时，后端识别为「保持原值不变」，从而避免明文密钥泄露给浏览器，
// 也避免「未改动即保存」把占位符当成真实密钥写回配置。
const maskedSecretPlaceholder = "********"

// maskSecret 对非空密钥返回固定占位符；空值保持空（表示未配置）。
func maskSecret(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return maskedSecretPlaceholder
}

// resolveMaskedSecret 在更新配置时还原密钥：收到占位符表示保持 stored 原值，否则使用 incoming（含清空）。
func resolveMaskedSecret(incoming, stored string) string {
	if incoming == maskedSecretPlaceholder {
		return stored
	}
	return incoming
}

// maskModelsSecrets 返回 models 分层配置的脱敏深拷贝（不改动内存中的实时配置）。
func maskModelsSecrets(m config.ModelsConfig) config.ModelsConfig {
	out := config.ModelsConfig{Enabled: m.Enabled}
	if m.High != nil {
		h := *m.High
		h.APIKey = maskSecret(h.APIKey)
		out.High = &h
	}
	if m.Low != nil {
		l := *m.Low
		l.APIKey = maskSecret(l.APIKey)
		out.Low = &l
	}
	return out
}

// applyModelsUpdate 以 incoming 覆盖内存 models 配置，并按 base_url/原值还原各档被脱敏的密钥。
func applyModelsUpdate(dst *config.ModelsConfig, incoming *config.ModelsConfig) {
	if dst == nil || incoming == nil {
		return
	}
	resolveTier := func(in *config.OpenAIConfig, stored *config.OpenAIConfig) *config.OpenAIConfig {
		if in == nil {
			return nil
		}
		t := *in
		var storedKey string
		if stored != nil {
			storedKey = stored.APIKey
		}
		t.APIKey = resolveMaskedSecret(t.APIKey, storedKey)
		return &t
	}
	next := config.ModelsConfig{Enabled: incoming.Enabled}
	next.High = resolveTier(incoming.High, dst.High)
	next.Low = resolveTier(incoming.Low, dst.Low)
	*dst = next
}

// resolveStoredKeyForTest 在「测试连接 / 拉取模型列表」时还原占位符密钥：
// 优先按 base_url 匹配已存储的某一档配置，匹配不到则回退主 openai 密钥。
func (h *ConfigHandler) resolveStoredKeyForTest(baseURL, incoming string) string {
	if incoming != maskedSecretPlaceholder {
		return incoming
	}
	norm := func(s string) string { return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "/")) }
	target := norm(baseURL)
	candidates := []config.OpenAIConfig{h.config.OpenAI}
	if h.config.Models.High != nil {
		candidates = append(candidates, *h.config.Models.High)
	}
	if h.config.Models.Low != nil {
		candidates = append(candidates, *h.config.Models.Low)
	}
	for _, c := range candidates {
		if strings.TrimSpace(c.APIKey) != "" && norm(c.BaseURL) == target {
			return c.APIKey
		}
	}
	return h.config.OpenAI.APIKey
}
