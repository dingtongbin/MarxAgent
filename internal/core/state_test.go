// SPDX-License-Identifier: Apache-2.0

package core

import (
	"encoding/json"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestStateSnapshotsAndViewsAreDeeplyImmutable(t *testing.T) {
	variables := map[string]any{
		"nested": map[string]any{"items": []any{"one", "two"}},
		"number": 7,
	}
	metadata := map[string]any{"labels": []any{"test"}}
	message := validTestMessage("message-1")
	message.Content[0].Input = json.RawMessage(`{"value":1}`)
	message.Metadata = map[string]any{"source": map[string]any{"name": "test"}}
	state := newState(Config{
		AgentID:   "agent-1",
		ParentID:  "parent-1",
		Variables: variables,
		Metadata:  metadata,
		InitialMessages: []Message{
			message,
		},
	})

	variables["nested"].(map[string]any)["items"].([]any)[0] = "changed"
	metadata["labels"].([]any)[0] = "changed"
	message.Content[0].Input[2] = '9'
	message.Metadata["source"].(map[string]any)["name"] = "changed"

	snapshot := state.snapshot()
	if got := snapshot.Variables["nested"].(map[string]any)["items"].([]any)[0]; got != "one" {
		t.Fatalf("state variable mutated through input: %v", got)
	}
	if got := snapshot.Metadata["labels"].([]any)[0]; got != "test" {
		t.Fatalf("state metadata mutated through input: %v", got)
	}
	if got := string(snapshot.Messages[0].Content[0].Input); got != `{"value":1}` {
		t.Fatalf("message input mutated through input: %s", got)
	}
	if got := snapshot.Messages[0].Metadata["source"].(map[string]any)["name"]; got != "test" {
		t.Fatalf("message metadata mutated through input: %v", got)
	}

	appended := validTestMessage("message-2")
	state.appendMessage(appended)
	appended.Content[0].Text = "changed"
	state.appendMessages([]Message{validTestMessage("message-3")})
	if got := state.snapshot().Messages[1].Content[0].Text; got != "message-2" {
		t.Fatalf("appended message was not cloned: %q", got)
	}

	view := &stateView{snapshot: state.snapshot()}
	messages := view.Messages()
	messages[0].Content[0].Text = "view mutation"
	variablesFromView := view.Variables()
	variablesFromView["number"] = 99
	metadataFromView := view.Metadata()
	metadataFromView["labels"].([]any)[0] = "view mutation"
	if view.Messages()[0].Content[0].Text == "view mutation" ||
		view.Variables()["number"] != 7 ||
		view.Metadata()["labels"].([]any)[0] == "view mutation" {
		t.Fatal("StateView exposed mutable internal data")
	}
	if view.AgentID() != "agent-1" || view.ParentID() != "parent-1" {
		t.Fatalf("StateView IDs = %q, %q", view.AgentID(), view.ParentID())
	}
}

func TestStateHandlesNilContentAndStorageGrowth(t *testing.T) {
	state := newState(Config{
		AgentID: "agent",
		InitialMessages: []Message{{
			ID:        "nil-content",
			Role:      RoleUser,
			CreatedAt: time.Now().UTC(),
		}},
	})
	state.appendMessage(Message{
		ID:        "nil-appended",
		Role:      RoleUser,
		CreatedAt: time.Now().UTC(),
	})
	state.appendMessage(validTestMessage("content"))
	state.appendMessage(Message{
		ID:        "metadata",
		Role:      RoleUser,
		CreatedAt: time.Now().UTC(),
		Content: []ContentBlock{{
			Type:  ContentTypeToolUse,
			Input: json.RawMessage(`{"value":1}`),
		}},
		Metadata: map[string]any{"source": "test"},
	})
	snapshot := state.snapshot()
	if len(snapshot.Messages) != 4 || snapshot.Messages[0].Content != nil || snapshot.Messages[1].Content != nil {
		t.Fatalf("nil-content state snapshot = %#v", snapshot.Messages)
	}
	if string(snapshot.Messages[3].Content[0].Input) != `{"value":1}` {
		t.Fatalf("raw content was not preserved: %#v", snapshot.Messages[3])
	}
}

func TestStateConcurrentSnapshotsAndAppends(t *testing.T) {
	state := newState(Config{AgentID: "agent"})
	var waitGroup sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		waitGroup.Add(2)
		go func() {
			defer waitGroup.Done()
			for index := 0; index < 100; index++ {
				state.appendMessage(validTestMessage(time.Now().String()))
			}
		}()
		go func() {
			defer waitGroup.Done()
			for index := 0; index < 100; index++ {
				_ = state.snapshot()
			}
		}()
	}
	waitGroup.Wait()
	if got := len(state.snapshot().Messages); got != 400 {
		t.Fatalf("message count = %d, want 400", got)
	}
}

