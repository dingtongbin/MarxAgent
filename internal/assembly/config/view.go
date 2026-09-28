// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// The shapes the settings page reads and writes.
//
// They are not the file's shapes, and the difference is deliberate. A file holds
// an API key for a provider declared inline, and a page that had the key in it
// would be a page that has to be trusted with the key: it would be in the
// browser, in the devtools of every window that had the settings open, and in
// any screenshot. So the page is shown whether a key is set and never what it
// is, and a page that wants to change one sends a new one.
//
// Which means a round trip has to distinguish three cases that a plain string
// cannot: leave the key alone, replace it, or remove it. An absent field is
// leave. A field that is present and marked for clearing is remove. A field that
// is present is replace.

// ModelView is one model as the settings page sees it, with the assumed values
// filled in so the page does not have to know the defaults.
type ModelView struct {
	ID             string   `json:"id"`
	ContextWindow  int      `json:"context_window"`
	MaxOutput      int      `json:"max_output"`
	Vision         bool     `json:"vision"`
	Thinking       []string `json:"thinking,omitempty"`
	Disabled       bool     `json:"disabled"`
	DeclaredWindow bool     `json:"declared_window"`
	DeclaredOutput bool     `json:"declared_output"`
}

// ModelInput is one model as the settings page sends it.
//
// The two declared flags are what a person editing a field means by leaving it
// empty: "I do not know" and "this model has no limit" are different answers, and
// only one of them can be written down.
type ModelInput struct {
	ID string `json:"id"`
	// ContextWindow is the largest context the model takes, in tokens. It counts
	// only when DeclaredWindow says it was declared, because an empty field means
	// "not declared" and a zero would otherwise be indistinguishable from one.
	ContextWindow  int      `json:"context_window,omitempty"`
	MaxOutput      int      `json:"max_output,omitempty"`
	Vision         bool     `json:"vision,omitempty"`
	Thinking       []string `json:"thinking,omitempty"`
	Disabled       bool     `json:"disabled,omitempty"`
	DeclaredWindow bool     `json:"declared_window,omitempty"`
	DeclaredOutput bool     `json:"declared_output,omitempty"`
}

// Model renders the input as the file's own shape.
func (m ModelInput) Model() (Model, error) {
	model := Model{
		ID:       strings.TrimSpace(m.ID),
		Vision:   m.Vision,
		Disabled: m.Disabled,
	}
	if model.ID == "" {
		return Model{}, fmt.Errorf("%w: a model must have an id", ErrInvalidConfig)
	}
	if m.DeclaredWindow {
		if m.ContextWindow <= 0 {
			return Model{}, fmt.Errorf("%w: model %q declares a context window of %d",
				ErrInvalidConfig, model.ID, m.ContextWindow)
		}
		model.ContextWindow = m.ContextWindow
	}
	if m.DeclaredOutput {
		if m.MaxOutput <= 0 {
			return Model{}, fmt.Errorf("%w: model %q declares an output limit of %d",
				ErrInvalidConfig, model.ID, m.MaxOutput)
		}
		model.MaxOutput = m.MaxOutput
	}
	for _, level := range m.Thinking {
		parsed := ThinkingLevel(strings.TrimSpace(level))
		if !validThinkingLevel(parsed) {
			return Model{}, fmt.Errorf("%w: model %q declares an unknown reasoning level %q",
				ErrInvalidConfig, model.ID, level)
		}
		model.Thinking = append(model.Thinking, parsed)
	}
	return model, nil
}

func validThinkingLevel(level ThinkingLevel) bool {
	for _, known := range ThinkingLevels {
		if known == level {
			return true
		}
	}
	return false
}

