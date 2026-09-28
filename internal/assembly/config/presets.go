// SPDX-License-Identifier: Apache-2.0

package config

import "github.com/dingtongbin/MarxAgent/internal/engine/gateway"

// A preset is a provider somebody else has already written down: where it is and
// what it speaks.
//
// It carries no models. The list a preset gave out was a list somebody typed once
// and never checked, and a stale model in a list is worse than an empty one: it
// offers a name the endpoint will refuse, and somebody picks it and then wonders
// why the conversation fails. The models come from the endpoint when they are
// asked for, which is the only list that is true.
//
// It carries no key either, for the same kind of reason: a preset with a
// credential in it is a credential in a repository.
type Preset struct {
	// Name is what a person reads in the menu.
	Name string `json:"name"`
	// Slug is the provider name a preset creates, and is not shown: a person adding
	// two presets wants two providers, and the names are how they are told apart.
	Slug      string `json:"slug"`
	Endpoint  string `json:"endpoint"`
	Format    string `json:"format"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
}

// Presets is the catalog of providers this build knows about.
//
// It is deliberately short. Every entry here is a claim about somebody else's
// service, and a claim that goes stale is worse than no claim: a menu that offers
// a preset which no longer works is a menu that sends people to a dead endpoint.
// The list is the ones that need no account to reach, and the interface can always
// add a provider by hand.
func Presets() []Preset {
	return []Preset{
		{
			Name: "Ollama（本地）", Slug: "ollama",
			Endpoint: "http://127.0.0.1:11434/v1", Format: string(gateway.FormatOpenAICompletions),
		},
		{
			Name: "DeepSeek", Slug: "deepseek",
			Endpoint: "https://api.deepseek.com/v1", Format: string(gateway.FormatOpenAICompletions),
			APIKeyEnv: "DEEPSEEK_API_KEY",
		},
		{
			Name: "通义千问（DashScope）", Slug: "dashscope",
			Endpoint: "https://dashscope.aliyuncs.com/compatible-mode/v1", Format: string(gateway.FormatOpenAICompletions),
			APIKeyEnv: "DASHSCOPE_API_KEY",
		},
		{
			Name: "智谱 GLM", Slug: "zhipu",
			Endpoint: "https://open.bigmodel.cn/api/paas/v4", Format: string(gateway.FormatOpenAICompletions),
			APIKeyEnv: "ZHIPU_API_KEY",
		},
		{
			Name: "月之暗面 Kimi", Slug: "kimi",
			Endpoint: "https://api.moonshot.cn/v1", Format: string(gateway.FormatOpenAICompletions),
			APIKeyEnv: "MOONSHOT_API_KEY",
		},
		{
			Name: "本地 vLLM", Slug: "vllm",
			Endpoint: "http://127.0.0.1:8000/v1", Format: string(gateway.FormatOpenAICompletions),
		},
	}
}

// ProviderFromPreset turns a preset into a provider that can be saved.
//
// The model list is left empty on purpose: the models come from the endpoint when
// somebody asks, and a preset that guessed at them would be a second source of
// truth about somebody else's service.
func ProviderFromPreset(preset Preset) Provider {
	return Provider{
		Name:      preset.Name,
		Enabled:   true,
		Endpoint:  preset.Endpoint,
		Format:    preset.Format,
		APIKeyEnv: preset.APIKeyEnv,
	}
}
