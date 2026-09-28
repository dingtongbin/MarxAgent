// SPDX-License-Identifier: Apache-2.0

// Package config is the configuration system: a global file, a project file, and
// the rule for combining them.
//
// It is assembly rather than a part, because deciding that a provider's endpoint
// comes from the project file when a project declares one and from the global file
// when it does not is an assembly decision. What a provider looks like is not, and
// that is what the types here describe.
//
// The merge order is fixed by the design and is worth stating once: command line,
// then project file, then global file, then the built-in defaults. A project that
// declares a provider with the same name as a global one replaces it rather than
// merging field by field, because a half-overridden provider is a provider whose
// endpoint and format may come from two files that were never read together.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/dingtongbin/MarxAgent/internal/engine/gateway"
)

// Version is the schema version written into every file this package saves.
const Version = 1

// FileName is the name of a configuration file, in a project root and in the
// application data directory alike.
const FileName = "marxagent.json"

// ErrInvalidConfig reports a configuration that cannot be used.
var ErrInvalidConfig = errors.New("config: invalid configuration")

// ThinkingConfig enables extended thinking or reasoning summaries on one
// provider, which is a property of the provider rather than of a single
// conversation.
type ThinkingConfig struct {
	Enabled      bool `json:"enabled"`
	BudgetTokens int  `json:"budget_tokens,omitempty"`
	// ReasoningEffort is the OpenAI Chat Completions spelling of the same idea.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// ThinkingLevels are the reasoning strengths a model can be asked for.
//
// They are spelled as the interface spells them and stored as they are spelled
// in a file, because the two are different: a level stored as a number would
// have to be renumbered the day somebody added a level in the middle, and every
// configuration already on disk would then mean something different.
var ThinkingLevels = []ThinkingLevel{"off", "low", "medium", "high"}

// ThinkingLevel is one reasoning strength.
type ThinkingLevel string

// ThinkingLabel is what a person reads for a level.
func (t ThinkingLevel) Label() string {
	switch t {
	case "off":
		return "关闭"
	case "low":
		return "低"
	case "medium":
		return "中"
	case "high":
		return "高"
	default:
		return string(t)
	}
}

// Defaults used when a model does not say. They are the values the interface
// shows in the fields as placeholders, and the values a model with nothing
// declared is treated as having.
const (
	// DefaultContextWindow is what a model with no declared window is assumed to
	// have.
	DefaultContextWindow = 131072
	// DefaultMaxOutput is what a model with no declared output limit is assumed to
	// allow.
	DefaultMaxOutput = 16384
)

// Model is one model a provider offers, and what is known about it.
//
// The four numbers and the flag are not decoration. A conversation has to know
// how much room it has before it decides how much to compact, and a window has
// to know whether it may send a picture at all; both of those come from here
// rather than from being guessed at the moment they are needed.
//
// A zero value means "not declared" rather than "none", so a model that says
// nothing about its window is treated as having the default rather than as
// having no room.
type Model struct {
	ID string `json:"id"`
	// ContextWindow is the largest context the model takes, in tokens.
	ContextWindow int `json:"context_window,omitempty"`
	// MaxOutput is the largest single answer the model gives, in tokens.
	MaxOutput int `json:"max_output,omitempty"`
	// Vision says the model takes images as well as text.
	Vision bool `json:"vision,omitempty"`
	// Thinking is the reasoning strengths this model can be asked for, and an
	// empty one means it has no reasoning control of its own.
	Thinking []ThinkingLevel `json:"thinking,omitempty"`
	// Disabled keeps a model out of the choices without deleting it, which is what
	// somebody does to a model an endpoint offers and this machine should not use.
	Disabled bool `json:"disabled,omitempty"`
}

// ContextWindowOrDefault is the window to plan a conversation against.
func (m Model) ContextWindowOrDefault() int {
	if m.ContextWindow > 0 {
		return m.ContextWindow
	}
	return DefaultContextWindow
}

// MaxOutputOrDefault is the output limit to plan a conversation against.
func (m Model) MaxOutputOrDefault() int {
	if m.MaxOutput > 0 {
		return m.MaxOutput
	}
	return DefaultMaxOutput
}

// cloneModel copies a model, including its slice, because a configuration handed
// out is a snapshot rather than a view of the one in force: a caller that could
// reach through it could change what a running conversation is using.
func cloneModel(model Model) Model {
	cloned := model
	if model.Thinking != nil {
		cloned.Thinking = append([]ThinkingLevel(nil), model.Thinking...)
	}
	return cloned
}

// Provider is one model endpoint: where to send a request, in which of the three
// API formats, and with what credentials.
//
// The key is written inline even though the API key is not, because an editor
// that came back with a provider and no name could not show a person which one
// they were looking at. The key itself never leaves this process except as the
// one byte that says whether it is set.
type Provider struct {
	// Name is what a person calls this provider in the interface. The key it is
	// stored under is for files; this is for screens.
	Name     string `json:"name,omitempty"`
	Enabled  bool   `json:"enabled,omitempty"`
	Endpoint string `json:"endpoint"`
	// Format is one of the three API formats the gateway knows. It is required:
	// a provider without one is an endpoint, not a provider.
	Format string `json:"format"`
	// APIKeyEnv names the environment variable holding the key, which is how a key
	// stays out of a file. APIKey is the inline alternative for a local endpoint
	// that needs no real credential at all.
	APIKeyEnv string `json:"api_key_env,omitempty"`
	APIKey    string `json:"api_key,omitempty"`
	// Models are the models this provider offers. They are a convenience for the
	// settings page, not a restriction: any model name may be used, and one that
	// is not listed is not refused.
	Models []Model `json:"models,omitempty"`
	// Headers are sent with every request, which is how a gateway that wants a
	// tenant header or a version pin gets one.
	Headers map[string]string `json:"headers,omitempty"`
	// ServerState enables the server-side conversation state of the formats that
	// have it.
	ServerState bool            `json:"server_state,omitempty"`
	Thinking    *ThinkingConfig `json:"thinking,omitempty"`
}

// FindModel returns one of the provider's models by name.
func (p Provider) FindModel(id string) (Model, bool) {
	for _, model := range p.Models {
		if model.ID == id {
			return model, true
		}
	}
	return Model{}, false
}

// ModelConfig is the model section: which providers exist and which one a
// conversation uses when nobody says otherwise.
type ModelConfig struct {
	// Default is "provider:model". A model with no provider of that name in it
	// is a whole-model name for the only configured provider, which is what a
	// single-provider installation writes.
	Default   string              `json:"default,omitempty"`
	Providers map[string]Provider `json:"providers,omitempty"`
}

// Config is the file as it is written. It holds the sections this build reads;
// a section it does not read is not here, because a field that is always the
// zero value is a lie about what the program does.
type Config struct {
	Version int         `json:"version"`
	Model   ModelConfig `json:"model"`
}

// defaultConfig is what a machine with no configuration file has: no model, and
// therefore a reason for the settings page to say so rather than for a
// conversation to fail with a message about a missing endpoint.
func defaultConfig() Config {
	return Config{Version: Version}
}

// Store holds the configuration and the paths it came from.
//
// It is a store rather than a value because two windows can have the settings
// page open at once, and both of them will save. Whichever arrives second wins,
// and it wins the whole section rather than a field of it, for the reason given
// in the package comment.
type Store struct {
	mu sync.RWMutex
	// config is the merged view: what the program runs on.
	config Config
	// sources records where the merged view came from, so an error can say which
	// file is wrong rather than only that something is.
	globalPath  string
	projectPath string
	// projectRaw is the project file as it was read, kept so a save can leave it
	// alone. A project file is in version control and belongs to whoever wrote
	// it; a settings page that rewrote it would be a settings page making commits.
	projectRaw Config
	// hasProject records whether a project file was found at all, so a save into
	// a directory that has no project file does not create one.
	hasProject bool
}

// Open reads the global configuration and the project configuration, if there is
// a project, and combines them.
//
// A missing file is not an error. A configuration that is present and unreadable
// is, because silently running on the built-in defaults after somebody spent an
// afternoon writing a file is worse than refusing to start.
func Open(projectRoot string) (*Store, error) {
	globalPath, err := GlobalPath()
	if err != nil {
		return nil, err
	}
	return openAt(globalPath, filepath.Join(projectRoot, FileName))
}

// OpenAt is Open with the global path given as well.
//
// It exists because the global path belongs to the machine, and a test that read
// the real one would be reading whichever providers and API keys the person
// running it has configured — and a test that passed or failed according to that
// is a test whose result means nothing on anybody else's machine.
func OpenAt(globalPath, projectRoot string) (*Store, error) {
	return openAt(globalPath, filepath.Join(projectRoot, FileName))
}

// openAt is Open with both paths given, which is what a test needs and Open does
// not: the global path belongs to the machine, and a test that wrote to it would
// be a test that edited the settings of whoever was running it.
func openAt(globalPath, projectPath string) (*Store, error) {
	store := &Store{globalPath: globalPath, config: defaultConfig()}

	global, err := readFile(globalPath)
	if err != nil {
		return nil, err
	}
	if global != nil {
		store.merge(*global)
	}

	store.projectPath = projectPath
	project, err := readFile(projectPath)
	if err != nil {
		return nil, err
	}
	if project != nil {
		store.projectRaw = *project
		store.hasProject = true
		store.merge(*project)
	}
	return store, nil
}

// GlobalPath is where the global configuration lives.
//
// It goes through os.UserConfigDir, so it is the platform's own answer rather
// than a path this project has to keep right for six combinations of operating
// system and architecture.
func GlobalPath() (string, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: locate user config directory: %w", err)
	}
	return filepath.Join(directory, "marxagent", FileName), nil
}

