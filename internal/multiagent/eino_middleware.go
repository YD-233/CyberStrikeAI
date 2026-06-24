package multiagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"cyberstrike-ai/internal/agent"
	"cyberstrike-ai/internal/config"
	"cyberstrike-ai/internal/mcp/builtin"

	localbk "github.com/cloudwego/eino-ext/adk/backend/local"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/adk/middlewares/dynamictool/toolsearch"
	"github.com/cloudwego/eino/adk/middlewares/patchtoolcalls"
	"github.com/cloudwego/eino/adk/middlewares/plantask"
	"github.com/cloudwego/eino/adk/middlewares/reduction"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
)

// einoMWPlacement controls which optional middleware runs on orchestrator vs sub-agents.
type einoMWPlacement int

const (
	einoMWMain einoMWPlacement = iota // Deep / Supervisor main chat agent
	einoMWSub                         // Specialist ChatModelAgent
)

func sanitizeEinoPathSegment(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "default"
	}
	s = strings.ReplaceAll(s, string(filepath.Separator), "-")
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, "\\", "-")
	s = strings.ReplaceAll(s, "..", "__")
	if len(s) > 180 {
		s = s[:180]
	}
	return s
}

func splitToolsForToolSearch(all []tool.BaseTool, alwaysVisible int) (static []tool.BaseTool, dynamic []tool.BaseTool, ok bool) {
	if alwaysVisible <= 0 || len(all) <= alwaysVisible+1 {
		return all, nil, false
	}
	return append([]tool.BaseTool(nil), all[:alwaysVisible]...), append([]tool.BaseTool(nil), all[alwaysVisible:]...), true
}

func splitToolsForToolSearchByNames(all []tool.BaseTool, names []string, fallbackAlwaysVisible int) (static []tool.BaseTool, dynamic []tool.BaseTool, ok bool) {
	nameSet := expandAlwaysVisibleNameSet(names)
	if len(nameSet) == 0 {
		return splitToolsForToolSearch(all, fallbackAlwaysVisible)
	}
	static = make([]tool.BaseTool, 0, len(all))
	dynamic = make([]tool.BaseTool, 0, len(all))
	for _, t := range all {
		if t == nil {
			continue
		}
		info, err := t.Info(context.Background())
		name := ""
		if err == nil && info != nil {
			name = info.Name
		}
		if toolMatchesAlwaysVisible(name, nameSet) {
			static = append(static, t)
			continue
		}
		dynamic = append(dynamic, t)
	}
	if len(static) == 0 || len(dynamic) == 0 {
		// fallback: preserve previous behavior when whitelist misses all or includes all.
		return splitToolsForToolSearch(all, fallbackAlwaysVisible)
	}
	return static, dynamic, true
}

func mergeAlwaysVisibleToolNames(configured []string) []string {
	merged := make([]string, 0, len(configured)+32)
	seen := make(map[string]struct{}, len(configured)+32)
	add := func(name string) {
		n := strings.TrimSpace(strings.ToLower(name))
		if n == "" {
			return
		}
		if _, ok := seen[n]; ok {
			return
		}
		seen[n] = struct{}{}
		merged = append(merged, n)
	}
	for _, n := range configured {
		add(n)
	}
	// Always include hardcoded backend builtin MCP tools from constants.
	for _, n := range builtin.GetAllBuiltinTools() {
		add(n)
	}
	return merged
}

func reductionCacheRootDir(configuredBase, projectID, conversationID string) string {
	base := strings.TrimSpace(configuredBase)
	if base == "" {
		base = filepath.Join("tmp", "reduction")
	}
	if pid := strings.TrimSpace(projectID); pid != "" {
		return filepath.Join(base, "projects", sanitizeEinoPathSegment(pid))
	}
	conv := strings.TrimSpace(conversationID)
	if conv == "" {
		conv = "default"
	}
	return filepath.Join(base, "conversations", sanitizeEinoPathSegment(conv))
}

