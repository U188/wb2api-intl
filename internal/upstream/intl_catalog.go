package upstream

import "github.com/linguo2625469/workbuddy2api-panel/internal/auth"

// intlBuiltinModels follows the official international CodeBuddy CLI catalog
// (@tencent-ai/codebuddy-code 2.151.0 product.json, agents[name=cli].models)
// and includes the current international DeepSeek V4.1 Flash rollout. The
// website has no usable CN-style model endpoint, so discovery stays local.
var intlBuiltinModels = []ModelInfo{
	intlModel("default-model", "Auto", 176_000, 24_000, 80_000, "x0.79 credits", nil, "", false, true),
	intlModel("fast-model", "Fast", 200_000, 32_000, 200_000, "x0.34 credits", nil, "medium", false, true),
	intlModel("balanced-model", "Balanced", 256_000, 32_000, 256_000, "x0.59 credits", nil, "medium", false, true),
	intlModel("primary-model", "Primary", 272_000, 72_000, 272_000, "x3.31 credits", nil, "high", false, true),
	intlModel("deep-model", "Deep", 176_000, 24_000, 200_000, "x3.33 credits", nil, "", false, false),
	intlModel("gpt-5.6-sol", "GPT-5.6-Sol", 1_000_000, 128_000, 1_000_000, "x3.47 credits", []string{"low", "medium", "high", "xhigh"}, "high", false, true),
	intlModel("gpt-5.6-terra", "GPT-5.6-Terra", 1_000_000, 128_000, 1_000_000, "x1.39 credits", []string{"low", "medium", "high", "xhigh"}, "high", false, true),
	intlModel("gpt-5.6-luna", "GPT-5.6-Luna", 1_000_000, 128_000, 1_000_000, "x0.14 credits", []string{"low", "medium", "high", "xhigh"}, "high", false, true),
	intlModel("gpt-5.5", "GPT-5.5", 1_000_000, 72_000, 1_000_000, "x3.31 credits", nil, "high", false, true),
	intlModel("gpt-5.4", "GPT-5.4", 272_000, 128_000, 0, "x1.65 credits", nil, "high", false, true),
	intlModel("gpt-5.3-codex", "GPT-5.3-Codex", 272_000, 128_000, 0, "x1.25 credits", nil, "high", false, true),
	intlModel("gemini-3.5-flash", "Gemini-3.5-Flash", 1_000_000, 65_536, 1_000_000, "x0.99 credits", nil, "medium", false, true),
	// DeepSeek 官方目录对 V4.1 Flash 标注输出上限 384K（models.dev 上 deepseek
	// 官方 provider 值，且 384000 是 28 家 provider 的共识档）。腾讯 CLI
	// product.json 的 50K 只属于 CN 侧 deepseek-v4-flash，两者不是同一部署，
	// 禁止互相套用；该字段仅用于 /v1/models 与面板展示，不参与出站改写。
	intlModel("deepseek-v4.1-flash", "DeepSeek-V4.1-Flash", 1_000_000, 384_000, 1_000_000, "x0.06 credits", nil, "high", false, true),
	intlModel("glm-5.3", "GLM-5.3", 1_000_000, 48_000, 1_000_000, "x0.79 credits", []string{"low", "high", "max"}, "high", true, true),
	intlModel("glm-5.2", "GLM-5.2", 1_000_000, 48_000, 1_000_000, "x0.79 credits", []string{"high", "xhigh"}, "high", true, true),
	intlModel("kimi-k3", "Kimi-K3", 1_000_000, 32_000, 1_000_000, "x1.62 credits", nil, "medium", false, true),
	intlModel("kimi-k2.6", "Kimi-K2.6", 256_000, 32_000, 256_000, "x0.52 credits", nil, "medium", false, true),
	intlModel("minimax-m3", "MiniMax-M3", 512_000, 128_000, 512_000, "x0.25 credits", nil, "medium", false, true),
}

func intlModel(id, name string, input, output, allowed int64, credits string, efforts []string, defaultEffort string, canDisable, supportsReasoning bool) ModelInfo {
	m := ModelInfo{
		ID:                 id,
		Name:               name,
		ContextWindow:      input,
		MaxTokens:          output,
		MaxAllowedSize:     allowed,
		Efforts:            efforts,
		DefaultEffort:      defaultEffort,
		CanDisableThinking: canDisable,
		SupportsReasoning:  supportsReasoning,
		Credits:            credits,
	}
	// 物理 1M 模型保留 canonical 200K / 1M 会话预算选择；运行时开关只在
	// 输出模型元数据时选择默认档，不改目录，也不改写上游 max_tokens。
	if input >= 1_000_000 && allowed >= 1_000_000 {
		m.ContextLengths = []int64{200_000, 1_000_000}
		m.DefaultContextLength = 200_000
	}
	return m
}

// IntlBuiltinModels returns an independent copy. Callers may safely modify the
// returned slice and nested capability lists.
func IntlBuiltinModels() []ModelInfo {
	out := make([]ModelInfo, len(intlBuiltinModels))
	for i, model := range intlBuiltinModels {
		out[i] = model
		out[i].ContextLengths = append([]int64(nil), model.ContextLengths...)
		out[i].Efforts = append([]string(nil), model.Efforts...)
	}
	return out
}

// refreshEfforts replaces one site's capability cache without sharing maps or
// slices with the other site.
func (c *Client) refreshEfforts(site string, models []ModelInfo) {
	cache := make(map[string][]string, len(models))
	for _, model := range models {
		if len(model.Efforts) > 0 {
			cache[model.ID] = append([]string(nil), model.Efforts...)
		}
	}
	c.effortsMu.Lock()
	if c.efforts == nil {
		c.efforts = make(map[string]map[string][]string, 2)
	}
	c.efforts[auth.SiteFrom(site)] = cache
	c.effortsMu.Unlock()
}