// readFile loads one configuration file. A file that is not there is reported as
// absent rather than as an error, because a machine that has never been
// configured is a normal machine.
func readFile(path string) (*Config, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var loaded Config
	if err := json.Unmarshal(encoded, &loaded); err != nil {
		return nil, fmt.Errorf("config: %s is not valid configuration: %w", path, err)
	}
	return &loaded, nil
}

// merge folds one configuration into the running one. Providers replace
// wholesale; nothing is merged field by field.
func (s *Store) merge(loaded Config) {
	if loaded.Version > 0 {
		s.config.Version = loaded.Version
	}
	if loaded.Model.Default != "" {
		s.config.Model.Default = loaded.Model.Default
	}
	if len(loaded.Model.Providers) > 0 {
		if s.config.Model.Providers == nil {
			s.config.Model.Providers = make(map[string]Provider, len(loaded.Model.Providers))
		}
		for name, provider := range loaded.Model.Providers {
			s.config.Model.Providers[name] = provider
		}
	}
}

// Current returns the merged configuration.
func (s *Store) Current() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.clone()
}

// Replace validates a configuration and makes it the one in force, writing the
// global file.
//
// It is the whole section rather than a patch. A settings page that sent only
// the field it had changed would be a settings page that could not be sure what
// the result was, and a half-written provider is worse than a rejected one.
func (s *Store) Replace(next Config) error {
	if err := Validate(next); err != nil {
		return err
	}
	s.mu.Lock()
	s.config = s.cloneOf(next)
	path := s.globalPath
	s.mu.Unlock()
	return writeFile(path, next)
}

