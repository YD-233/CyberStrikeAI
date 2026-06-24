package config

import "testing"

func TestInferModelContextWindow(t *testing.T) {
	cases := map[string]int{
		// 当代旗舰：1M 起
		"qwen3-max":         1000000,
		"qwen3-plus":        1000000,
		"claude-opus-4-8":   1000000,
		"claude-sonnet-4-6": 1000000,
		"gpt-5.5":           1000000,
		"gpt-5.4":           1000000,
		"gemini-3.1-pro":    1000000,
		"deepseek-v4-pro":   1000000,
		"glm-5.2":           1000000,
		"minimax-m3":        1000000,
		"mimo-v2.5":         1000000,
		// 特例
		"gpt-5.4-mini":      400000,  // mini 档 400K
		"grok-4":            2000000, // grok-4 标称 2M
		"grok-4.3":          1000000, // grok-4.x 回到 1M
		"kimi-k2.6":         256000,
		"deepseek-chat":     64000, // V3 老窗口
		"deepseek-reasoner": 64000,
		// 老模型保守
		"qwen2.5-max":   128000,
		"claude-3-opus": 200000,
		"claude-2.1":    200000,
		"gpt-4o":        128000,
		"gpt-4":         32000,
		"gpt-4-32k":     32000,
		"gpt-3.5-turbo": 16000,
		"o1-preview":    200000,
		"glm-4-plus":    128000,
		// 未知保守
		"":               128000,
		"some-new-model": 128000,
	}
	for model, want := range cases {
		if got := inferModelContextWindow(model); got != want {
			t.Errorf("inferModelContextWindow(%q) = %d, want %d", model, got, want)
		}
	}
}

func TestMaxTotalTokensEffective(t *testing.T) {
	// 显式配置优先
	explicit := OpenAIConfig{Model: "deepseek-chat", MaxTotalTokens: 500000}
	if got := explicit.MaxTotalTokensEffective(); got != 500000 {
		t.Errorf("explicit override = %d, want 500000", got)
	}
	// 0 = 自动推断
	auto := OpenAIConfig{Model: "claude-opus-4-8", MaxTotalTokens: 0}
	if got := auto.MaxTotalTokensEffective(); got != 1000000 {
		t.Errorf("auto-infer = %d, want 1000000", got)
	}
	// 负数也视为自动
	neg := OpenAIConfig{Model: "qwen3-max", MaxTotalTokens: -1}
	if got := neg.MaxTotalTokensEffective(); got != 1000000 {
		t.Errorf("negative treated as auto = %d, want 1000000", got)
	}
}