func buildReductionMiddleware(ctx context.Context, mw config.MultiAgentEinoMiddlewareConfig, projectID, convID string, loc *localbk.Local, modelName string, logger *zap.Logger) (adk.ChatModelAgentMiddleware, error) {
	if loc == nil {
		return nil, fmt.Errorf("reduction: local backend nil")
	}
	root := reductionCacheRootDir(mw.ReductionRootDir, projectID, convID)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("reduction root: %w", err)
	}
	excl := append([]string(nil), mw.ReductionClearExclude...)
	defaultExcl := []string{
		"task", "transfer_to_agent", "exit", "write_todos", "skill", "tool_search",
		"TaskCreate", "TaskGet", "TaskUpdate", "TaskList",
	}
	excl = append(excl, defaultExcl...)
	redMW, err := reduction.New(ctx, &reduction.Config{
		Backend:           loc,
		RootDir:           root,
		ReadFileToolName:  "read_file",
		ClearExcludeTools: excl,
		MaxLengthForTrunc: mw.ReductionMaxLengthForTruncEffective(),
		MaxTokensForClear: int64(mw.ReductionMaxTokensForClearEffective()),
		TokenCounter:      buildReductionTokenCounter(modelName),
	})
	if err != nil {
		return nil, err
	}
	if logger != nil {
		logger.Info("eino middleware: reduction enabled", zap.String("root", root))
	}
	return redMW, nil
}

// buildReductionTokenCounter 用 tiktoken 精确计数，替换 Eino 默认的 ~4字符/token 估算，
// 让 MaxTokensForClear 的清除时机基于真实 token 数而非字符数。
// 这与 einoSummarizationTokenCounter 使用同一底层计数器，保证压缩触发与清除阈值口径一致。
func buildReductionTokenCounter(modelName string) func(context.Context, []adk.Message, []*schema.ToolInfo) (int64, error) {
	tc := agent.NewTikTokenCounter()
	return func(_ context.Context, msgs []adk.Message, tools []*schema.ToolInfo) (int64, error) {
		// We build a representative string from all roles/content/tool data.
		var sb strings.Builder
		for _, msg := range msgs {
			if msg == nil {
				continue
			}
			sb.WriteString(string(msg.Role))
			sb.WriteByte('\n')
			sb.WriteString(msg.Content)
			sb.WriteByte('\n')
			if len(msg.ToolCalls) > 0 {
				if b, err := json.Marshal(msg.ToolCalls); err == nil {
					sb.Write(b)
					sb.WriteByte('\n')
				}
			}
		}
		for _, tl := range tools {
			if tl == nil {
				continue
			}
			if b, err := json.Marshal(tl); err == nil {
				sb.Write(b)
				sb.WriteByte('\n')
			}
		}
		text := sb.String()
		n, err := tc.Count(modelName, text)
		if err != nil {
			// tiktoken 失败（未知模型等）：回退到 (字节数+3)/4 估算。
			return int64((len(text) + 3) / 4), nil
		}
		return int64(n), nil
	}
}