// Provider returns one provider by name.
func (s *Store) Provider(name string) (Provider, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	provider, exists := s.config.Model.Providers[name]
	return provider, exists
}

// ProviderNames lists the configured providers in a stable order, because a list
// that reorders itself between two reads is a list a person cannot learn.
func (s *Store) ProviderNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.config.Model.Providers))
	for name := range s.config.Model.Providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ModelRef is a model named the way a configuration names one.
type ModelRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// String is the "provider:model" spelling.
func (r ModelRef) String() string { return r.Provider + ":" + r.Model }

// Spec renders the reference the way it is written in a file.
func (r ModelRef) Spec() string { return r.String() }

// Resolve splits a model specification into its provider and its model name.
//
// The split is not a cut at the first colon, because model names contain colons:
// an Ollama tag is `qwen3:8b` and a versioned name is `llama3:70b-instruct`. A
// first-colon cut turns the most common local model name into a provider that
// does not exist.
//
// So the specification is matched against the providers that exist, longest name
// first, and whatever is left is the model. A specification that names no
// provider is run against the only provider there is, and against none at all
// when there is more than one, because guessing between two is not an answer.
func (s *Store) Resolve(spec string) (ModelRef, error) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		trimmed = strings.TrimSpace(s.Current().Model.Default)
		if trimmed == "" {
			return ModelRef{}, fmt.Errorf("%w: no model is configured", ErrInvalidConfig)
		}
	}
	for _, name := range s.providerNamesByLengthDescending() {
		// Cut at the provider's own name plus a colon, so what is left is the
		// model with any colons of its own still in it.
		if _, modelName, matched := strings.Cut(trimmed, name+":"); matched {
			modelName = strings.TrimSpace(modelName)
			if modelName == "" {
				return ModelRef{}, fmt.Errorf("%w: %q names a provider but no model", ErrInvalidConfig, trimmed)
			}
			return ModelRef{Provider: name, Model: modelName}, nil
		}
	}
	if provider, single := s.onlyProvider(); single {
		return ModelRef{Provider: provider, Model: trimmed}, nil
	}
	if strings.Contains(trimmed, ":") {
		providerName, modelName, _ := strings.Cut(trimmed, ":")
		return ModelRef{}, fmt.Errorf(
			"%w: no provider named %q is configured (model %q)", ErrInvalidConfig, providerName, modelName)
	}
	return ModelRef{}, fmt.Errorf(
		"%w: %q names no provider and no single provider is configured", ErrInvalidConfig, trimmed)
}

