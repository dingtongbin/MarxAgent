// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfigFile(t *testing.T, path string, config Config) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create directory: %v", err)
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func openAtTemp(t *testing.T) (*Store, string, string) {
	t.Helper()
	directory := t.TempDir()
	globalPath := filepath.Join(directory, "global", FileName)
	projectPath := filepath.Join(directory, "project", FileName)
	store, err := openAt(globalPath, projectPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return store, globalPath, projectPath
}

func TestAMachineWithNoFilesHasNoModelRatherThanAFailure(t *testing.T) {
	store, _, _ := openAtTemp(t)

	if got := store.Current().Model.Default; got != "" {
		t.Fatalf("default model = %q, want empty", got)
	}
	if names := store.ProviderNames(); len(names) != 0 {
		t.Fatalf("providers = %v, want none", names)
	}
	if _, err := store.Resolve(""); err == nil {
		t.Fatal("resolving nothing was allowed on a machine with no model configured")
	}
}

func TestTheProjectFileReplacesAProviderRatherThanMergingIntoIt(t *testing.T) {
	directory := t.TempDir()
	globalPath := filepath.Join(directory, FileName)
	projectPath := filepath.Join(directory, "project", FileName)

	writeConfigFile(t, globalPath, Config{
		Version: Version,
		Model: ModelConfig{
			Default: "anthropic:claude-sonnet-4-5",
			Providers: map[string]Provider{
				"anthropic": {
					Endpoint:  "https://api.anthropic.com",
					Format:    "anthropic-messages",
					APIKeyEnv: "ANTHROPIC_API_KEY",
					Models:    []Model{{ID: "claude-sonnet-4-5"}},
				},
				"ollama": {
					Endpoint: "http://localhost:11434",
					Format:   "openai-completions",
				},
			},
		},
	})
	writeConfigFile(t, projectPath, Config{
		Version: Version,
		Model: ModelConfig{
			Default: "ollama:qwen3",
			Providers: map[string]Provider{
				"ollama": {Endpoint: "http://127.0.0.1:11434", Format: "openai-completions"},
			},
		},
	})

	store, err := openAt(globalPath, projectPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	current := store.Current()
	if current.Model.Default != "ollama:qwen3" {
		t.Fatalf("default = %q, want the project's", current.Model.Default)
	}
	ollama := current.Model.Providers["ollama"]
	if ollama.Endpoint != "http://127.0.0.1:11434" {
		t.Fatalf("endpoint = %q, want the project's", ollama.Endpoint)
	}
	// The global one had models listed and the project one did not. A field by
	// field merge would have kept them, and a project that replaced a provider
	// would then be unable to remove a model from it.
	if len(ollama.Models) != 0 {
		t.Fatalf("models = %v, want the project's empty list rather than the global's", ollama.Models)
	}
	// A provider the project says nothing about is still there.
	if _, exists := current.Model.Providers["anthropic"]; !exists {
		t.Fatal("the global provider was dropped by a project that did not mention it")
	}
	if names := store.ProviderNames(); strings.Join(names, ",") != "anthropic,ollama" {
		t.Fatalf("provider names = %v, want them in a stable order", names)
	}
}

func TestResolveNamesTheProviderAndTheModel(t *testing.T) {
	store, globalPath, _ := openAtTemp(t)
	writeConfigFile(t, globalPath, Config{
		Version: Version,
		Model: ModelConfig{
			Default: "anthropic:claude-sonnet-4-5",
			Providers: map[string]Provider{
				"anthropic": {Endpoint: "https://api.anthropic.com", Format: "anthropic-messages"},
				"ollama":    {Endpoint: "http://localhost:11434", Format: "openai-completions"},
			},
		},
	})
	store, err := openAt(globalPath, store.projectPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	reference, err := store.Resolve("")
	if err != nil {
		t.Fatalf("resolve the default: %v", err)
	}
	if reference.Spec() != "anthropic:claude-sonnet-4-5" {
		t.Fatalf("reference = %q, want the configured default", reference.Spec())
	}

	reference, err = store.Resolve("ollama:qwen3")
	if err != nil {
		t.Fatalf("resolve an explicit model: %v", err)
	}
	if reference.Provider != "ollama" || reference.Model != "qwen3" {
		t.Fatalf("reference = %#v, want ollama/qwen3", reference)
	}
}

func TestResolveRefusesAModelNamedByAProviderThatDoesNotExist(t *testing.T) {
	store, globalPath, _ := openAtTemp(t)
	writeConfigFile(t, globalPath, Config{
		Version: Version,
		Model: ModelConfig{
			Default: "anthropic:claude-sonnet-4-5",
			Providers: map[string]Provider{
				"anthropic": {Endpoint: "https://api.anthropic.com", Format: "anthropic-messages"},
				"ollama":    {Endpoint: "http://localhost:11434", Format: "openai-completions"},
			},
		},
	})
	store, err := openAt(globalPath, store.projectPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Two providers are configured, so a bare model name is ambiguous rather than
	// convenient.
	if _, err := store.Resolve("qwen3"); err == nil {
		t.Fatal("a bare model name was resolved against two configured providers")
	}
	if _, err := store.Resolve("nope:qwen3"); err == nil {
		t.Fatal("a model was resolved against a provider that is not configured")
	}
	if _, err := store.Resolve("ollama:"); err == nil {
		t.Fatal("a provider with no model was resolved")
	}
}

func TestASingleProviderMakesABareModelNameUnambiguous(t *testing.T) {
	store, globalPath, _ := openAtTemp(t)
	writeConfigFile(t, globalPath, Config{
		Version: Version,
		Model: ModelConfig{
			Default: "ollama:qwen3",
			Providers: map[string]Provider{
				"ollama": {Endpoint: "http://localhost:11434", Format: "openai-completions"},
			},
		},
	})
	store, err := openAt(globalPath, store.projectPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	reference, err := store.Resolve("qwen3:8b")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// The whole thing is the model name, colon and all, because the only
	// provider is "ollama" and the specification does not start with it.
	if reference.Spec() != "ollama:qwen3:8b" {
		t.Fatalf("reference = %q, want the single provider and the whole model name", reference.Spec())
	}

	// And a qualified one is split at the provider, keeping a colon in the model
	// where one is there.
	reference, err = store.Resolve("ollama:qwen3:8b")
	if err != nil {
		t.Fatalf("resolve a qualified model: %v", err)
	}
	if reference.Provider != "ollama" || reference.Model != "qwen3:8b" {
		t.Fatalf("reference = %#v, want the model name kept whole", reference)
	}
}

func TestReplaceWritesTheGlobalFileAndIsReadBack(t *testing.T) {
	store, globalPath, _ := openAtTemp(t)

	next := Config{
		Version: Version,
		Model: ModelConfig{
			Default: "ollama:qwen3",
			Providers: map[string]Provider{
				"ollama": {Endpoint: "http://127.0.0.1:11434", Format: "openai-completions"},
			},
		},
	}
	if err := store.Replace(next); err != nil {
		t.Fatalf("replace: %v", err)
	}

	// A fresh store is what the next start of the program will build, so it is
	// the only honest check that the file is where the file is supposed to be.
	reread, err := openAt(globalPath, store.projectPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reread.Current().Model.Default != "ollama:qwen3" {
		t.Fatalf("default = %q after reopening", reread.Current().Model.Default)
	}
	if _, exists := reread.Provider("ollama"); !exists {
		t.Fatal("the provider written by Replace was not read back")
	}
}

func TestAFileThatCannotBeParsedIsRefusedRatherThanIgnored(t *testing.T) {
	directory := t.TempDir()
	globalPath := filepath.Join(directory, FileName)
	if err := os.WriteFile(globalPath, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := openAt(globalPath, filepath.Join(directory, "project", FileName)); err == nil {
		t.Fatal("a configuration file that cannot be parsed was ignored")
	}
}

func TestThePageCanSeeTheKeySoItCanBeFixed(t *testing.T) {
	store, globalPath, projectPath := openAtTemp(t)
	writeConfigFile(t, projectPath, Config{Version: Version})
	writeConfigFile(t, globalPath, Config{Version: Version})
	encoded := `{"model":{"default":"anthropic:claude-sonnet-4-5","providers":{
		"anthropic":{
			"endpoint":"https://api.anthropic.com",
			"format":"anthropic-messages",
			"api_key":"sk-secret-value"
		}
	}}}`
	if err := os.WriteFile(globalPath, []byte(encoded), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	store, err := openAt(globalPath, projectPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	view := store.View()
	rendered := view.Model.Providers["anthropic"]
	// The key is on this machine, in a file this person can open, and the answer
	// to the only question they came here with — is the key right — cannot be
	// answered by a field that shows nothing.
	if rendered.APIKey != "sk-secret-value" {
		t.Fatalf("the page was shown %q for the key, want the one that is stored", rendered.APIKey)
	}
	if !rendered.APIKeySet {
		t.Fatal("the page was not told a key is held")
	}
	if view.GlobalPath != globalPath {
		t.Fatalf("global path = %q, want %q", view.GlobalPath, globalPath)
	}
	if view.ProjectPath != projectPath {
		t.Fatalf("project path = %q, want %q", view.ProjectPath, projectPath)
	}
	if len(view.Formats) != 3 {
		t.Fatalf("formats = %v, want the three formats", view.Formats)
	}

	// A save that does not mention the key at all keeps it, so a client that
	// cannot see it is not the way to clear it.
	var update Update
	if err := json.Unmarshal([]byte(`{
		"model": {
			"default": "anthropic:claude-opus-4-1",
			"providers": {
				"anthropic": {
					"endpoint": "https://api.anthropic.com",
					"format": "anthropic-messages",
					"models": [{"id": "claude-opus-4-1"}, {"id": "claude-sonnet-4-5"}]
				}
			}
		}
	}`), &update); err != nil {
		t.Fatalf("decode the update: %v", err)
	}
	if err := store.Apply(update); err != nil {
		t.Fatalf("apply: %v", err)
	}
	provider, _ := store.Provider("anthropic")
	if provider.APIKey != "sk-secret-value" {
		t.Fatalf("key = %q, want the one the update did not mention to be kept", provider.APIKey)
	}
	if len(provider.Models) != 2 {
		t.Fatalf("models = %v, want the two that were sent", provider.Models)
	}

	// And an empty key sent on purpose clears it, because a field emptied is a key
	// removed rather than a question.
	clear := `{"model":{"default":"anthropic:claude-opus-4-1","providers":{
		"anthropic":{"endpoint":"https://api.anthropic.com","format":"anthropic-messages",
			"api_key":""}}}}`
	if err := json.Unmarshal([]byte(clear), &update); err != nil {
		t.Fatalf("decode the clearing update: %v", err)
	}
	if err := store.Apply(update); err != nil {
		t.Fatalf("apply the clearing update: %v", err)
	}
	if provider, _ := store.Provider("anthropic"); provider.APIKey != "" {
		t.Fatalf("key = %q, want it cleared", provider.APIKey)
	}
	if store.View().Model.Providers["anthropic"].APIKeySet {
		t.Fatal("the view still claims a key is held")
	}
}

func TestClearingAKeyCanAlsoBeSaidOutLoud(t *testing.T) {
	// Some clients cannot send an empty string meaning "none", so the removal is
	// also expressible on its own.
	store, globalPath, projectPath := openAtTemp(t)
	writeConfigFile(t, globalPath, Config{
		Version: Version,
		Model: ModelConfig{
			Default: "a:m",
			Providers: map[string]Provider{
				"a": {Endpoint: "https://x", Format: "openai-completions", APIKey: "k"},
			},
		},
	})
	store, err := openAt(globalPath, projectPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	var update Update
	encoded := `{"model":{"default":"a:m","providers":{
		"a":{"endpoint":"https://x","format":"openai-completions","clear_api_key":true}}}}`
	if err := json.Unmarshal([]byte(encoded), &update); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := store.Apply(update); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if provider, _ := store.Provider("a"); provider.APIKey != "" {
		t.Fatalf("key = %q, want it cleared", provider.APIKey)
	}
}

func TestApplySetsAKeyThatWasSent(t *testing.T) {
	store, _, _ := openAtTemp(t)
	encoded := `{"model":{"default":"ollama:qwen3","providers":{
		"ollama":{"endpoint":"http://127.0.0.1:11434","format":"openai-completions",
			"api_key":"local-key"}}}}`
	var update Update
	if err := json.Unmarshal([]byte(encoded), &update); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := store.Apply(update); err != nil {
		t.Fatalf("apply: %v", err)
	}
	provider, _ := store.Provider("ollama")
	if provider.APIKey != "local-key" {
		t.Fatalf("key = %q, want the one that was sent", provider.APIKey)
	}
}

func TestApplyRefusesAnUpdateThatWouldLeaveTheDefaultPointingAtNothing(t *testing.T) {
	store, _, _ := openAtTemp(t)
	encoded := `{"model":{"default":"ollama:qwen3","providers":{}}}`
	var update Update
	if err := json.Unmarshal([]byte(encoded), &update); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := store.Apply(update); err == nil {
		t.Fatal("a default model with no providers behind it was accepted")
	}
}

func TestApplyRefusesAnUpdateThatNamesNoProvidersAtAll(t *testing.T) {
	store, globalPath, projectPath := openAtTemp(t)
	writeConfigFile(t, globalPath, Config{
		Version: Version,
		Model: ModelConfig{
			Default: "ollama:qwen3",
			Providers: map[string]Provider{
				"ollama": {Endpoint: "http://127.0.0.1:11434", Format: "openai-completions"},
			},
		},
	})
	store, err := openAt(globalPath, projectPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// A page that failed to parse its own answer would send an absent map rather
	// than an empty one. Deleting every provider because of it would be worse than
	// refusing.
	var update Update
	if err := json.Unmarshal([]byte(`{"model":{"default":"ollama:qwen3"}}`), &update); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := store.Apply(update); err == nil {
		t.Fatal("an update with no providers was accepted")
	}
	if _, exists := store.Provider("ollama"); !exists {
		t.Fatal("the provider was removed by a refused update")
	}
}

func TestGatewayBuildsFromAProvider(t *testing.T) {
	store, globalPath, projectPath := openAtTemp(t)
	writeConfigFile(t, globalPath, Config{
		Version: Version,
		Model: ModelConfig{
			Default: "ollama:qwen3",
			Providers: map[string]Provider{
				"ollama": {
					Endpoint: "http://127.0.0.1:11434",
					Format:   "openai-completions",
					Thinking: &ThinkingConfig{Enabled: false},
				},
			},
		},
	})
	store, err := openAt(globalPath, projectPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	reference, err := store.Resolve("")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	built, err := store.Gateway(reference)
	if err != nil {
		t.Fatalf("build the gateway: %v", err)
	}
	if built.Format() != "openai-completions" {
		t.Fatalf("format = %q", built.Format())
	}
	if _, err := store.Gateway(ModelRef{Provider: "nope", Model: "x"}); err == nil {
		t.Fatal("a gateway was built for a provider that is not configured")
	}
}

func TestValidateRefusesWhatWouldFailLater(t *testing.T) {
	cases := []struct {
		name   string
		config Config
	}{
		{"no endpoint", Config{Version: Version, Model: ModelConfig{
			Providers: map[string]Provider{"a": {Format: "openai-completions"}}}}},
		{"no format", Config{Version: Version, Model: ModelConfig{
			Providers: map[string]Provider{"a": {Endpoint: "http://x"}}}}},
		{"unknown format", Config{Version: Version, Model: ModelConfig{
			Providers: map[string]Provider{"a": {Endpoint: "http://x", Format: "grpc"}}}}},
		{"a newer schema", Config{Version: Version + 1}},
		{"a default with no model", Config{Version: Version, Model: ModelConfig{
			Default:   "ollama",
			Providers: map[string]Provider{"ollama": {Endpoint: "http://x", Format: "openai-completions"}}}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := Validate(testCase.config); err == nil {
				t.Fatalf("%+v was accepted", testCase.config)
			}
		})
	}
}

func TestAModelSaysWhatIsKnownAboutItAndDefaultsStandInForWhatIsNot(t *testing.T) {
	store, globalPath, projectPath := openAtTemp(t)
	encoded := `{
		"version": 1,
		"model": {
			"default": "local:big",
			"providers": {
				"local": {
					"endpoint": "http://127.0.0.1:11434/v1",
					"format": "openai-completions",
					"models": [
						{"id": "small"},
						{"id": "big", "context_window": 200000, "max_output": 64000,
						 "vision": true, "thinking": ["off", "high"], "disabled": true}
					]
				}
			}
		}
	}`
	if err := os.MkdirAll(filepath.Dir(globalPath), 0o755); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.WriteFile(globalPath, []byte(encoded), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	store, err := openAt(globalPath, projectPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	provider, exists := store.Provider("local")
	if !exists {
		t.Fatal("the provider did not load")
	}
	small, _ := provider.FindModel("small")
	if small.ContextWindowOrDefault() != DefaultContextWindow {
		t.Fatalf("a model that declared no window has %d", small.ContextWindowOrDefault())
	}
	if small.MaxOutputOrDefault() != DefaultMaxOutput {
		t.Fatalf("a model that declared no output limit has %d", small.MaxOutputOrDefault())
	}
	if small.Vision {
		t.Fatal("a model that said nothing about images was given images")
	}
	big, _ := provider.FindModel("big")
	if big.ContextWindowOrDefault() != 200000 || big.MaxOutputOrDefault() != 64000 {
		t.Fatalf("a model that declared its limits came back as %+v", big)
	}
	if !big.Vision || !big.Disabled || len(big.Thinking) != 2 {
		t.Fatalf("a model that declared its abilities came back as %+v", big)
	}

	// The page is shown the assumed figures rather than the zeros, and is told
	// which of them were declared, because "not declared" and "this number" are
	// different answers and only one of them is one.
	view := store.View().Model.Providers["local"]
	if view.Models[0].ContextWindow != DefaultContextWindow || view.Models[0].DeclaredWindow {
		t.Fatalf("an undeclared window was shown as %+v", view.Models[0])
	}
	if !view.Models[1].DeclaredWindow || view.Models[1].ContextWindow != 200000 {
		t.Fatalf("a declared window was shown as %+v", view.Models[1])
	}
	if len(view.Models[1].Thinking) != 2 || view.Models[1].Thinking[1] != "high" {
		t.Fatalf("thinking levels = %v", view.Models[1].Thinking)
	}
}

func TestAModelThatDeclaresNothingAsItsWindowIsRefused(t *testing.T) {
	store, _, _ := openAtTemp(t)
	// Zero with the flag set says the field was filled in and the answer was
	// nothing, which is a contradiction rather than a fact.
	encoded := `{"model":{"default":"local:m","providers":{
		"local":{"endpoint":"http://x","format":"openai-completions",
			"models":[{"id":"m","declared_window":true,"context_window":0}]}}}}`
	var update Update
	if err := json.Unmarshal([]byte(encoded), &update); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := store.Apply(update); err == nil {
		t.Fatal("a declared window of nothing was accepted")
	}
}

func TestAModelWithAnUnknownReasoningLevelIsRefused(t *testing.T) {
	store, _, _ := openAtTemp(t)
	encoded := `{"model":{"default":"local:m","providers":{
		"local":{"endpoint":"http://x","format":"openai-completions",
			"models":[{"id":"m","thinking":["loud"]}]}}}}`
	var update Update
	if err := json.Unmarshal([]byte(encoded), &update); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := store.Apply(update); err == nil {
		t.Fatal("a reasoning level that does not exist was accepted")
	}
}

func TestAModelListedTwiceIsRefused(t *testing.T) {
	store, _, _ := openAtTemp(t)
	encoded := `{"model":{"default":"local:m","providers":{
		"local":{"endpoint":"http://x","format":"openai-completions",
			"models":[{"id":"m"},{"id":"m"}]}}}}`
	var update Update
	if err := json.Unmarshal([]byte(encoded), &update); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := store.Apply(update); err == nil {
		t.Fatal("the same model was accepted twice")
	}
}

func TestTheDefaultModelMayBeLeftEmptyWhileProvidersExist(t *testing.T) {
	// A person saving a provider they have not chosen a model for yet is not
	// making a mistake, and refusing their save would be a page that cannot be
	// filled in one field at a time.
	store, _, _ := openAtTemp(t)
	encoded := `{"model":{"default":"","providers":{
		"local":{"endpoint":"http://x","format":"openai-completions","models":[{"id":"m"}]}}}}`
	var update Update
	if err := json.Unmarshal([]byte(encoded), &update); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := store.Apply(update); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, exists := store.Provider("local"); !exists {
		t.Fatal("the provider was not kept")
	}
}

func TestEveryPresetIsOneSomebodyCanActuallyUse(t *testing.T) {
	seen := make(map[string]struct{})
	for _, preset := range Presets() {
		if preset.Name == "" || preset.Slug == "" {
			t.Fatalf("preset %#v has no name to show or no name to store it under", preset)
		}
		if _, duplicate := seen[preset.Slug]; duplicate {
			t.Fatalf("two presets share the slug %q", preset.Slug)
		}
		seen[preset.Slug] = struct{}{}
		provider := ProviderFromPreset(preset)
		if err := validateProvider(preset.Slug, provider); err != nil {
			t.Fatalf("preset %q would not save: %v", preset.Name, err)
		}
		// A preset that carried a key would be a key in a repository.
		if provider.APIKey != "" {
			t.Fatalf("preset %q carries a key", preset.Name)
		}
		if !provider.Enabled {
			t.Fatalf("preset %q is switched off by default", preset.Name)
		}
		// A preset that listed models would be a second source of truth about
		// somebody else's service, and a stale one in that list is a name the
		// endpoint refuses. The models come from the endpoint when they are asked
		// for.
		if len(provider.Models) != 0 {
			t.Fatalf("preset %q brings %d models with it, want none", preset.Name, len(provider.Models))
		}
	}
}

func TestAPresetBecomesAProviderThatCanBeSaved(t *testing.T) {
	store, _, _ := openAtTemp(t)
	var update Update
	encoded, err := json.Marshal(map[string]any{"model": map[string]any{
		"default": "ollama:qwen3",
		"providers": map[string]any{
			"ollama": ProviderFromPreset(Preset{Slug: "ollama", Name: "Ollama",
				Endpoint: "http://127.0.0.1:11434/v1", Format: "openai-completions"}),
		},
	}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// A preset becomes a provider the same way a typed one does, so a preset is a
	// shortcut rather than a second kind of thing.
	if err := json.Unmarshal(encoded, &update); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := store.Apply(update); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if store.DisplayName("ollama") != "Ollama" {
		t.Fatalf("display name = %q", store.DisplayName("ollama"))
	}
	provider, _ := store.Provider("ollama")
	if len(provider.Models) != 0 {
		t.Fatalf("the preset brought %d models with it, want none", len(provider.Models))
	}
}

func TestFormatsCarryThePathTheirRequestsGoTo(t *testing.T) {
	// A person with a gateway in front of a model service is trying to work out
	// which format it speaks, and the address is what they have.
	for _, format := range Formats {
		if FormatEndpoint(format) == "" {
			t.Fatalf("format %q has no path to show", format)
		}
		if FormatLabel(format) == format {
			t.Fatalf("format %q is shown to a person as its own wire name", format)
		}
	}
}

func TestAValidConfigurationPassesValidation(t *testing.T) {
	valid := Config{
		Version: Version,
		Model: ModelConfig{
			Default: "anthropic:claude-sonnet-4-5",
			Providers: map[string]Provider{
				"anthropic": {
					Endpoint:  "https://api.anthropic.com",
					Format:    "anthropic-messages",
					APIKeyEnv: "ANTHROPIC_API_KEY",
					Thinking:  &ThinkingConfig{Enabled: true, BudgetTokens: 8000},
				},
			},
		},
	}
	if err := Validate(valid); err != nil {
		t.Fatalf("validate: %v", err)
	}
}