// prependEinoMiddlewares returns handlers to prepend (outermost first) and optionally replaces tools when tool_search is used.
// toolSearchActive is true when the toolsearch middleware was mounted (dynamic tools split off); callers should pass this to
// injectToolNamesOnlyInstruction — tool_search is not part of the pre-middleware tools list, so name-scanning alone cannot detect it.
func prependEinoMiddlewares(
	ctx context.Context,
	mw *config.MultiAgentEinoMiddlewareConfig,
	place einoMWPlacement,
	tools []tool.BaseTool,
	einoLoc *localbk.Local,
	skillsRoot string,
	conversationID string,
	projectID string,
	modelName string,
	logger *zap.Logger,
) (outTools []tool.BaseTool, extraHandlers []adk.ChatModelAgentMiddleware, toolSearchActive bool, err error) {
	if mw == nil {
		return tools, nil, false, nil
	}
	outTools = tools

	if mw.PatchToolCallsEffective() {
		patchMW, perr := patchtoolcalls.New(ctx, &patchtoolcalls.Config{})
		if perr != nil {
			return nil, nil, false, fmt.Errorf("patchtoolcalls: %w", perr)
		}
		extraHandlers = append(extraHandlers, patchMW)
	}

	if mw.ReductionEnable && einoLoc != nil {
		if place == einoMWSub && !mw.ReductionSubAgents {
			// skip
		} else {
			redMW, rerr := buildReductionMiddleware(ctx, *mw, projectID, conversationID, einoLoc, modelName, logger)
			if rerr != nil {
				return nil, nil, false, rerr
			}
			extraHandlers = append(extraHandlers, redMW)
		}
	}

	minTools := mw.ToolSearchMinTools
	if minTools <= 0 {
		minTools = 20
	}
	alwaysVis := mw.ToolSearchAlwaysVisible
	if alwaysVis <= 0 {
		alwaysVis = 12
	}
	if mw.ToolSearchEnable && len(tools) >= minTools {
		static, dynamic, split := splitToolsForToolSearchByNames(tools, mergeAlwaysVisibleToolNames(mw.ToolSearchAlwaysVisibleTools), alwaysVis)
		if split && len(dynamic) > 0 {
			ts, terr := toolsearch.New(ctx, &toolsearch.Config{DynamicTools: dynamic})
			if terr != nil {
				return nil, nil, false, fmt.Errorf("toolsearch: %w", terr)
			}
			extraHandlers = append(extraHandlers, ts)
			outTools = static
			toolSearchActive = true
			if logger != nil {
				logger.Info("eino middleware: tool_search enabled",
					zap.Int("static_tools", len(static)),
					zap.Int("dynamic_tools", len(dynamic)))
			}
		}
	}

	if place == einoMWMain && mw.PlantaskEnable {
		if einoLoc == nil || strings.TrimSpace(skillsRoot) == "" {
			if logger != nil {
				logger.Warn("eino middleware: plantask_enable ignored (need eino_skills + skills_dir)")
			}
		} else {
			rel := strings.TrimSpace(mw.PlantaskRelDir)
			if rel == "" {
				rel = ".eino/plantask"
			}
			baseDir := filepath.Join(skillsRoot, rel, sanitizeEinoPathSegment(conversationID))
			if mk := os.MkdirAll(baseDir, 0o755); mk != nil {
				return nil, nil, toolSearchActive, fmt.Errorf("plantask mkdir: %w", mk)
			}
			ptBE := newLocalPlantaskBackend(einoLoc)
			pt, perr := plantask.New(ctx, &plantask.Config{Backend: ptBE, BaseDir: baseDir})
			if perr != nil {
				return nil, nil, toolSearchActive, fmt.Errorf("plantask: %w", perr)
			}
			extraHandlers = append(extraHandlers, pt)
			if logger != nil {
				logger.Info("eino middleware: plantask enabled", zap.String("baseDir", baseDir))
			}
		}
	}

	return outTools, extraHandlers, toolSearchActive, nil
}

func deepExtrasFromConfig(ma *config.MultiAgentConfig) (outputKey string, retry *adk.ModelRetryConfig, taskDesc func(context.Context, []adk.Agent) (string, error)) {
	if ma == nil {
		return "", nil, nil
	}
	mw := ma.EinoMiddleware
	if k := strings.TrimSpace(mw.DeepOutputKey); k != "" {
		outputKey = k
	}
	if mw.DeepModelRetryMaxRetries > 0 {
		retry = &adk.ModelRetryConfig{MaxRetries: mw.DeepModelRetryMaxRetries}
	}
	prefix := strings.TrimSpace(mw.TaskToolDescriptionPrefix)
	if prefix != "" {
		taskDesc = func(ctx context.Context, agents []adk.Agent) (string, error) {
			_ = ctx
			var names []string
			for _, a := range agents {
				if a == nil {
					continue
				}
				n := strings.TrimSpace(a.Name(ctx))
				if n != "" {
					names = append(names, n)
				}
			}
			if len(names) == 0 {
				return prefix, nil
			}
			return prefix + "\n可用子代理（按名称 transfer / task 调用）：" + strings.Join(names, "、"), nil
		}
	}
	return outputKey, retry, taskDesc
}
