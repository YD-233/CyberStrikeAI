package multiagent

import (
	"net"
	"net/http"
	"strings"
	"time"

	"cyberstrike-ai/internal/config"
	"cyberstrike-ai/internal/openai"
	"cyberstrike-ai/internal/reasoning"

	einoopenai "github.com/cloudwego/eino-ext/components/model/openai"
	"go.uber.org/zap"
)

// tierModels 为双模型分层路由准备好「高/低」两档的 Eino ChatModelConfig 模板。
// 每档拥有独立的 HTTP 客户端（因 Claude 桥接 transport 与 provider 绑定，高/低可能不同 provider）。
// 当 models 未配置时，两档都回退到 appCfg.OpenAI，行为等价于原单模型路径。
type tierModels struct {
	high    *einoopenai.ChatModelConfig
	low     *einoopenai.ChatModelConfig
	highCfg config.OpenAIConfig
	lowCfg  config.OpenAIConfig
	active  bool
}

// newTierHTTPClient 复用原 runner/single 的连接参数，并按该档 provider 注入 Claude 桥接与摘要诊断 transport。
func newTierHTTPClient(effCfg *config.OpenAIConfig, logger *zap.Logger) *http.Client {
	httpClient := &http.Client{
		Timeout: 30 * time.Minute,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   300 * time.Second,
				KeepAlive: 300 * time.Second,
			}).DialContext,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 60 * time.Minute,
		},
	}
	httpClient = openai.NewEinoHTTPClient(effCfg, httpClient)
	openai.AttachSummarizationDiagTransport(httpClient, logger)
	return httpClient
}

// buildTierChatModelConfig 由某档有效 OpenAIConfig 构建 Eino ChatModelConfig 并应用推理扩展。
func buildTierChatModelConfig(effCfg *config.OpenAIConfig, reasoningClient *reasoning.ClientIntent, logger *zap.Logger) *einoopenai.ChatModelConfig {
	cfg := &einoopenai.ChatModelConfig{
		APIKey:     effCfg.APIKey,
		BaseURL:    strings.TrimSuffix(effCfg.BaseURL, "/"),
		Model:      effCfg.Model,
		HTTPClient: newTierHTTPClient(effCfg, logger),
	}
	reasoning.ApplyToEinoChatModelConfig(cfg, effCfg, reasoningClient)
	return cfg
}

// prepareTierModels 计算高/低档有效配置并构建两档模板。Claude 桥接 round tripper 持有 cfg 指针，
// 因此把 highCfg/lowCfg 存入返回的 tm（堆分配，生命周期覆盖整轮请求），再取其地址传入。
func prepareTierModels(appCfg *config.Config, reasoningClient *reasoning.ClientIntent, logger *zap.Logger) *tierModels {
	tm := &tierModels{active: appCfg.Models.Active()}
	tm.highCfg = appCfg.Models.HighEffective(appCfg.OpenAI)
	tm.lowCfg = appCfg.Models.LowEffective(appCfg.OpenAI)
	tm.high = buildTierChatModelConfig(&tm.highCfg, reasoningClient, logger)
	tm.low = buildTierChatModelConfig(&tm.lowCfg, reasoningClient, logger)
	return tm
}

// cfgForTier 按档位返回 ChatModelConfig 模板；未知档位回退到 low（子代理/执行器默认档）。
func (tm *tierModels) cfgForTier(tier string) *einoopenai.ChatModelConfig {
	switch config.NormalizeModelTier(tier) {
	case "high":
		return tm.high
	case "low":
		return tm.low
	default:
		return tm.low
	}
}

// modelNameForTier 返回该档实际模型名，供 token 遥测标签使用。
func (tm *tierModels) modelNameForTier(tier string) string {
	switch config.NormalizeModelTier(tier) {
	case "high":
		return tm.highCfg.Model
	default:
		return tm.lowCfg.Model
	}
}

// tierForSub 决定子代理使用的档位：显式 model_tier 优先，否则默认 low（遵循好、快、省）。
func tierForSub(sub config.MultiAgentSubConfig) string {
	if t := config.NormalizeModelTier(sub.ModelTier); t != "" {
		return t
	}
	return "low"
}