// providerNamesByLengthDescending orders provider names so that the longest match
// wins. A provider named "a" and one named "a:b" would otherwise both match the
// specification "a:b:c", and the shorter one would win whichever order the map
// happened to iterate in.
func (s *Store) providerNamesByLengthDescending() []string {
	names := s.ProviderNames()
	sort.Slice(names, func(left, right int) bool {
		if len(names[left]) != len(names[right]) {
			return len(names[left]) > len(names[right])
		}
		return names[left] < names[right]
	})
	return names
}

// onlyProvider reports the single configured provider, and refuses when there is
// not exactly one, because guessing between two is not an answer.
func (s *Store) onlyProvider() (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	switch len(s.config.Model.Providers) {
	case 0:
		return "", false
	case 1:
		for name := range s.config.Model.Providers {
			return name, true
		}
	}
	return "", false
}

// Gateway builds the model gateway for one model reference.
//
// This is where a provider in a file becomes a gateway that can be handed to the
// core, and it is deliberately the only place that translation happens: a
// second one would be a second place for an endpoint and a format to disagree.
func (s *Store) Gateway(ref ModelRef, options ...func(*gateway.Config)) (*gateway.ProviderGateway, error) {
	provider, exists := s.Provider(ref.Provider)
	if !exists {
		return nil, fmt.Errorf("%w: no provider named %q is configured", ErrInvalidConfig, ref.Provider)
	}
	if err := validateProvider(ref.Provider, provider); err != nil {
		return nil, err
	}
	format, err := ParseFormat(provider.Format)
	if err != nil {
		return nil, fmt.Errorf("config: provider %q: %w", ref.Provider, err)
	}
	gatewayConfig := gateway.Config{
		Format:      format,
		Endpoint:    provider.Endpoint,
		APIKey:      provider.APIKey,
		APIKeyEnv:   provider.APIKeyEnv,
		Model:       ref.Model,
		Headers:     provider.Headers,
		ServerState: provider.ServerState,
	}
	if provider.Thinking != nil {
		gatewayConfig.Thinking = &gateway.ThinkingConfig{
			Enabled:         provider.Thinking.Enabled,
			BudgetTokens:    provider.Thinking.BudgetTokens,
			ReasoningEffort: provider.Thinking.ReasoningEffort,
		}
	}
	for _, option := range options {
		option(&gatewayConfig)
	}
	return gateway.New(gatewayConfig)
}

