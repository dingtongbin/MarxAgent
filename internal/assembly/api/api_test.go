// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/assembly/config"
	"github.com/dingtongbin/MarxAgent/internal/assembly/runtime"
	"github.com/dingtongbin/MarxAgent/internal/assembly/workspace"
	"github.com/dingtongbin/MarxAgent/internal/core"
	"github.com/dingtongbin/MarxAgent/internal/engine/storage"
)

type fixture struct {
	handler  http.Handler
	runtime  *runtime.Runtime
	settings *config.Store
	tabs     *workspace.Store
}

// tabbed is a request about the one tab strip.
func (f *fixture) tabbed(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

// scriptedProvider answers every request with a fixed script, so a test can say
// what the model said rather than what a model would have said.
type scriptedProvider struct {
	chunks []core.StreamChunk
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) ChatStream(_ context.Context, _ core.ChatRequest) (<-chan core.StreamChunk, error) {
	out := make(chan core.StreamChunk, len(p.chunks))
	for _, chunk := range p.chunks {
		out <- chunk
	}
	close(out)
	return out, nil
}

// A fixture with a tab store, because a window's tabs are part of what a server
// serves and a test of anything else would pass with no tabs at all.
func newFixture(t *testing.T, provider core.Provider) *fixture {
	t.Helper()
	directory := t.TempDir()
	scope, err := storage.OpenScope(storage.ScopeAppData, filepath.Join(directory, "appdata"))
	if err != nil {
		t.Fatalf("open the scope: %v", err)
	}
	settings, err := config.OpenAt(filepath.Join(directory, "config", config.FileName), directory)
	if err != nil {
		t.Fatalf("open the config: %v", err)
	}
	tabs, err := workspace.Open(filepath.Join(directory, "workspaces"))
	if err != nil {
		t.Fatalf("open the tab store: %v", err)
	}
	counter := atomic.Int32{}
	sessions, err := runtime.New(runtime.Options{
		Config:       settings,
		AppDataScope: scope,
		NewID: func() (string, error) {
			return fmt.Sprintf("session-%d", counter.Add(1)), nil
		},
		Gateway: func(config.ModelRef) (core.Provider, error) { return provider, nil },
	})
	if err != nil {
		t.Fatalf("build the runtime: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sessions.Close(ctx)
	})
	return &fixture{
		handler:  New(sessions, settings, tabs, nil),
		runtime:  sessions,
		settings: settings,
		tabs:     tabs,
	}
}

func TestATabStripComesBackAfterAWindowIsReloaded(t *testing.T) {
	// The whole reason the tree is on the server: a window that is reloaded has no
	// memory, so a strip that lived only in that window is gone.
	f := newFixture(t, nil)
	recorder := f.tabbed(t, http.MethodPut, pathWorkspace, map[string]any{
		"tabs": []map[string]string{
			{"id": "session-1", "kind": "session"},
			{"id": "settings", "kind": "settings"},
		},
		"active": "session-1",
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT workspace = %d: %s", recorder.Code, recorder.Body.String())
	}

	recorder = f.tabbed(t, http.MethodGet, pathWorkspace, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET workspace = %d: %s", recorder.Code, recorder.Body.String())
	}
	var tree struct {
		Tabs []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"tabs"`
		Active string `json:"active"`
	}
	decodeInto(t, recorder, &tree)
	if len(tree.Tabs) != 2 || tree.Tabs[0].ID != "session-1" {
		t.Fatalf("tabs = %#v, want the two that were stored", tree.Tabs)
	}
	if tree.Active != "session-1" {
		t.Fatalf("active = %q, want the one that was stored", tree.Active)
	}
}

func TestTheStripIsTheSameWhoeverAsks(t *testing.T) {
	// A tree kept per window needs a value stored on the browser to tell one window
	// from another, and the same window on another machine would then come back to
	// different tabs. There is one strip, and everybody reads it.
	f := newFixture(t, nil)
	f.tabbed(t, http.MethodPut, pathWorkspace, map[string]any{
		"tabs":   []map[string]string{{"id": "a", "kind": "session"}},
		"active": "a",
	})
	for range 2 {
		recorder := f.tabbed(t, http.MethodGet, pathWorkspace, nil)
		var tree struct {
			Tabs []struct {
				ID   string `json:"id"`
				Kind string `json:"kind"`
			} `json:"tabs"`
		}
		decodeInto(t, recorder, &tree)
		if len(tree.Tabs) != 1 || tree.Tabs[0].ID != "a" {
			t.Fatalf("the strip changed between reads: %#v", tree.Tabs)
		}
	}
}

func TestATreeThatCouldNotBeShownIsRefusedWithWhatIsWrong(t *testing.T) {
	f := newFixture(t, nil)
	recorder := f.tabbed(t, http.MethodPut, pathWorkspace, map[string]any{
		"tabs": []map[string]string{{"id": "x", "kind": "wormhole"}},
	})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("PUT a tree with a kind nothing can render = %d, want 400: %s",
			recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "wormhole") {
		t.Fatalf("the refusal does not say what is wrong: %s", recorder.Body.String())
	}
}

func TestTheStripIsNotWrittenByAGet(t *testing.T) {
	f := newFixture(t, nil)
	recorder := f.tabbed(t, http.MethodDelete, pathWorkspace, nil)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE the strip = %d, want 405: %s", recorder.Code, recorder.Body.String())
	}
}

// configured is a fixture with a model configured, because most of what a window
// does is on a machine that has one.
func configured(t *testing.T, provider core.Provider) *fixture {
	t.Helper()
	f := newFixture(t, provider)
	encoded := `{"model":{"default":"scripted:fake","providers":{
		"scripted":{"endpoint":"http://127.0.0.1:1","format":"openai-completions"}}}}`
	var update config.Update
	if err := json.Unmarshal([]byte(encoded), &update); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := f.settings.Apply(update); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return f
}

func (f *fixture) call(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	recorder := f.do(t, method, path, body)
	switch recorder.Code {
	case http.StatusOK, http.StatusCreated, http.StatusAccepted:
	default:
		t.Fatalf("%s %s = %d: %s", method, path, recorder.Code, recorder.Body.String())
	}
	return recorder
}

// do makes a request and hands back whatever came, status included.
func (f *fixture) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode the body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

// want is a request whose answer is a particular status, which is how the refusals
// are tested: a 404 that has been checked is a 404, and a page that cannot tell a
// refusal from a failure is not something these tests would catch.
func (f *fixture) want(t *testing.T, method, path string, body any, want int) *httptest.ResponseRecorder {
	t.Helper()
	recorder := f.do(t, method, path, body)
	if recorder.Code != want {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, recorder.Code, want, recorder.Body.String())
	}
	return recorder
}

func decodeInto(t *testing.T, recorder *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), target); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
}

func TestTheConfigurationIsReadAndWritten(t *testing.T) {
	f := newFixture(t, nil)

	recorder := f.call(t, http.MethodGet, pathConfig, nil)
	var view struct {
		Formats []struct {
			Value string `json:"value"`
		} `json:"formats"`
		GlobalPath string `json:"global_path"`
	}
	decodeInto(t, recorder, &view)
	if len(view.Formats) != 3 {
		t.Fatalf("formats = %v, want the three the gateway knows", view.Formats)
	}
	if view.GlobalPath == "" {
		t.Fatal("the view does not say where a save goes")
	}

	update := map[string]any{"model": map[string]any{
		"default": "ollama:qwen3",
		"providers": map[string]any{
			"ollama": map[string]any{
				"endpoint": "http://127.0.0.1:11434",
				"format":   "openai-completions",
				"models": []map[string]any{
					{"id": "qwen3"},
					{"id": "qwen3:8b", "declared_window": true, "context_window": 32768},
				},
			},
		},
	}}
	recorder = f.call(t, http.MethodPut, pathConfig, update)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT config = %d: %s", recorder.Code, recorder.Body.String())
	}
	// The saved configuration comes back, so a page that saved something knows
	// what was stored rather than having to ask again.
	var saved struct {
		Model struct {
			Default   string `json:"default"`
			Providers map[string]struct {
				Name    string `json:"name"`
				Enabled bool   `json:"enabled"`
				Models  []struct {
					ID             string `json:"id"`
					ContextWindow  int    `json:"context_window"`
					DeclaredWindow bool   `json:"declared_window"`
				} `json:"models"`
			} `json:"providers"`
		} `json:"model"`
	}
	decodeInto(t, recorder, &saved)
	if saved.Model.Default != "ollama:qwen3" {
		t.Fatalf("default = %q after the save", saved.Model.Default)
	}
	provider := saved.Model.Providers["ollama"]
	if len(provider.Models) != 2 {
		t.Fatalf("models = %d after the save, want 2", len(provider.Models))
	}
	// A model that declared nothing and a model that declared a window are told
	// apart, because the second is a fact the first one does not have.
	if provider.Models[0].DeclaredWindow || provider.Models[0].ContextWindow != config.DefaultContextWindow {
		t.Fatalf("a model that declared nothing came back as %+v", provider.Models[0])
	}
	if !provider.Models[1].DeclaredWindow || provider.Models[1].ContextWindow != 32768 {
		t.Fatalf("a model that declared a window came back as %+v", provider.Models[1])
	}
}

func TestAConfigurationThatWouldLeaveTheDefaultPointingAtNothingIsRefused(t *testing.T) {
	f := newFixture(t, nil)
	recorder := f.do(t, http.MethodPut, pathConfig, map[string]any{"model": map[string]any{
		"default":   "ollama:qwen3",
		"providers": map[string]any{},
	}})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("PUT config = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
}

func TestAMethodAnAddressDoesNotServeSaysWhichOnesItDoes(t *testing.T) {
	f := newFixture(t, nil)
	recorder := f.do(t, http.MethodDelete, pathConfig, nil)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE config = %d, want 405", recorder.Code)
	}
	if allow := recorder.Header().Get("Allow"); allow == "" {
		t.Fatal("a 405 with no Allow header leaves the caller guessing what to try")
	}
}

func TestAConversationIsCreatedListedAndOpened(t *testing.T) {
	f := newFixture(t, nil)

	recorder := f.call(t, http.MethodPost, pathSessions, map[string]any{"title": "a conversation"})
	if recorder.Code != http.StatusCreated {
		t.Fatalf("POST sessions = %d: %s", recorder.Code, recorder.Body.String())
	}
	var created struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Mode  string `json:"mode"`
	}
	decodeInto(t, recorder, &created)
	if created.ID == "" {
		t.Fatal("a created conversation has no identifier")
	}
	if created.State != "idle" || created.Mode != "chat" {
		t.Fatalf("created = %#v, want an idle chat conversation", created)
	}

	recorder = f.call(t, http.MethodGet, pathSessions, nil)
	var listed struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	decodeInto(t, recorder, &listed)
	if len(listed.Sessions) != 1 || listed.Sessions[0].ID != created.ID {
		t.Fatalf("listed = %#v, want the created conversation", listed)
	}

	recorder = f.call(t, http.MethodGet, pathSessions+"/"+created.ID, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET session = %d: %s", recorder.Code, recorder.Body.String())
	}
	var opened struct {
		Info struct {
			ID string `json:"id"`
		} `json:"info"`
		Messages []any `json:"messages"`
	}
	decodeInto(t, recorder, &opened)
	if opened.Info.ID != created.ID {
		t.Fatalf("opened = %#v, want the conversation asked for", opened)
	}
	// An empty array rather than null, because a window that received null has to
	// decide what to do about it.
	if !bytes.Contains(recorder.Body.Bytes(), []byte(`"messages":[]`)) {
		t.Fatalf("an empty conversation did not answer with an empty array: %s", recorder.Body.String())
	}
}

func TestAMessageIsAcceptedAndTheAnswerArrivesOnTheStream(t *testing.T) {
	provider := &scriptedProvider{chunks: []core.StreamChunk{
		{Type: core.StreamTypeText, Text: "answered"},
		{Type: core.StreamTypeDone},
	}}
	f := configured(t, provider)
	var created struct {
		ID string `json:"id"`
	}
	decodeInto(t, f.call(t, http.MethodPost, pathSessions, map[string]any{}), &created)

	recorder := f.call(t, http.MethodPost, "/v1/sessions/"+created.ID+"/messages",
		map[string]any{"text": "a question"})
	// Accepted rather than finished: the answer arrives on the event stream, and
	// a window that waited here would hold a request open for as long as a model
	// takes to answer.
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("POST message = %d: %s", recorder.Code, recorder.Body.String())
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		messages, err := f.runtime.Messages(created.ID)
		if err == nil && len(messages) == 2 {
			if text := textOf(messages[1]); text != "answered" {
				t.Fatalf("answer = %q", text)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the answer")
}

func TestAMessageWithNoModelIsRefusedWithSomethingAWindowCanActOn(t *testing.T) {
	f := newFixture(t, &scriptedProvider{})
	var created struct {
		ID string `json:"id"`
	}
	decodeInto(t, f.do(t, http.MethodPost, pathSessions, map[string]any{}), &created)

	recorder := f.do(t, http.MethodPost, "/v1/sessions/"+created.ID+"/messages",
		map[string]any{"text": "a question"})
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("POST message = %d, want 422 so the window can open the settings page: %s",
			recorder.Code, recorder.Body.String())
	}
}

func TestAnEmptyMessageIsRefused(t *testing.T) {
	f := configured(t, &scriptedProvider{})
	var created struct {
		ID string `json:"id"`
	}
	decodeInto(t, f.do(t, http.MethodPost, pathSessions, map[string]any{}), &created)
	recorder := f.do(t, http.MethodPost, "/v1/sessions/"+created.ID+"/messages",
		map[string]any{"text": "   "})
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("POST an empty message = %d, want 400", recorder.Code)
	}
}

func TestAConversationThatIsNotThereIsANotFound(t *testing.T) {
	f := newFixture(t, nil)
	for _, path := range []string{
		"/v1/sessions/missing",
		"/v1/sessions/missing/messages",
	} {
		recorder := f.do(t, http.MethodGet, path, nil)
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", path, recorder.Code)
		}
	}
	recorder := f.do(t, http.MethodPost, "/v1/sessions/missing/interrupt", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("POST interrupt = %d, want 404", recorder.Code)
	}
}

func TestAnIdentifierThatIsAPathIsRefused(t *testing.T) {
	f := newFixture(t, nil)
	// The identifier reaches a directory name on the other side, so one carrying
	// a separator is one that could name a directory somebody did not mean.
	recorder := f.do(t, http.MethodGet, "/v1/sessions/"+urlEscape("../../etc")+"/messages", nil)
	if recorder.Code == http.StatusOK {
		t.Fatalf("a path was accepted as a session identifier: %s", recorder.Body.String())
	}
}

func TestInterruptingAConversationIsAnsweredEvenWhenNothingIsRunning(t *testing.T) {
	f := newFixture(t, nil)
	var created struct {
		ID string `json:"id"`
	}
	decodeInto(t, f.call(t, http.MethodPost, pathSessions, map[string]any{}), &created)
	recorder := f.call(t, http.MethodPost, "/v1/sessions/"+created.ID+"/interrupt", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST interrupt = %d: %s", recorder.Code, recorder.Body.String())
	}
	// A second press is not an error either, because a window whose stop arrived
	// after the turn ended has done nothing wrong.
	if again := f.call(t, http.MethodPost, "/v1/sessions/"+created.ID+"/interrupt", nil); again.Code != http.StatusOK {
		t.Fatalf("a second interrupt = %d, want 200", again.Code)
	}
}

func TestABodyThatIsNotJSONIsRefusedRatherThanIgnored(t *testing.T) {
	f := newFixture(t, nil)
	request := httptest.NewRequest(http.MethodPost, pathSessions, bytes.NewReader([]byte("{not json")))
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("POST a broken body = %d, want 400", recorder.Code)
	}
}

func TestAnEmptyBodyCreatesAConversationRatherThanFailing(t *testing.T) {
	f := newFixture(t, nil)
	recorder := f.call(t, http.MethodPost, pathSessions, nil)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("POST an empty body = %d, want 201: %s", recorder.Code, recorder.Body.String())
	}
}

func TestTheModelsAProviderOffersAreListed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen3:8b"},{"id":"qwen3"}]}`))
	}))
	defer server.Close()

	f := newFixture(t, nil)
	recorder := f.call(t, http.MethodPut, pathConfig, map[string]any{"model": map[string]any{
		"default": "local:qwen3",
		"providers": map[string]any{
			"local": map[string]any{"endpoint": server.URL + "/v1", "format": "openai-completions"},
		},
	}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT config = %d: %s", recorder.Code, recorder.Body.String())
	}

	recorder = f.call(t, http.MethodGet, "/v1/providers/local/models", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET models = %d: %s", recorder.Code, recorder.Body.String())
	}
	var listed struct {
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	decodeInto(t, recorder, &listed)
	if len(listed.Models) != 2 || listed.Models[0].ID != "qwen3" {
		t.Fatalf("models = %#v, want them in order", listed.Models)
	}
}

func TestTheCatalogOfKnownProvidersIsServed(t *testing.T) {
	f := newFixture(t, nil)
	recorder := f.call(t, http.MethodGet, pathProviders, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET the catalog = %d: %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Catalog []struct {
			Name     string `json:"name"`
			Slug     string `json:"slug"`
			Endpoint string `json:"endpoint"`
			Format   string `json:"format"`
		} `json:"catalog"`
	}
	decodeInto(t, recorder, &body)
	if len(body.Catalog) == 0 {
		t.Fatal("the catalog is empty, so the menu that offers it has nothing to offer")
	}
	for _, entry := range body.Catalog {
		if entry.Name == "" || entry.Slug == "" || entry.Endpoint == "" || entry.Format == "" {
			t.Fatalf("catalog entry %#v could not be turned into a provider", entry)
		}
	}
}

func TestTheCatalogIsAGetAndNothingElse(t *testing.T) {
	f := newFixture(t, nil)
	if recorder := f.do(t, http.MethodPost, pathProviders, map[string]any{}); recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST the catalog = %d, want 405", recorder.Code)
	}
}