func TestValidateJSONCompatible(t *testing.T) {
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	tests := []struct {
		name    string
		value   any
		wantErr bool
	}{
		{name: "nil", value: nil},
		{name: "bool", value: true},
		{name: "integer", value: 1},
		{name: "float", value: 1.5},
		{name: "string", value: "value"},
		{name: "slice", value: []any{1, "two", false}},
		{name: "array", value: [2]int{1, 2}},
		{name: "bytes", value: []byte("value")},
		{name: "map", value: map[string]any{"key": []any{1}}},
		{name: "typed map", value: map[string]string{"key": "value"}},
		{name: "function", value: func() {}, wantErr: true},
		{name: "channel", value: make(chan struct{}), wantErr: true},
		{name: "struct", value: time.Time{}, wantErr: true},
		{name: "integer map key", value: map[int]string{1: "value"}, wantErr: true},
		{name: "not finite", value: math.NaN(), wantErr: true},
		{name: "cycle", value: cyclic, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateJSONCompatible(test.value)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateJSONCompatible() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestCloneHelpersPreserveNilAndCredentialBytes(t *testing.T) {
	if cloneRawJSON(nil) != nil {
		t.Fatal("cloneRawJSON(nil) must remain nil")
	}
	empty := json.RawMessage{}
	clonedEmpty := cloneRawJSON(empty)
	if clonedEmpty == nil || len(clonedEmpty) != 0 {
		t.Fatalf("cloneRawJSON(empty) = %#v", clonedEmpty)
	}
	if cloneMessages(nil) != nil || cloneContentBlocks(nil) != nil || cloneMetadata(nil) != nil {
		t.Fatal("nil clone helpers must remain nil")
	}

	raw := json.RawMessage(`{"encrypted":"exact"}`)
	clonedRaw := cloneRawJSON(raw)
	raw[2] = 'x'
	if string(clonedRaw) != `{"encrypted":"exact"}` {
		t.Fatalf("raw JSON clone = %s", clonedRaw)
	}
	block := ContentBlock{
		Type:             ContentTypeReasoning,
		EncryptedContent: "encrypted",
		ThinkingSig:      "signature",
		ResponseID:       "response",
	}
	cloned := cloneContentBlocks([]ContentBlock{block})
	if !reflect.DeepEqual(cloned[0], block) {
		t.Fatalf("credential block clone = %#v, want %#v", cloned[0], block)
	}

	mixedMessages := []Message{
		{ID: "nil-content"},
		{ID: "empty-content", Content: []ContentBlock{}},
		validTestMessage("with-content"),
	}
	clonedMessages := cloneMessages(mixedMessages)
	if clonedMessages[0].Content != nil || clonedMessages[1].Content == nil || len(clonedMessages[2].Content) != 1 {
		t.Fatalf("cloneMessages() nil shape = %#v", clonedMessages)
	}
	clonedMessages[2].Content[0].Text = "changed"
	if mixedMessages[2].Content[0].Text == "changed" {
		t.Fatal("batched content clone shares message storage")
	}
	if cloneMessages([]Message{{ID: "empty"}})[0].Content != nil {
		t.Fatal("zero-content message should preserve a nil content slice")
	}
}

func TestStateChangeReason(t *testing.T) {
	if got := stateChangeReason(validTestMessage("id")); got != "user_message_appended" {
		t.Fatalf("stateChangeReason() = %q", got)
	}
}

func TestCloneReflectValueCoversContainerShapes(t *testing.T) {
	if cloneValue(nil) != nil {
		t.Fatal("cloneValue(nil) must remain nil")
	}
	if cloneReflectValue(reflect.ValueOf(nil)).IsValid() {
		t.Fatal("invalid reflect.Value must remain invalid")
	}
	var interfaceHolder any
	interfaceValue := reflect.ValueOf(&interfaceHolder).Elem()
	if !cloneReflectValue(interfaceValue).IsNil() {
		t.Fatal("nil interface clone must remain nil")
	}
	var nilMap map[string]any
	if cloneValue(nilMap).(map[string]any) != nil {
		t.Fatal("nil map clone must remain nil")
	}
	var nilSlice []any
	if cloneValue(nilSlice).([]any) != nil {
		t.Fatal("nil slice clone must remain nil")
	}
	array := [2]any{[]any{"value"}, map[string]any{"key": "value"}}
	clonedArray := cloneValue(array).([2]any)
	clonedArray[0].([]any)[0] = "changed"
	clonedArray[1].(map[string]any)["key"] = "changed"
	if array[0].([]any)[0] != "value" || array[1].(map[string]any)["key"] != "value" {
		t.Fatal("array clone shares nested mutable values")
	}
	if cloneReflectValue(reflect.ValueOf(42)).Int() != 42 {
		t.Fatal("primitive clone changed value")
	}
	if err := validateJSONCompatible(map[string]any{"time": time.Now()}); err == nil {
		t.Fatal("nested unsupported JSON value was accepted")
	}
	if err := validateJSONCompatible([]any{time.Now()}); err == nil {
		t.Fatal("unsupported value inside a slice was accepted")
	}
	if err := validateJSONReflectValue(interfaceValue); err != nil {
		t.Fatalf("nil interface validation error = %v", err)
	}
}