// ProviderSpec is a provider as one request carries it, whether or not it has been
// saved.
//
// It exists because the two things a person does with a provider they have just
// typed — asking the endpoint what models it has, and asking whether it answers at
// all — happen before the save, every time. A page that can only do those to a
// provider already on disk is a page that makes somebody save a half-finished
// provider to find out they got the address wrong, which is the opposite of what
// the buttons are for.
type ProviderSpec struct {
	// Name says which stored provider to fall back on. It is ignored when Endpoint
	// is set, because a provider that has an address is one this request describes
	// in full.
	Name string `json:"name,omitempty"`
	// Endpoint and Format are the request's own answer. A spec with neither is the
	// stored provider named above.
	Endpoint string `json:"endpoint,omitempty"`
	Format   string `json:"format,omitempty"`
	// APIKey travels in the request for a provider being tested, and is the same
	// key the save would write. It is not kept by this method.
	APIKey    string            `json:"api_key,omitempty"`
	APIKeyEnv string            `json:"api_key_env,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
}

// GatewayFor builds the gateway for one spec.
//
// A spec that carries its own address is answered from itself, and one that does
// not is answered from the stored provider it names. Failing to find a stored
// provider is an error rather than an empty gateway, because a probe of nothing
// would report the endpoint as unreachable when the truth is that there is no
// such provider to reach.
func (s *Store) GatewayFor(spec ProviderSpec, model string) (*gateway.ProviderGateway, error) {
	if strings.TrimSpace(spec.Endpoint) != "" {
		format, err := ParseFormat(spec.Format)
		if err != nil {
			return nil, err
		}
		return gateway.New(gateway.Config{
			Format:    format,
			Endpoint:  spec.Endpoint,
			APIKey:    spec.APIKey,
			APIKeyEnv: spec.APIKeyEnv,
			Model:     model,
			Headers:   spec.Headers,
		})
	}
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: name a provider or give an address", ErrInvalidConfig)
	}
	if _, exists := s.Provider(name); !exists {
		return nil, fmt.Errorf("%w: no provider named %q is configured", ErrInvalidConfig, name)
	}
	reference := ModelRef{Provider: name, Model: model}
	built, err := s.Gateway(reference)
	if err != nil {
		return nil, err
	}
	// A key sent with the request overrides the stored one, which is what makes a
	// freshly typed key testable before it is saved.
	if spec.APIKey != "" {
		return s.Gateway(reference, WithAPIKey(spec.APIKey))
	}
	return built, nil
}

// WithAPIKey is how a caller says which key to use rather than the stored one.
func WithAPIKey(key string) func(*gateway.Config) { return func(c *gateway.Config) { c.APIKey = key } }

// Formats are the three API formats a provider can speak, which is the number
// the settings page offers as a choice.
var Formats = []string{
	string(gateway.FormatOpenAICompletions),
	string(gateway.FormatAnthropicMessages),
	string(gateway.FormatOpenAIResponses),
}

// FormatOption is one of the three formats, named for a person to pick.
type FormatOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
	// Endpoint is the path the format's requests go to, shown beside the name
	// because "which of the three" is a question about an address as much as about
	// a name, and the address is what somebody with a gateway in front of it
	// actually has.
	Endpoint string `json:"endpoint"`
}

// FormatLabel is what a person reads for one format.
//
// The name alone, with nothing appended. A bracketed explanation of who a format
// is for reads as a hedge on a control that is not hedged: picking one of three
// is picking one of three, and the address beside it already says which one an
// endpoint speaks.
func FormatLabel(format string) string {
	switch format {
	case string(gateway.FormatOpenAICompletions):
		return "OpenAI Chat Completions"
	case string(gateway.FormatAnthropicMessages):
		return "Anthropic Messages"
	case string(gateway.FormatOpenAIResponses):
		return "OpenAI Responses"
	default:
		return format
	}
}

// FormatEndpoint is the path a format's requests go to, which is the second half
// of what somebody needs to tell when they are looking at a gateway's address and
// trying to work out which format it speaks.
func FormatEndpoint(format string) string {
	switch gateway.APIFormat(format) {
	case gateway.FormatOpenAICompletions:
		return "/chat/completions"
	case gateway.FormatAnthropicMessages:
		return "/messages"
	case gateway.FormatOpenAIResponses:
		return "/responses"
	default:
		return ""
	}
}

// ParseFormat turns a configured format into the gateway's own name for it.
func ParseFormat(format string) (gateway.APIFormat, error) {
	parsed := gateway.APIFormat(strings.TrimSpace(format))
	if _, err := gateway.NewAdapter(parsed); err != nil {
		return "", fmt.Errorf("%w: unknown API format %q", ErrInvalidConfig, format)
	}
	return parsed, nil
}

// Validate refuses a configuration that would fail later, in the middle of a
// conversation, with a message that names a file rather than a provider.
func Validate(config Config) error {
	if config.Version > Version {
		return fmt.Errorf("%w: version %d is newer than this build understands (%d)",
			ErrInvalidConfig, config.Version, Version)
	}
	for name, provider := range config.Model.Providers {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("%w: a provider name must not be empty", ErrInvalidConfig)
		}
		if err := validateProvider(name, provider); err != nil {
			return err
		}
	}
	if spec := strings.TrimSpace(config.Model.Default); spec != "" {
		providerName, modelName, found := strings.Cut(spec, ":")
		if !found || strings.TrimSpace(modelName) == "" {
			return fmt.Errorf("%w: the default model %q is not in provider:model form", ErrInvalidConfig, spec)
		}
		if _, exists := config.Model.Providers[strings.TrimSpace(providerName)]; !exists {
			return fmt.Errorf("%w: the default model names provider %q, which is not configured",
				ErrInvalidConfig, providerName)
		}
	}
	return nil
}

func validateProvider(name string, provider Provider) error {
	if strings.TrimSpace(provider.Endpoint) == "" {
		return fmt.Errorf("%w: provider %q has no endpoint", ErrInvalidConfig, name)
	}
	if _, err := ParseFormat(provider.Format); err != nil {
		return fmt.Errorf("config: provider %q: %w", name, err)
	}
	if provider.Thinking != nil && provider.Thinking.BudgetTokens < 0 {
		return fmt.Errorf("%w: provider %q has a negative thinking budget", ErrInvalidConfig, name)
	}
	return nil
}

// writeFile writes a configuration atomically.
//
// The temporary file and the rename are the point. A settings save that was cut
// short by a crash would otherwise leave a file that cannot be parsed, and the
// next start would refuse to run because of an interrupted save.
func writeFile(path string, config Config) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("config: create %s: %w", directory, err)
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encode configuration: %w", err)
	}
	encoded = append(encoded, '\n')

	// 0o600 because a configuration file can hold an API key for a provider
	// declared inline, and a file that is readable by every account on the
	// machine is a file the machine's other users can read the key out of.
	temporary, err := os.CreateTemp(directory, FileName+".*")
	if err != nil {
		return fmt.Errorf("config: create temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() {
		_ = os.Remove(temporaryName)
	}()
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("config: write temporary file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("config: sync temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("config: close temporary file: %w", err)
	}
	// The temporary file is created by CreateTemp with 0o600, so it is already
	// private; the mode is set anyway so a umask between the two cannot widen it.
	if err := os.Chmod(temporaryName, 0o600); err != nil {
		return fmt.Errorf("config: set file mode: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	return nil
}

func (s *Store) clone() Config { return s.cloneOf(s.config) }

func (s *Store) cloneOf(source Config) Config {
	cloned := Config{Version: source.Version, Model: ModelConfig{Default: source.Model.Default}}
	if source.Model.Providers != nil {
		cloned.Model.Providers = make(map[string]Provider, len(source.Model.Providers))
		for name, provider := range source.Model.Providers {
			copied := provider
			if provider.Models != nil {
				copied.Models = make([]Model, len(provider.Models))
				for index, model := range provider.Models {
					copied.Models[index] = cloneModel(model)
				}
			}
			if provider.Headers != nil {
				copied.Headers = make(map[string]string, len(provider.Headers))
				for header, value := range provider.Headers {
					copied.Headers[header] = value
				}
			}
			if provider.Thinking != nil {
				thinking := *provider.Thinking
				copied.Thinking = &thinking
			}
			cloned.Model.Providers[name] = copied
		}
	}
	return cloned
}
