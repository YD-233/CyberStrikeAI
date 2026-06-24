package config

import "strings"

// ModelsConfig 双模型分层路由（受 Cairn 多后端 dispatch.yaml 启发）：
//   - high：解决能力强的高级模型（如 Claude Opus），用于主编排器 / 规划器 / Reason / 单代理主脑；
//   - low ：指令遵循好、速度快、成本低的低级模型（如 DeepSeek flash），用于子代理 / 执行器 / Explore / 摘要。
//
// 兼容性：任一档位未配置（或字段留空）时按字段回退到主 openai 块；整个 models 块缺省时，
// 高/低档位都等价于现有 openai 单模型，旧配置零改动可用。
type ModelsConfig struct {
	// Enabled 仅作为 UI/日志的显式意图标记；实际是否分层由 Active() 按是否配置了档位模型判定。
	Enabled bool          `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	High    *OpenAIConfig `yaml:"high,omitempty" json:"high,omitempty"`
	Low     *OpenAIConfig `yaml:"low,omitempty" json:"low,omitempty"`
}

// mergeOpenAIOverride 以 main 为基底，用 override 中的非空字段逐项覆盖（与 VisionConfig.OpenAICfgEffective 同思路）。
func mergeOpenAIOverride(main OpenAIConfig, override *OpenAIConfig) OpenAIConfig {
	out := main
	if override == nil {
		return out
	}
	if s := strings.TrimSpace(override.Provider); s != "" {
		out.Provider = s
	}
	if s := strings.TrimSpace(override.APIKey); s != "" {
		out.APIKey = s
	}
	if s := strings.TrimSpace(override.BaseURL); s != "" {
		out.BaseURL = s
	}
	if s := strings.TrimSpace(override.Model); s != "" {
		out.Model = s
	}
	if override.MaxTotalTokens > 0 {
		out.MaxTotalTokens = override.MaxTotalTokens
	}
	if s := strings.TrimSpace(override.Reasoning.Mode); s != "" {
		out.Reasoning.Mode = s
	}
	if s := strings.TrimSpace(override.Reasoning.Effort); s != "" {
		out.Reasoning.Effort = s
	}
	if s := strings.TrimSpace(override.Reasoning.Profile); s != "" {
		out.Reasoning.Profile = s
	}
	if override.Reasoning.AllowClientReasoning != nil {
		out.Reasoning.AllowClientReasoning = override.Reasoning.AllowClientReasoning
	}
	if len(override.Reasoning.ExtraRequestFields) > 0 {
		out.Reasoning.ExtraRequestFields = override.Reasoning.ExtraRequestFields
	}
	return out
}

// HighEffective 返回高级档位的有效 OpenAIConfig（未配置字段回退到 main）。
func (m ModelsConfig) HighEffective(main OpenAIConfig) OpenAIConfig {
	return mergeOpenAIOverride(main, m.High)
}

// LowEffective 返回低级档位的有效 OpenAIConfig（未配置字段回退到 main）。
func (m ModelsConfig) LowEffective(main OpenAIConfig) OpenAIConfig {
	return mergeOpenAIOverride(main, m.Low)
}

// TierEffective 按 tier（high|low）返回有效配置；其余取值回退到 main。
func (m ModelsConfig) TierEffective(tier string, main OpenAIConfig) OpenAIConfig {
	switch NormalizeModelTier(tier) {
	case "high":
		return m.HighEffective(main)
	case "low":
		return m.LowEffective(main)
	default:
		return main
	}
}

// Active 表示已实际配置出可用的分层（至少一个档位指定了 model）。
// 未激活时高/低档位都回退到主 openai，路由逻辑仍可无条件调用 TierEffective。
func (m ModelsConfig) Active() bool {
	hasHigh := m.High != nil && strings.TrimSpace(m.High.Model) != ""
	hasLow := m.Low != nil && strings.TrimSpace(m.Low.Model) != ""
	return hasHigh || hasLow
}

// NormalizeModelTier 归一化档位字符串；空或未知返回空串（交由调用方按角色默认）。
func NormalizeModelTier(tier string) string {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "high", "strong", "opus", "advanced":
		return "high"
	case "low", "fast", "flash", "cheap", "basic":
		return "low"
	default:
		return ""
	}
}
