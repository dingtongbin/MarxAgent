// SPDX-License-Identifier: Apache-2.0

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/assembly/api"
	"github.com/dingtongbin/MarxAgent/internal/assembly/config"
	"github.com/dingtongbin/MarxAgent/internal/assembly/runtime"
	"github.com/dingtongbin/MarxAgent/internal/assembly/ui/web"
	"github.com/dingtongbin/MarxAgent/internal/assembly/workspace"
	"github.com/dingtongbin/MarxAgent/internal/core"
	"github.com/dingtongbin/MarxAgent/internal/engine/storage"
)

// The model configuration seen the way the interface sees it. Declared here rather
// than reusing the server's own view type, because a test that reuses the type it
// is testing cannot catch the type being wrong.
type configViewBody struct {
	Model struct {
		Default   string `json:"default"`
		Providers map[string]struct {
			Endpoint  string `json:"endpoint"`
			Format    string `json:"format"`
			APIKeySet bool   `json:"api_key_set"`
		} `json:"providers"`
	} `json:"model"`
	Formats []struct {
		Value string `json:"value"`
		Label string `json:"label"`
	} `json:"formats"`
	GlobalPath string `json:"global_path"`
}

// fakeModelServer is a model endpoint: a listing on one path, a stream on the
// other. It is the whole of the outside world for this test, so everything the
// interface does has to work against it and nothing else.
func fakeModelServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"fake-small","display_name":"Fake Small"},` +
				`{"id":"fake-large","display_name":"Fake Large"}]}`))
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			for _, frame := range []string{
				`data: {"id":"1","choices":[{"delta":{"role":"assistant"}}]}`,
				`data: {"id":"1","choices":[{"delta":{"content":"hello"}}]}`,
				`data: {"id":"1","choices":[{"delta":{"content":" from the model"}}]}`,
				`data: {"id":"1","choices":[{"delta":{},"finish_reason":"stop"}]}`,
				"data: [DONE]",
			} {
				_, _ = fmt.Fprint(w, frame+"\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// served is a running program: a real web server, the real runtime behind it, and
// the temporary directories it is using rather than the ones on this machine.
type served struct {
	base  string
	token string
	// sessions is the runtime, so a test can watch the event stream the way a
	// window would rather than reaching inside the server.
	sessions *runtime.Runtime
	home     string
}

func serve(t *testing.T) *served {
	t.Helper()
	home := t.TempDir()
	// Both the configuration and the data are inside the temporary directory, so
	// this test neither reads nor writes the machine it is running on. The
	// environment is set rather than the paths passed in, because the storage
	// scope asks the platform where the application data directory is and a test
	// that answered for it would be testing a different program.
	t.Setenv("APPDATA", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))

	globalConfig := filepath.Join(home, "config", "marxagent", config.FileName)
	if err := os.MkdirAll(filepath.Dir(globalConfig), 0o755); err != nil {
		t.Fatalf("create: %v", err)
	}
	settings, err := config.OpenAt(globalConfig, home)
	if err != nil {
		t.Fatalf("open the config: %v", err)
	}
	scope, err := storage.OpenScope(storage.ScopeAppData, filepath.Join(home, "data"))
	if err != nil {
		t.Fatalf("open the scope: %v", err)
	}
	sessions, err := runtime.New(runtime.Options{
		Config:       settings,
		AppDataScope: scope,
		Logger:       func(message string, fields map[string]any) { t.Logf("runtime: %s %v", message, fields) },
	})
	if err != nil {
		t.Fatalf("build the runtime: %v", err)
	}
	// The tab strip is assembled here too, because a window that opens a
	// conversation and finds no strip endpoint is a window that cannot be
	// reassembled, and the integration test is the only place that whole assembly
	// is built at once.
	tabs, err := workspace.Open(filepath.Join(home, "workspaces"))
	if err != nil {
		t.Fatalf("open the tab store: %v", err)
	}
	token, err := web.NewToken()
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	listener, err := web.LoopbackListener()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server, err := web.New(web.Config{
		Listener: listener,
		Token:    token,
		Stream:   sessions,
		Handler: api.New(sessions, settings, tabs, func(message string, fields map[string]any) {
			t.Logf("api: %s %v", message, fields)
		}),
	})
	if err != nil {
		t.Fatalf("build the server: %v", err)
	}
	running, stopServing := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_ = server.Start(running)
	}()
	t.Cleanup(func() {
		stopServing()
		<-finished
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sessions.Close(ctx)
	})
	return &served{base: "http://" + server.Addr(), token: token, sessions: sessions, home: home}
}

// call makes one request and insists on the status it expected.
func (s *served) call(t *testing.T, method, path string, body any, want int) []byte {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, s.base+path, reader)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	if s.token != "" {
		request.Header.Set("Authorization", "Bearer "+s.token)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if response.StatusCode != want {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, response.StatusCode, want, payload)
	}
	return payload
}

func (s *served) config(t *testing.T) configViewBody {
	t.Helper()
	var view configViewBody
	payload := s.call(t, http.MethodGet, "/v1/config", nil, http.StatusOK)
	if err := json.Unmarshal(payload, &view); err != nil {
		t.Fatalf("decode the configuration %q: %v", payload, err)
	}
	return view
}

func TestAMachineWithNothingConfiguredSaysSoRatherThanGuessing(t *testing.T) {
	server := serve(t)
	view := server.config(t)
	if view.Model.Default != "" {
		t.Fatalf("a machine with no configuration has a default model: %q", view.Model.Default)
	}
	if len(view.Formats) != 3 {
		t.Fatalf("formats = %#v, want the three the gateway speaks", view.Formats)
	}
	if view.GlobalPath == "" {
		t.Fatal("the configuration does not say where a save goes")
	}
}

func TestTheConfigurationIsBehindTheToken(t *testing.T) {
	server := serve(t)
	// With no token at all. The health check stays outside because a health check
	// that needs a secret is a health check nobody can run.
	if status := plainGet(t, server.base+"/healthz"); status != http.StatusOK {
		t.Fatalf("health = %d, want 200", status)
	}
	if status := plainGet(t, server.base+"/v1/config"); status != http.StatusUnauthorized {
		t.Fatalf("the configuration without a token = %d, want 401", status)
	}
	if status := plainGet(t, server.base+"/"); status != http.StatusUnauthorized {
		t.Fatalf("the interface without a token = %d, want 401", status)
	}
}

func plainGet(t *testing.T, url string) int {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

func TestAConversationIsRefusedWithSomethingTheInterfaceCanActOnWhenNoModelIsConfigured(t *testing.T) {
	server := serve(t)
	payload := server.call(t, http.MethodPost, "/v1/sessions", map[string]any{}, http.StatusCreated)
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decode %q: %v", payload, err)
	}
	body := server.call(t, http.MethodPost, "/v1/sessions/"+created.ID+"/messages",
		map[string]any{"text": "hello"}, http.StatusUnprocessableEntity)
	// The window's answer to this is to open the settings page, so the message has
	// to say what is missing rather than only that something is.
	if !strings.Contains(string(body), "no model is configured") {
		t.Fatalf("the refusal does not say what is missing: %s", body)
	}
}

func TestTheSettingsPageCanSaveAProviderAndThenUseIt(t *testing.T) {
	model := fakeModelServer(t)
	server := serve(t)

	// Exactly what the settings page sends: a provider, and a default model named
	// the way a configuration names one.
	server.call(t, http.MethodPut, "/v1/config", map[string]any{
		"model": map[string]any{
			"default": "local:fake-small",
			"providers": map[string]any{
				"local": map[string]any{
					"endpoint": model.URL + "/v1",
					"format":   "openai-completions",
				},
			},
		},
	}, http.StatusOK)

	// The saved configuration is what the server now holds, not what the page sent.
	view := server.config(t)
	if view.Model.Default != "local:fake-small" {
		t.Fatalf("default = %q after the save", view.Model.Default)
	}
	stored, listed := view.Model.Providers["local"]
	if !listed || stored.Endpoint != model.URL+"/v1" {
		t.Fatalf("the saved provider = %#v", view.Model.Providers)
	}

	// The provider is asked what it offers, at the address a conversation would go
	// to, with the credentials a conversation would use.
	payload := server.call(t, http.MethodGet, "/v1/providers/local/models", nil, http.StatusOK)
	var listing struct {
		Models []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"models"`
	}
	if err := json.Unmarshal(payload, &listing); err != nil {
		t.Fatalf("decode %q: %v", payload, err)
	}
	if len(listing.Models) != 2 || listing.Models[0].ID != "fake-large" {
		t.Fatalf("models = %#v, want the endpoint's own two in order", listing.Models)
	}

	// And now a message goes to the model that was just configured.
	payload = server.call(t, http.MethodPost, "/v1/sessions", map[string]any{}, http.StatusCreated)
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(payload, &created); err != nil {
		t.Fatalf("decode %q: %v", payload, err)
	}

	// The window is opened before the message is sent, because that is the order a
	// person is in: a stream that starts after the turn has already happened is
	// told the conversation's position and nothing else, so a test that sent first
	// and watched second would be waiting for something that has been and gone.
	watching, stopWatching, err := server.sessions.Subscribe(context.Background(), created.ID, 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer stopWatching()
	<-watching

	server.call(t, http.MethodPost, "/v1/sessions/"+created.ID+"/messages",
		map[string]any{"text": "say hello"}, http.StatusAccepted)

	answer := waitForAnswer(t, watching)
	if answer != "hello from the model" {
		t.Fatalf("answer = %q, want what the endpoint streamed", answer)
	}

	// The conversation is on disk, in the file a person can read. It is read after
	// the turn has settled rather than after the answer appeared, because that is
	// when the pipeline is flushed: the answer arriving is the model finishing,
	// and the file is written at the boundary after it.
	waitFor(t, "the turn to be written down", func() bool {
		for _, session := range server.sessions.List() {
			if session.ID == created.ID && session.State == runtime.StateCheckpointed {
				return true
			}
		}
		return false
	})
	journal := filepath.Join(server.home, "data", storage.SessionsDirName, created.ID, "session.jsonl")
	if listing, err := os.ReadDir(filepath.Dir(journal)); err != nil {
		t.Logf("the session directory does not exist: %v", err)
	} else {
		names := make([]string, 0, len(listing))
		for _, entry := range listing {
			info, statErr := entry.Info()
			size := -1
			if statErr == nil {
				size = int(info.Size())
			}
			names = append(names, fmt.Sprintf("%s(%d)", entry.Name(), size))
		}
		t.Logf("the session directory holds %v", names)
	}
	records, partial, err := storage.ReadJournal(journal)
	if err != nil {
		t.Fatalf("read the journal: %v", err)
	}
	if len(partial) != 0 {
		t.Fatalf("the journal ends in a half written line: %q", partial)
	}
	if len(records) != 2 {
		t.Fatalf("journal = %d records, want one per message", len(records))
	}
}

