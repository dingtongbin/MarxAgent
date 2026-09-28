// SPDX-License-Identifier: Apache-2.0

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/assembly/config"
	"github.com/dingtongbin/MarxAgent/internal/assembly/ui/web"
	"github.com/dingtongbin/MarxAgent/internal/core"
	"github.com/dingtongbin/MarxAgent/internal/engine/storage"
)

// scriptedProvider answers every request with a fixed script of chunks, so a test
// can say what the model said rather than what a model would have said.
type scriptedProvider struct {
	chunks []core.StreamChunk
	err    error
	calls  atomic.Int32
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) ChatStream(ctx context.Context, _ core.ChatRequest) (<-chan core.StreamChunk, error) {
	p.calls.Add(1)
	if p.err != nil {
		return nil, p.err
	}
	out := make(chan core.StreamChunk, len(p.chunks))
	for _, chunk := range p.chunks {
		out <- chunk
	}
	close(out)
	return out, nil
}

func textChunks(parts ...string) []core.StreamChunk {
	chunks := make([]core.StreamChunk, 0, len(parts)+1)
	for _, part := range parts {
		chunks = append(chunks, core.StreamChunk{Type: core.StreamTypeText, Text: part})
	}
	return append(chunks, core.StreamChunk{Type: core.StreamTypeDone})
}