func TestAProviderIsTestedForRealRatherThanAssumed(t *testing.T) {
	var served atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, frame := range []string{
			`data: {"id":"1","choices":[{"delta":{"content":"ok"}}]}`,
			"data: [DONE]",
		} {
			_, _ = fmt.Fprint(w, frame+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer server.Close()

	f := newFixture(t, nil)
	f.call(t, http.MethodPut, pathConfig, map[string]any{"model": map[string]any{
		"default": "local:m",
		"providers": map[string]any{
			"local": map[string]any{
				"endpoint": server.URL + "/v1", "format": "openai-completions",
				"models": []map[string]any{{"id": "m", "declared_window": true, "context_window": 32768}},
			},
		},
	}})

	recorder := f.call(t, http.MethodPost, "/v1/providers/local/test", map[string]any{"model": "m"})
	var result struct {
		OK            bool   `json:"ok"`
		LatencyMS     int64  `json:"latency_ms"`
		ContextWindow int    `json:"context_window"`
		Error         string `json:"error"`
	}
	decodeInto(t, recorder, &result)
	if !result.OK {
		t.Fatalf("a working provider was reported as failing: %q", result.Error)
	}
	if served.Load() == 0 {
		t.Fatal("the provider was never asked, so nothing was actually tested")
	}
	if result.ContextWindow != 32768 {
		t.Fatalf("context window = %d, want the one the configuration declares", result.ContextWindow)
	}
}

func TestAProviderThatCannotBeReachedIsReportedAsAFailureAndNotAsAnError(t *testing.T) {
	f := newFixture(t, nil)
	f.call(t, http.MethodPut, pathConfig, map[string]any{"model": map[string]any{
		"default": "gone:m",
		"providers": map[string]any{
			"gone": map[string]any{"endpoint": "http://127.0.0.1:1", "format": "openai-completions",
				"models": []map[string]any{{"id": "m"}}},
		},
	}})

	// The page asked a question whose answers are yes and no. A status code that
	// said the question could not be asked would be a third answer, and a page
	// that has to tell them apart has more error handling than the question does.
	recorder := f.call(t, http.MethodPost, "/v1/providers/gone/test", map[string]any{"model": "m"})
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	decodeInto(t, recorder, &result)
	if result.OK {
		t.Fatal("an endpoint that refuses connections was reported as working")
	}
	if result.Error == "" {
		t.Fatal("the failure carries no reason, so there is nothing to act on")
	}
}

func TestTestingAProviderThatIsNotConfiguredSaysSo(t *testing.T) {
	f := newFixture(t, nil)
	recorder := f.call(t, http.MethodPost, "/v1/providers/nope/test", map[string]any{"model": "m"})
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	decodeInto(t, recorder, &result)
	if result.OK || result.Error == "" {
		t.Fatalf("result = %#v, want a refusal with a reason", result)
	}
}

func TestAProviderCanBeProbedBeforeItIsSaved(t *testing.T) {
	// The person pressing this has usually just typed the address, and a probe that
	// only reached saved providers would fail on exactly the one being set up.
	var served atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, frame := range []string{
			`data: {"id":"1","choices":[{"delta":{"content":"ok"}}]}`,
			"data: [DONE]",
		} {
			_, _ = fmt.Fprint(w, frame+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer server.Close()

	// Nothing has been saved: the configuration is empty, and the probe still has
	// to work because the provider came whole in the request.
	f := newFixture(t, nil)
	recorder := f.call(t, http.MethodPost, "/v1/providers/test", map[string]any{
		"name":     "draft",
		"endpoint": server.URL + "/v1",
		"format":   "openai-completions",
		"model":    "m",
	})
	var result struct {
		OK bool `json:"ok"`
	}
	decodeInto(t, recorder, &result)
	if !result.OK {
		t.Fatalf("a provider that had not been saved could not be probed: %#v", result)
	}
	if served.Load() == 0 {
		t.Fatal("the probe never reached the endpoint, so it proved nothing")
	}
}

func TestAProviderCanBeListedBeforeItIsSaved(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"a"},{"id":"b"}]}`))
	}))
	defer server.Close()

	f := newFixture(t, nil)
	recorder := f.call(t, http.MethodPost, "/v1/providers/models", map[string]any{
		"name":     "draft",
		"endpoint": server.URL + "/v1",
		"format":   "openai-completions",
	})
	var body struct {
		Models []map[string]any `json:"models"`
	}
	decodeInto(t, recorder, &body)
	if len(body.Models) != 2 {
		t.Fatalf("models = %#v, want the endpoint's own two", body.Models)
	}
}

func TestAProviderThatIsNotConfiguredIsRefused(t *testing.T) {
	f := newFixture(t, nil)
	recorder := f.call(t, http.MethodPost, "/v1/providers/test", map[string]any{
		"name": "nope", "model": "m",
	})
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	decodeInto(t, recorder, &result)
	if result.OK {
		t.Fatal("a provider that is nowhere was reported as working")
	}
	if !strings.Contains(result.Error, "nope") {
		t.Fatalf("error = %q, want it to name what was not found", result.Error)
	}
}

func TestAModelListingForAProviderThatIsNotConfiguredIsRefused(t *testing.T) {
	f := newFixture(t, nil)
	recorder := f.do(t, http.MethodGet, "/v1/providers/nope/models", nil)
	if recorder.Code == http.StatusOK {
		t.Fatalf("models were listed for a provider that is not configured: %s", recorder.Body.String())
	}
}

func TestAModelListingForAProviderWithNoKeySaysWhichVariableIsMissing(t *testing.T) {
	f := newFixture(t, nil)
	recorder := f.do(t, http.MethodPut, pathConfig, map[string]any{"model": map[string]any{
		"default": "remote:m",
		"providers": map[string]any{
			"remote": map[string]any{
				"endpoint":    "http://127.0.0.1:1",
				"format":      "anthropic-messages",
				"api_key_env": "MARXAGENT_TEST_ABSENT_KEY",
			},
		},
	}})
	if recorder.Code != http.StatusOK {
		t.Fatalf("PUT config = %d: %s", recorder.Code, recorder.Body.String())
	}
	recorder = f.do(t, http.MethodGet, "/v1/providers/remote/models", nil)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("GET models = %d, want 502: %s", recorder.Code, recorder.Body.String())
	}
	// The message has to name the variable, because "the request failed" and
	// "this variable is not set" have different fixes and only one of them is a
	// setting on this page.
	if !bytes.Contains(recorder.Body.Bytes(), []byte("MARXAGENT_TEST_ABSENT_KEY")) {
		t.Fatalf("the answer does not name the variable: %s", recorder.Body.String())
	}
}

func TestAnAddressTheHandlerDoesNotClaimIsNotFound(t *testing.T) {
	f := newFixture(t, nil)
	recorder := f.do(t, http.MethodGet, "/v1/sessions/one/nonsense", nil)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("GET an unknown address = %d, want 404", recorder.Code)
	}
}

func textOf(message core.Message) string {
	var text string
	for _, block := range message.Content {
		if block.Type == core.ContentTypeText {
			text += block.Text
		}
	}
	return text
}

func urlEscape(value string) string {
	escaped := ""
	for _, character := range []byte(value) {
		switch {
		case character == '.', character == '-', character == '_', character == '~':
			escaped += string(character)
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9':
			escaped += string(character)
		default:
			escaped += fmt.Sprintf("%%%02X", character)
		}
	}
	return escaped
}