// waitFor is a condition with a deadline, for the things a turn does after the
// thing being waited for. The message says what was being waited for, because
// "timed out" on its own leaves a person guessing which of four waits it was.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitForAnswer reads the event stream the way a window does, and returns the text
// of the answer as the pieces arrived. Reading it rather than asking the
// conversation afterwards is deliberate: the pieces are what a window shows while
// a model is still talking, and a test that only looked at the end would not know
// whether any of them were ever sent.
func waitForAnswer(t *testing.T, stream <-chan web.Event) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var answer string
	for {
		select {
		case event, open := <-stream:
			if !open {
				t.Fatalf("the stream ended before the answer arrived; got %q", answer)
			}
			var envelope struct {
				Type string          `json:"type"`
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(event.Payload, &envelope); err != nil {
				continue
			}
			if envelope.Type != string(core.EventTypeStateChange) {
				continue
			}
			var change core.StateChangePayload
			if err := json.Unmarshal(envelope.Data, &change); err != nil {
				continue
			}
			if change.Message.Role != core.RoleAssistant {
				continue
			}
			for _, block := range change.Message.Content {
				if block.Type == core.ContentTypeText {
					answer += block.Text
				}
			}
			return answer
		case <-ctx.Done():
			t.Fatal("timed out waiting for the answer")
		}
	}
}

func TestTheProvidersOwnRoutesDoNotTakeTheInterfaceWithThem(t *testing.T) {
	server := serve(t)
	// The caller's routes are mounted ahead of the built interface, so a catch-all
	// would have answered requests for the interface itself with whatever the
	// caller had for an unmatched path. A server with API routes and no interface
	// is a page of nothing, which looks like a broken build.
	server.call(t, http.MethodPut, "/v1/config", map[string]any{"model": map[string]any{
		"default": "local:m",
		"providers": map[string]any{
			"local": map[string]any{"endpoint": "http://127.0.0.1:11434/v1", "format": "openai-completions"},
		},
	}}, http.StatusOK)
	if status := plainGet(t, server.base+"/"); status != http.StatusUnauthorized {
		// Without a token it is the token that refuses it, which is the right
		// answer and still not the interface being gone.
		t.Fatalf("the interface without a token = %d, want 401", status)
	}
	payload := server.call(t, http.MethodGet, "/", nil, http.StatusOK)
	if !bytes.Contains(payload, []byte("<html")) {
		t.Fatalf("the interface is not being served: %.200s", payload)
	}
}