// newTestRuntime builds a runtime over temporary directories, with an identifier
// source a test can predict.
func newTestRuntime(t *testing.T) (*Runtime, *config.Store) {
	t.Helper()
	directory := t.TempDir()
	appData, err := storage.OpenScope(storage.ScopeAppData, filepath.Join(directory, "appdata"))
	if err != nil {
		t.Fatalf("open app data scope: %v", err)
	}
	settings, err := config.OpenAt(filepath.Join(directory, "config", config.FileName), directory)
	if err != nil {
		t.Fatalf("open config: %v", err)
	}
	counter := atomic.Int32{}
	sessions, err := New(Options{
		Config:       settings,
		AppDataScope: appData,
		NewID: func() (string, error) {
			return fmt.Sprintf("session-%d", counter.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sessions.Close(ctx)
	})
	return sessions, settings
}

// configuredRuntime is a runtime whose model is a script rather than an endpoint.
//
// The configuration is still written and still resolved, because the parts of a
// turn that have nothing to do with the transport are exactly the parts worth
// testing: which model a conversation ended up on, what happens with none
// configured, and what a window is told.
func configuredRuntime(t *testing.T, provider core.Provider) (*Runtime, *config.Store) {
	t.Helper()
	directory := t.TempDir()
	appData, err := storage.OpenScope(storage.ScopeAppData, filepath.Join(directory, "appdata"))
	if err != nil {
		t.Fatalf("open app data scope: %v", err)
	}
	settings, err := config.OpenAt(filepath.Join(directory, "config", config.FileName), directory)
	if err != nil {
		t.Fatalf("open config: %v", err)
	}
	encoded := `{"model":{"default":"scripted:fake","providers":{
		"scripted":{"endpoint":"http://127.0.0.1:1","format":"openai-completions"}}}}`
	var update config.Update
	if err := json.Unmarshal([]byte(encoded), &update); err != nil {
		t.Fatalf("decode the update: %v", err)
	}
	if err := settings.Apply(update); err != nil {
		t.Fatalf("apply: %v", err)
	}
	counter := atomic.Int32{}
	sessions, err := New(Options{
		Config:       settings,
		AppDataScope: appData,
		Gateway: func(config.ModelRef) (core.Provider, error) {
			return provider, nil
		},
		NewID: func() (string, error) {
			return fmt.Sprintf("session-%d", counter.Add(1)), nil
		},
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = sessions.Close(ctx)
	})
	return sessions, settings
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestASessionIsListedOnceItIsCreated(t *testing.T) {
	sessions, _ := newTestRuntime(t)

	created, err := sessions.Create(CreateRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("a created session has no identifier")
	}
	listed := sessions.List()
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("listed = %#v, want the created session", listed)
	}
	if listed[0].State != StateIdle {
		t.Fatalf("state = %q, want idle", listed[0].State)
	}
}

func TestAConversationWithADirectoryIsACodingSessionAndWithoutOneIsAChatSession(t *testing.T) {
	sessions, _ := newTestRuntime(t)
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatalf("create the project: %v", err)
	}

	coding, err := sessions.Create(CreateRequest{Project: project})
	if err != nil {
		t.Fatalf("create the coding session: %v", err)
	}
	chat, err := sessions.Create(CreateRequest{})
	if err != nil {
		t.Fatalf("create the chat session: %v", err)
	}
	if coding.Mode != ModeCoding {
		t.Fatalf("mode = %q, want coding", coding.Mode)
	}
	if chat.Mode != ModeChat {
		t.Fatalf("mode = %q, want chat", chat.Mode)
	}

	// And the data went where the mode says it goes: the coding conversation is
	// in the project, the chat one is in the application data directory.
	if _, err := os.Stat(filepath.Join(project, storage.ProjectDirName, storage.SessionsDirName, coding.ID)); err != nil {
		t.Fatalf("the coding session is not in the project: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessions.options.AppDataScope.DataRoot(), storage.SessionsDirName, chat.ID)); err != nil {
		t.Fatalf("the chat session is not in the application data directory: %v", err)
	}
}

func TestAMessageIsAnsweredAndBothMessagesAreKept(t *testing.T) {
	provider := &scriptedProvider{chunks: textChunks("hello ", "there")}
	sessions, _ := configuredRuntime(t, provider)

	created, err := sessions.Create(CreateRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := sessions.Send(context.Background(), created.ID, SendRequest{Text: "hi"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitFor(t, "the turn to finish", func() bool {
		listed := sessions.List()
		return len(listed) == 1 && listed[0].State == StateCheckpointed
	})

	messages, err := sessions.Messages(created.ID)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want the question and the answer", len(messages))
	}
	if messages[0].Role != core.RoleUser {
		t.Fatalf("first role = %q, want the user's", messages[0].Role)
	}
	if messages[1].Role != core.RoleAssistant {
		t.Fatalf("second role = %q, want the assistant's", messages[1].Role)
	}
	if text := messageText(messages[1]); text != "hello there" {
		t.Fatalf("answer = %q, want the two chunks joined", text)
	}

	// And the title came from the first thing said, because that is what a person
	// recognises a conversation by in a list.
	listed := sessions.List()
	if listed[0].Title != "hi" {
		t.Fatalf("title = %q, want the first message", listed[0].Title)
	}
}

func TestAConversationCarriesItsHistoryIntoTheNextTurn(t *testing.T) {
	provider := &scriptedProvider{chunks: textChunks("ok")}
	sessions, _ := configuredRuntime(t, provider)
	created, err := sessions.Create(CreateRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, text := range []string{"first", "second"} {
		if _, err := sessions.Send(context.Background(), created.ID, SendRequest{Text: text}); err != nil {
			t.Fatalf("send %q: %v", text, err)
		}
		waitFor(t, "the turn to finish", func() bool {
			listed := sessions.List()
			return len(listed) == 1 && listed[0].State == StateCheckpointed
		})
	}
	messages, err := sessions.Messages(created.ID)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("messages = %d, want two turns of two", len(messages))
	}
	if provider.calls.Load() != 2 {
		t.Fatalf("provider calls = %d, want one per turn", provider.calls.Load())
	}
}

func TestAConversationIsOnDiskAndComesBackAfterARestart(t *testing.T) {
	provider := &scriptedProvider{chunks: textChunks("remembered")}
	sessions, settings := configuredRuntime(t, provider)
	directory := sessions.options.AppDataScope.DataRoot()
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatalf("create the project: %v", err)
	}

	created, err := sessions.Create(CreateRequest{Project: project})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := sessions.Send(context.Background(), created.ID, SendRequest{Text: "remember this"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitFor(t, "the turn to finish", func() bool {
		listed := sessions.List()
		return len(listed) == 1 && listed[0].State == StateCheckpointed
	})
	closeRuntime(t, sessions)

	// A second runtime over the same directories is what a restart is. If the
	// conversation is not here, the task menu empties itself every time the
	// program is closed, which is the whole thing a journal is for.
	scope, err := storage.OpenScope(storage.ScopeAppData, directory)
	if err != nil {
		t.Fatalf("reopen the app data scope: %v", err)
	}
	restored, err := New(Options{Config: settings, AppDataScope: scope})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = restored.Close(ctx)
	})
	listed := restored.List()
	if len(listed) != 1 {
		t.Fatalf("listed %d sessions after a restart, want 1: %#v", len(listed), listed)
	}
	if listed[0].ID != created.ID || listed[0].Project != project {
		t.Fatalf("restored = %#v, want the same conversation in the same project", listed[0])
	}
	if listed[0].Title != "remember this" {
		t.Fatalf("title = %q after a restart", listed[0].Title)
	}
	messages, err := restored.Messages(created.ID)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %d after a restart, want 2", len(messages))
	}
	if text := messageText(messages[1]); text != "remembered" {
		t.Fatalf("answer = %q after a restart", text)
	}
}

func TestSendingWithNoModelConfiguredIsRefusedWithSomethingToActOn(t *testing.T) {
	sessions, _ := newTestRuntime(t)
	created, err := sessions.Create(CreateRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = sessions.Send(context.Background(), created.ID, SendRequest{Text: "hi"})
	if err == nil {
		t.Fatal("a turn was started with no model configured")
	}
	// The error has to name the thing that is missing, because the window's
	// response to it is to open the settings page.
	if !containsAll(err.Error(), "no model is configured") {
		t.Fatalf("error = %q, want it to say no model is configured", err.Error())
	}
}

func TestASecondMessageWhileOneIsRunningIsRefused(t *testing.T) {
	blocking := make(chan struct{})
	provider := &blockingProvider{release: blocking}
	sessions, _ := configuredRuntime(t, provider)
	created, err := sessions.Create(CreateRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := sessions.Send(context.Background(), created.ID, SendRequest{Text: "one"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitFor(t, "the turn to start", func() bool { return provider.started.Load() > 0 })
	if _, err := sessions.Send(context.Background(), created.ID, SendRequest{Text: "two"}); err == nil {
		t.Fatal("a second turn was started on a conversation that was already running one")
	}
	close(blocking)
	waitFor(t, "the turn to finish", func() bool {
		listed := sessions.List()
		return len(listed) == 1 && listed[0].State == StateCheckpointed
	})
}

func TestInterruptingKeepsWhatWasSaidAndLeavesTheConversationUsable(t *testing.T) {
	release := make(chan struct{})
	provider := &blockingProvider{release: release, before: textChunks("half a th")}
	sessions, _ := configuredRuntime(t, provider)
	created, err := sessions.Create(CreateRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, stop, err := sessions.Subscribe(ctx, created.ID, 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer stop()

	if _, err := sessions.Send(context.Background(), created.ID, SendRequest{Text: "hi"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	// The interruption waits for the model's first piece to have been seen. Waiting
	// for the provider to say it had sent one is not the same thing: a piece can be
	// on its way and not yet read, and a turn stopped in that gap has said nothing
	// at all, which is a real thing but not what this test is about. A partial event
	// is proof the loop has the piece in hand.
	waitForPartial(t, stream)

	if err := sessions.Interrupt(created.ID); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	close(release)
	waitFor(t, "the turn to settle", func() bool {
		listed := sessions.List()
		return len(listed) == 1 && listed[0].State != StateRunning
	})

	messages, err := sessions.Messages(created.ID)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	// The user's message and whatever the model managed to say. A turn that was
	// stopped is a record of what was said, not something to be rolled back.
	if len(messages) < 2 {
		t.Fatalf("messages = %d, want the question and the interrupted answer", len(messages))
	}
	if messages[0].Role != core.RoleUser {
		t.Fatalf("the first message is not the user's")
	}
	if !messages[len(messages)-1].Incomplete {
		t.Fatalf("the last message = %#v, want it marked incomplete", messages[len(messages)-1])
	}

	// And interrupting nothing is not an error, because a window whose stop
	// arrived after the turn ended has done nothing wrong.
	if err := sessions.Interrupt(created.ID); err != nil {
		t.Fatalf("interrupting an idle conversation: %v", err)
	}
	// The conversation is not degraded. Somebody asked for the turn to stop, and a
	// state that says something went wrong would have them looking for a fault in a
	// turn that was stopped on purpose and recorded whole.
	if listed := sessions.List(); len(listed) == 1 && listed[0].State != StateCheckpointed {
		t.Fatalf("the conversation is left %q after a deliberate stop", listed[0].State)
	}
}

func TestAFailingModelLeavesTheConversationDegradedRatherThanGone(t *testing.T) {
	provider := &scriptedProvider{err: fmt.Errorf("the endpoint refused the request")}
	sessions, _ := configuredRuntime(t, provider)
	created, err := sessions.Create(CreateRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := sessions.Send(context.Background(), created.ID, SendRequest{Text: "hi"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitFor(t, "the turn to fail", func() bool {
		listed := sessions.List()
		return len(listed) == 1 && listed[0].State == StateDegraded
	})
	// The question is still there. A conversation whose turn failed is a
	// conversation with a question in it, not an empty row.
	messages, err := sessions.Messages(created.ID)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want the question that was asked", len(messages))
	}
}

func TestAWindowSubscribesAndSeesTheTurn(t *testing.T) {
	provider := &scriptedProvider{chunks: textChunks("watched")}
	sessions, _ := configuredRuntime(t, provider)
	created, err := sessions.Create(CreateRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, stop, err := sessions.Subscribe(ctx, created.ID, 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer stop()

	if _, err := sessions.Send(context.Background(), created.ID, SendRequest{Text: "hi"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	var sawStateChange, sawDone bool
	deadline := time.After(5 * time.Second)
	for !sawDone {
		select {
		case event, open := <-events:
			if !open {
				t.Fatal("the stream ended before the turn did")
			}
			var envelope struct {
				Type string          `json:"type"`
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(event.Payload, &envelope); err != nil {
				t.Fatalf("decode the event: %v", err)
			}
			if envelope.Type == string(core.EventTypeStateChange) {
				sawStateChange = true
			}
			if envelope.Type == string(core.EventTypeDone) {
				sawDone = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for the turn to appear on the stream")
		}
	}
	if !sawStateChange {
		t.Fatal("the stream carried no state change, so a window would render nothing")
	}
	if sessions.Watchers(created.ID) != 1 {
		t.Fatalf("watchers = %d, want the one window", sessions.Watchers(created.ID))
	}
}

func TestTwoWindowsWatchingOneConversationSeeTheSameEvents(t *testing.T) {
	provider := &scriptedProvider{chunks: textChunks("both")}
	sessions, _ := configuredRuntime(t, provider)
	created, err := sessions.Create(CreateRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first, stopFirst, err := sessions.Subscribe(ctx, created.ID, 0)
	if err != nil {
		t.Fatalf("first subscribe: %v", err)
	}
	defer stopFirst()
	second, stopSecond, err := sessions.Subscribe(ctx, created.ID, 0)
	if err != nil {
		t.Fatalf("second subscribe: %v", err)
	}
	defer stopSecond()

	if _, err := sessions.Send(context.Background(), created.ID, SendRequest{Text: "hi"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	firstEvents := drainUntilDone(t, first)
	secondEvents := drainUntilDone(t, second)
	if len(firstEvents) != len(secondEvents) {
		t.Fatalf("the two windows saw %d and %d events", len(firstEvents), len(secondEvents))
	}
	for index := range firstEvents {
		if firstEvents[index].Seq != secondEvents[index].Seq {
			t.Fatalf("event %d differs between the windows: %d vs %d",
				index, firstEvents[index].Seq, secondEvents[index].Seq)
		}
		if string(firstEvents[index].Payload) != string(secondEvents[index].Payload) {
			t.Fatalf("event %d differs between the windows: %s vs %s",
				index, firstEvents[index].Payload, secondEvents[index].Payload)
		}
	}
}

func TestAReconnectingWindowIsSentWhatItMissed(t *testing.T) {
	sessions, _ := newTestRuntime(t)
	created, err := sessions.Create(CreateRequest{Title: "one"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A first window watches, and is told where the conversation has got to.
	//
	// It is also handed one real event, because a window that has seen no event
	// has no position to resume from, and a test of resuming from a position no
	// window would ever hold is a test of something else.
	first, stopFirst, err := sessions.Subscribe(ctx, created.ID, 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if ready := <-first; ready.Seq != 0 {
		t.Fatalf("the opening notice carried position %d, want none", ready.Seq)
	}
	sessions.hub.publishState(SessionInfo{ID: created.ID, Title: "first", State: StateIdle})
	seen := <-first
	stopFirst()

	// Something happens while no window is watching.
	sessions.hub.publishState(SessionInfo{ID: created.ID, Title: "changed", State: StateIdle})

	second, stopSecond, err := sessions.Subscribe(ctx, created.ID, seen.Seq)
	if err != nil {
		t.Fatalf("resubscribe: %v", err)
	}
	defer stopSecond()
	replayed := <-second
	if replayed.Seq <= seen.Seq {
		t.Fatalf("resumed at %d, want past the position the window held (%d)", replayed.Seq, seen.Seq)
	}
	var envelope struct {
		Type    string      `json:"type"`
		Session SessionInfo `json:"session"`
	}
	if err := json.Unmarshal(replayed.Payload, &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Type != "session" || envelope.Session.Title != "changed" {
		t.Fatalf("replayed = %#v, want the state that was missed", envelope)
	}
}

func TestAWindowHoldingAPositionFromAnotherServerIsToldToReload(t *testing.T) {
	sessions, _ := newTestRuntime(t)
	created, err := sessions.Create(CreateRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, stop, err := sessions.Subscribe(ctx, created.ID, 9999)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer stop()
	event := <-events
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(event.Payload, &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Type != "reset" {
		t.Fatalf("event = %#v, want a reset so the window reloads rather than waiting", envelope.Type)
	}
}

func TestSubscribingToAConversationThatIsNotThereIsRefused(t *testing.T) {
	sessions, _ := newTestRuntime(t)
	if _, _, err := sessions.Subscribe(context.Background(), "nope", 0); err == nil {
		t.Fatal("a stream was opened for a conversation that does not exist")
	}
}

func TestClosingTheRuntimeFlushesWhatAConversationStillHeld(t *testing.T) {
	provider := &scriptedProvider{chunks: textChunks("written")}
	sessions, settings := configuredRuntime(t, provider)
	directory := sessions.options.AppDataScope.DataRoot()
	created, err := sessions.Create(CreateRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := sessions.Send(context.Background(), created.ID, SendRequest{Text: "hi"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitFor(t, "the turn to finish", func() bool {
		listed := sessions.List()
		return len(listed) == 1 && listed[0].State == StateCheckpointed
	})
	closeRuntime(t, sessions)

	// The journal is a file a person can read, and it is the copy that has to be
	// there the moment the program is closed rather than a moment after.
	journalPath := filepath.Join(directory, storage.SessionsDirName, created.ID, storage.StreamSession+".jsonl")
	records, partial, err := storage.ReadJournal(journalPath)
	if err != nil {
		t.Fatalf("read the journal: %v", err)
	}
	if len(partial) != 0 {
		t.Fatalf("the journal ends in a half written line: %q", partial)
	}
	if len(records) != 2 {
		t.Fatalf("journal records = %d, want one per message", len(records))
	}
	scope, err := storage.OpenScope(storage.ScopeAppData, directory)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = settings
	restored, err := New(Options{Config: settings, AppDataScope: scope})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = restored.Close(ctx)
}

func TestAskingForAConversationThatIsNotThereIsRefused(t *testing.T) {
	sessions, _ := newTestRuntime(t)
	if _, err := sessions.Get("missing"); err == nil {
		t.Fatal("a conversation that does not exist was returned")
	}
	if _, err := sessions.Messages("../escape"); err == nil {
		t.Fatal("an identifier that is a path was accepted")
	}
}

// waitForPartial waits until the loop has shown a piece of the model's answer.
//
// It is the proof that the loop holds a piece, which is what an interruption
// test needs: a piece on its way from the provider is not yet in the loop, and a
// turn stopped in that gap has said nothing at all.
func waitForPartial(t *testing.T, stream <-chan web.Event) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event, open := <-stream:
			if !open {
				t.Fatal("the stream ended before the model said anything")
			}
			var envelope struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(event.Payload, &envelope); err != nil {
				continue
			}
			if envelope.Type == string(core.EventTypePartial) {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for the model's first piece")
		}
	}
}

func messageText(message core.Message) string {
	var text string
	for _, block := range message.Content {
		if block.Type == core.ContentTypeText {
			text += block.Text
		}
	}
	return text
}

func containsAll(text string, parts ...string) bool {
	for _, part := range parts {
		found := false
		for index := 0; index+len(part) <= len(text); index++ {
			if text[index:index+len(part)] == part {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func closeRuntime(t *testing.T, sessions *Runtime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sessions.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func drainUntilDone(t *testing.T, events <-chan web.Event) []web.Event {
	t.Helper()
	var collected []web.Event
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event, open := <-events:
			if !open {
				return collected
			}
			collected = append(collected, event)
			var envelope struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(event.Payload, &envelope); err == nil &&
				envelope.Type == string(core.EventTypeDone) {
				return collected
			}
		case <-deadline:
			t.Fatal("timed out draining the stream")
		}
	}
}

// blockingProvider streams a prefix and then waits, so a test can look at a turn
// that is still in progress.
//
// The channel is unbuffered on purpose. A buffered one would let the provider
// report "the stream has started" before the loop had read a single chunk, and a
// test that interrupts on that signal would be testing a turn that was cut off
// before it began — which is a real thing, but not the thing the test is about.
type blockingProvider struct {
	before  []core.StreamChunk
	release <-chan struct{}
	started atomic.Int32
}

func (p *blockingProvider) Name() string { return "blocking" }

func (p *blockingProvider) ChatStream(ctx context.Context, _ core.ChatRequest) (<-chan core.StreamChunk, error) {
	out := make(chan core.StreamChunk)
	go func() {
		defer close(out)
		for _, chunk := range p.before {
			select {
			case out <- chunk:
			case <-ctx.Done():
				return
			}
		}
		// Every chunk has been taken, so the model has said something and is now
		// waiting for the rest of the answer.
		p.started.Add(1)
		select {
		case <-p.release:
		case <-ctx.Done():
		}
	}()
	return out, nil
}