// ProviderView is one provider as the settings page sees it.
type ProviderView struct {
	Name     string `json:"name,omitempty"`
	Enabled  bool   `json:"enabled"`
	Endpoint string `json:"endpoint"`
	// Format is one of the three, and the page is given the labels to go with
	// them rather than having to spell a format out itself.
	Format    string `json:"format"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// APIKey is the key as it is written down, so that a person editing this
	// provider can see what is there without going to the file.
	//
	// It goes out over the same loopback connection that everything else does,
	// behind the same token, to the same window the person is looking at. The
	// holder of the token is whoever opened this program on this machine, and the
	// key is already sitting in a file that person can read; hiding it from them
	// only meant a settings page that could not be used to fix a wrong key, which
	// is the one thing a settings page is for.
	APIKey string `json:"api_key,omitempty"`
	// APIKeySet says a key is held. The page uses it to tell a stored key from an
	// empty one when the field is left alone.
	APIKeySet bool `json:"api_key_set"`
	// APIKeyPresentFromEnv says the named environment variable currently has a
	// value, which is the difference between a provider that will work and one
	// that will be refused by the endpoint for want of a credential.
	APIKeyPresentFromEnv bool              `json:"api_key_present_from_env"`
	Models               []ModelView       `json:"models"`
	Headers              map[string]string `json:"headers,omitempty"`
	ServerState          bool              `json:"server_state,omitempty"`
	Thinking             *ThinkingConfig   `json:"thinking,omitempty"`
}

// ProviderInput is one provider as the settings page sends it.
type ProviderInput struct {
	Name      string `json:"name,omitempty"`
	Enabled   *bool  `json:"enabled,omitempty"`
	Endpoint  string `json:"endpoint"`
	Format    string `json:"format"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// APIKey replaces the stored key when it is present. A field that was left
	// out keeps what is stored; a field that was left empty on purpose is read as
	// clear when ClearAPIKey is set, because an absent field and an empty one are
	// the same thing in JSON unless something says otherwise.
	APIKey string `json:"api_key,omitempty"`
	// ClearAPIKey removes the stored key, which is the one operation the page
	// cannot express by sending a value.
	ClearAPIKey bool              `json:"clear_api_key,omitempty"`
	Models      []ModelInput      `json:"models,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	ServerState bool              `json:"server_state,omitempty"`
	Thinking    *ThinkingConfig   `json:"thinking,omitempty"`

	// apiKeySet records that the key field was present at all, which is the
	// difference between "leave it alone" and "set it to nothing".
	apiKeySet bool
}

// UnmarshalJSON notes whether the key field was present, before the settings
// page's own encoding is discarded.
func (p *ProviderInput) UnmarshalJSON(encoded []byte) error {
	type plain ProviderInput
	var shadow struct {
		plain
		APIKey *string `json:"api_key"`
	}
	if err := json.Unmarshal(encoded, &shadow); err != nil {
		return err
	}
	*p = ProviderInput(shadow.plain)
	if shadow.APIKey != nil {
		p.APIKey = *shadow.APIKey
		p.apiKeySet = true
	}
	return nil
}

// ModelView is what the page sees for a model, with the defaults made explicit.
func (m Model) View() ModelView {
	levels := make([]string, 0, len(m.Thinking))
	for _, level := range m.Thinking {
		levels = append(levels, string(level))
	}
	return ModelView{
		ID:             m.ID,
		ContextWindow:  m.ContextWindowOrDefault(),
		MaxOutput:      m.MaxOutputOrDefault(),
		Vision:         m.Vision,
		Thinking:       levels,
		Disabled:       m.Disabled,
		DeclaredWindow: m.ContextWindow > 0,
		DeclaredOutput: m.MaxOutput > 0,
	}
}

// View is the whole configuration as the settings page sees it, with the paths
// it would be written to, because a page whose writes go somewhere invisible
// cannot show what it is about to change.
type View struct {
	Version int `json:"version"`
	Model   struct {
		Default   string                  `json:"default"`
		Providers map[string]ProviderView `json:"providers"`
	} `json:"model"`
	Formats     []FormatOption `json:"formats"`
	GlobalPath  string         `json:"global_path"`
	ProjectPath string         `json:"project_path,omitempty"`
}

// Update is what the settings page sends.
type Update struct {
	Model struct {
		Default   string                   `json:"default"`
		Providers map[string]ProviderInput `json:"providers"`
	} `json:"model"`
}

// View renders the current configuration for the settings page.
func (s *Store) View() View {
	s.mu.RLock()
	defer s.mu.RUnlock()

	view := View{
		Version:    s.config.Version,
		Formats:    make([]FormatOption, 0, len(Formats)),
		GlobalPath: s.globalPath,
	}
	if s.hasProject {
		view.ProjectPath = s.projectPath
	}
	for _, format := range Formats {
		view.Formats = append(view.Formats, FormatOption{
			Value:    format,
			Label:    FormatLabel(format),
			Endpoint: FormatEndpoint(format),
		})
	}
	view.Model.Default = s.config.Model.Default
	view.Model.Providers = make(map[string]ProviderView, len(s.config.Model.Providers))
	for name, provider := range s.config.Model.Providers {
		view.Model.Providers[name] = s.providerViewLocked(name, provider)
	}
	return view
}

func (s *Store) providerViewLocked(name string, provider Provider) ProviderView {
	rendered := ProviderView{
		Name:                 s.displayName(name, provider),
		Enabled:              provider.Enabled,
		Endpoint:             provider.Endpoint,
		Format:               provider.Format,
		APIKeyEnv:            provider.APIKeyEnv,
		APIKey:               provider.APIKey,
		APIKeySet:            strings.TrimSpace(provider.APIKey) != "",
		APIKeyPresentFromEnv: provider.APIKeyEnv != "" && os.Getenv(provider.APIKeyEnv) != "",
		ServerState:          provider.ServerState,
		Models:               make([]ModelView, 0, len(provider.Models)),
	}
	if provider.Headers != nil {
		rendered.Headers = make(map[string]string, len(provider.Headers))
		for header, value := range provider.Headers {
			rendered.Headers[header] = value
		}
	}
	if provider.Thinking != nil {
		thinking := *provider.Thinking
		rendered.Thinking = &thinking
	}
	for _, model := range provider.Models {
		rendered.Models = append(rendered.Models, model.View())
	}
	return rendered
}

// displayName is what a provider is called on screen.
//
// A provider written before the name field existed is named after the key it is
// stored under, so that the list is never a column of blanks.
func (s *Store) displayName(name string, provider Provider) string {
	if trimmed := strings.TrimSpace(provider.Name); trimmed != "" {
		return trimmed
	}
	return name
}

// DisplayName is what a provider is called on screen, for callers outside the
// store.
func (s *Store) DisplayName(name string) string {
	provider, exists := s.Provider(name)
	if !exists {
		return name
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.displayName(name, provider)
}

// Apply validates an update, keeps the keys the page could not see, and makes it
// the configuration in force.
func (s *Store) Apply(update Update) error {
	s.mu.Lock()
	next, err := s.buildUpdate(update)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := Validate(next); err != nil {
		return err
	}
	return s.Replace(next)
}

// buildUpdate turns what a page sent into the configuration it means.
//
// It runs under the store's write lock because it reads the stored providers to
// carry their keys forward, and it returns the built configuration rather than
// installing it: validation happens outside the lock, so a rejected update does
// not hold the lock while a caller reads the error.
func (s *Store) buildUpdate(update Update) (Config, error) {
	// A page that sends no providers at all is a page that has lost them, not a
	// page that means to delete them all. An empty map with a key in it is
	// deliberate; an absent one is not.
	if update.Model.Providers == nil {
		if len(s.config.Model.Providers) > 0 {
			return Config{}, fmt.Errorf("%w: the update names no providers", ErrInvalidConfig)
		}
		return Config{Version: Version, Model: ModelConfig{
			Default:   strings.TrimSpace(update.Model.Default),
			Providers: map[string]Provider{},
		}}, nil
	}

	next := Config{Version: Version, Model: ModelConfig{
		Default:   strings.TrimSpace(update.Model.Default),
		Providers: make(map[string]Provider, len(update.Model.Providers)),
	}}
	for name, input := range update.Model.Providers {
		provider, err := input.provider(s.config.Model.Providers[name])
		if err != nil {
			return Config{}, fmt.Errorf("config: provider %q: %w", name, err)
		}
		next.Model.Providers[name] = provider
	}
	return next, nil
}

// provider builds one provider from what the page sent, keeping the key it could
// not see.
func (p ProviderInput) provider(previous Provider) (Provider, error) {
	provider := Provider{
		Name:        strings.TrimSpace(p.Name),
		Enabled:     true,
		Endpoint:    strings.TrimSpace(p.Endpoint),
		Format:      strings.TrimSpace(p.Format),
		APIKeyEnv:   strings.TrimSpace(p.APIKeyEnv),
		Headers:     p.Headers,
		ServerState: p.ServerState,
		Thinking:    p.Thinking,
	}
	if p.Enabled != nil {
		provider.Enabled = *p.Enabled
	}
	switch {
	case p.ClearAPIKey:
		// Said out loud rather than inferred from an empty field.
	case p.apiKeySet:
		provider.APIKey = p.APIKey
	default:
		// The page could not see the key, so it did not send it, so it is kept.
		// Losing a key because somebody renamed a model in another field would be a
		// surprise with no way back.
		provider.APIKey = previous.APIKey
	}
	models := make([]Model, 0, len(p.Models))
	seen := make(map[string]struct{}, len(p.Models))
	for _, input := range p.Models {
		model, err := input.Model()
		if err != nil {
			return Provider{}, err
		}
		if _, duplicate := seen[model.ID]; duplicate {
			return Provider{}, fmt.Errorf("%w: model %q is listed twice", ErrInvalidConfig, model.ID)
		}
		seen[model.ID] = struct{}{}
		models = append(models, model)
	}
	provider.Models = models
	return provider, nil
}
