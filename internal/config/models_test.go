package config

import "testing"

func boolPtr(b bool) *bool { return &b }

func baseMain() OpenAIConfig {
	return OpenAIConfig{
		Provider:       "openai",
		APIKey:         "main-key",
		BaseURL:        "https://main.example/v1",
		Model:          "main-model",
		MaxTotalTokens: 120000,
		Reasoning:      OpenAIReasoningConfig{Mode: "on", Effort: "high", Profile: "openai_compat"},
	}
}

func TestModelsConfig_FallbackWhenUnset(t *testing.T) {
	var m ModelsConfig
	main := baseMain()
	if m.Active() {
		t.Fatalf("empty models should be inactive")
	}
	if got := m.HighEffective(main); got.Model != "main-model" || got.APIKey != "main-key" {
		t.Fatalf("high should fall back to main, got %+v", got)
	}
	if got := m.LowEffective(main); got.Model != "main-model" || got.BaseURL != "https://main.example/v1" {
		t.Fatalf("low should fall back to main, got %+v", got)
	}
}

func TestModelsConfig_TierOverrideAndPartialFallback(t *testing.T) {
	main := baseMain()
	m := ModelsConfig{
		High: &OpenAIConfig{Provider: "claude", BaseURL: "https://api.anthropic.com", APIKey: "hi-key", Model: "claude-opus-4-8"},
		// Low only overrides model + base_url; api_key empty must fall back to main.
		Low: &OpenAIConfig{BaseURL: "https://api.deepseek.com/v1", Model: "deepseek-chat"},
	}
	if !m.Active() {
		t.Fatalf("models with tier models should be active")
	}

	high := m.HighEffective(main)
	if high.Provider != "claude" || high.Model != "claude-opus-4-8" || high.APIKey != "hi-key" {
		t.Fatalf("high override wrong: %+v", high)
	}
	// MaxTotalTokens not set on high override -> inherits main.
	if high.MaxTotalTokens != 120000 {
		t.Fatalf("high should inherit main max_total_tokens, got %d", high.MaxTotalTokens)
	}

	low := m.LowEffective(main)
	if low.Model != "deepseek-chat" || low.BaseURL != "https://api.deepseek.com/v1" {
		t.Fatalf("low override wrong: %+v", low)
	}
	if low.APIKey != "main-key" {
		t.Fatalf("low empty api_key should fall back to main, got %q", low.APIKey)
	}
	if low.Provider != "openai" {
		t.Fatalf("low empty provider should fall back to main, got %q", low.Provider)
	}
}

func TestModelsConfig_TierEffectiveAndNormalize(t *testing.T) {
	main := baseMain()
	m := ModelsConfig{
		High: &OpenAIConfig{Model: "hi"},
		Low:  &OpenAIConfig{Model: "lo"},
	}
	if got := m.TierEffective("high", main).Model; got != "hi" {
		t.Fatalf("tier high => hi, got %q", got)
	}
	if got := m.TierEffective("opus", main).Model; got != "hi" {
		t.Fatalf("alias opus => high(hi), got %q", got)
	}
	if got := m.TierEffective("flash", main).Model; got != "lo" {
		t.Fatalf("alias flash => low(lo), got %q", got)
	}
	// Unknown tier falls back to main (caller applies its own default).
	if got := m.TierEffective("", main).Model; got != "main-model" {
		t.Fatalf("unknown tier => main, got %q", got)
	}
}

func TestNormalizeModelTier(t *testing.T) {
	cases := map[string]string{
		"high": "high", "HIGH": "high", "strong": "high", "opus": "high", "advanced": "high",
		"low": "low", "fast": "low", "flash": "low", "cheap": "low", "basic": "low",
		"": "", "weird": "",
	}
	for in, want := range cases {
		if got := NormalizeModelTier(in); got != want {
			t.Fatalf("NormalizeModelTier(%q)=%q want %q", in, got, want)
		}
	}
}

func TestMergeReasoningClientReasoningPointerOverride(t *testing.T) {
	main := baseMain()
	main.Reasoning.AllowClientReasoning = boolPtr(true)
	m := ModelsConfig{Low: &OpenAIConfig{Model: "lo", Reasoning: OpenAIReasoningConfig{AllowClientReasoning: boolPtr(false)}}}
	low := m.LowEffective(main)
	if low.Reasoning.AllowClientReasoning == nil || *low.Reasoning.AllowClientReasoning != false {
		t.Fatalf("low override allow_client_reasoning should be false, got %v", low.Reasoning.AllowClientReasoning)
	}
}
