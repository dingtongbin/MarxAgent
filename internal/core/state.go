// SPDX-License-Identifier: Apache-2.0

package core

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
)

// StateView exposes immutable snapshots of one agent's in-memory state.
type StateView interface {
	Messages() []Message
	Variables() map[string]any
	AgentID() string
	ParentID() string
	Metadata() map[string]any
}

// StateSnapshot is the serializable value form of StateView.
type StateSnapshot struct {
	Messages  []Message      `json:"messages"`
	Variables map[string]any `json:"variables"`
	AgentID   string         `json:"agent_id"`
	ParentID  string         `json:"parent_id,omitempty"`
	Metadata  map[string]any `json:"metadata"`
}

type state struct {
	mu   sync.RWMutex
	data StateSnapshot
}

type stateView struct {
	snapshot StateSnapshot
}

func newState(config Config) *state {
	return &state{data: StateSnapshot{
		Messages:  cloneMessages(config.InitialMessages),
		Variables: cloneMetadata(config.Variables),
		AgentID:   config.AgentID,
		ParentID:  config.ParentID,
		Metadata:  cloneMetadata(config.Metadata),
	}}
}

func (s *state) appendMessage(message Message) {
	s.mu.Lock()
	s.data.Messages = append(s.data.Messages, cloneMessage(message))
	s.mu.Unlock()
}

func (s *state) appendMessages(messages []Message) {
	s.mu.Lock()
	s.data.Messages = append(s.data.Messages, cloneMessages(messages)...)
	s.mu.Unlock()
}

func (s *state) snapshot() StateSnapshot {
	s.mu.RLock()
	snapshot := StateSnapshot{
		Messages:  cloneMessages(s.data.Messages),
		Variables: cloneMetadata(s.data.Variables),
		AgentID:   s.data.AgentID,
		ParentID:  s.data.ParentID,
		Metadata:  cloneMetadata(s.data.Metadata),
	}
	s.mu.RUnlock()
	return snapshot
}

func (v *stateView) Messages() []Message {
	return cloneMessages(v.snapshot.Messages)
}

func (v *stateView) Variables() map[string]any {
	return cloneMetadata(v.snapshot.Variables)
}

func (v *stateView) AgentID() string {
	return v.snapshot.AgentID
}

func (v *stateView) ParentID() string {
	return v.snapshot.ParentID
}

func (v *stateView) Metadata() map[string]any {
	return cloneMetadata(v.snapshot.Metadata)
}

func cloneMessage(message Message) Message {
	cloned := message
	cloned.Content = cloneContentBlocks(message.Content)
	cloned.Metadata = cloneMetadata(message.Metadata)
	return cloned
}

const batchedMessageCloneThreshold = 256

func cloneMessages(messages []Message) []Message {
	if messages == nil {
		return nil
	}
	cloned := make([]Message, len(messages))
	if len(messages) < batchedMessageCloneThreshold {
		cloneMessageRange(messages, cloned, nil)
		return cloned
	}
	var allContent []ContentBlock
	if count := countContentBlocks(messages); count > 0 {
		allContent = make([]ContentBlock, count)
	}
	cloneMessageRange(messages, cloned, allContent)
	return cloned
}

func countContentBlocks(messages []Message) int {
	count := 0
	for _, message := range messages {
		count += len(message.Content)
	}
	return count
}

func cloneMessageRange(messages []Message, cloned []Message, allContent []ContentBlock) {
	contentOffset := 0
	for index, message := range messages {
		cloned[index] = message
		if message.Metadata != nil {
			cloned[index].Metadata = cloneMetadata(message.Metadata)
		}
		if message.Content == nil {
			continue
		}
		end := contentOffset + len(message.Content)
		var content []ContentBlock
		if allContent == nil {
			content = make([]ContentBlock, len(message.Content))
		} else {
			content = allContent[contentOffset:end:end]
		}
		copy(content, message.Content)
		for contentIndex := range content {
			if content[contentIndex].Input != nil {
				content[contentIndex].Input = cloneRawJSON(content[contentIndex].Input)
			}
			if content[contentIndex].Output != nil {
				content[contentIndex].Output = cloneRawJSON(content[contentIndex].Output)
			}
		}
		cloned[index].Content = content
		contentOffset = end
	}
}

func cloneContentBlocks(blocks []ContentBlock) []ContentBlock {
	if blocks == nil {
		return nil
	}
	cloned := make([]ContentBlock, len(blocks))
	for index, block := range blocks {
		cloned[index] = block
		cloned[index].Input = cloneRawJSON(block.Input)
		cloned[index].Output = cloneRawJSON(block.Output)
	}
	return cloned
}

func cloneRawJSON(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	cloned := make(json.RawMessage, len(raw))
	copy(cloned, raw)
	return cloned
}

func cloneMetadata(metadata map[string]any) map[string]any {
	if metadata == nil {
		return nil
	}
	cloned := make(map[string]any, len(metadata))
	for key, value := range metadata {
		cloned[key] = cloneValue(value)
	}
	return cloned
}

func cloneValue(value any) any {
	cloned := cloneReflectValue(reflect.ValueOf(value))
	if !cloned.IsValid() {
		return nil
	}
	return cloned.Interface()
}

func cloneReflectValue(value reflect.Value) reflect.Value {
	if !value.IsValid() {
		return reflect.Value{}
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := cloneReflectValue(value.Elem())
		result := reflect.New(value.Type()).Elem()
		result.Set(cloned)
		return result
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			result.SetMapIndex(cloneReflectValue(iterator.Key()), cloneReflectValue(iterator.Value()))
		}
		return result
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		result := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for index := 0; index < value.Len(); index++ {
			result.Index(index).Set(cloneReflectValue(value.Index(index)))
		}
		return result
	case reflect.Array:
		result := reflect.New(value.Type()).Elem()
		for index := 0; index < value.Len(); index++ {
			result.Index(index).Set(cloneReflectValue(value.Index(index)))
		}
		return result
	default:
		return value
	}
}

func validateJSONCompatible(value any) error {
	if _, err := json.Marshal(value); err != nil {
		return fmt.Errorf("value is not JSON serializable: %w", err)
	}
	return validateJSONReflectValue(reflect.ValueOf(value))
}

func validateJSONReflectValue(value reflect.Value) error {
	if !value.IsValid() {
		return nil
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return nil
		}
		return validateJSONReflectValue(value.Elem())
	case reflect.Bool,
		reflect.String,
		reflect.Int,
		reflect.Int8,
		reflect.Int16,
		reflect.Int32,
		reflect.Int64,
		reflect.Uint,
		reflect.Uint8,
		reflect.Uint16,
		reflect.Uint32,
		reflect.Uint64,
		reflect.Float32,
		reflect.Float64:
		return nil
	case reflect.Slice, reflect.Array:
		for index := 0; index < value.Len(); index++ {
			if err := validateJSONReflectValue(value.Index(index)); err != nil {
				return err
			}
		}
		return nil
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("map key type %s is not string", value.Type().Key())
		}
		iterator := value.MapRange()
		for iterator.Next() {
			if err := validateJSONReflectValue(iterator.Value()); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported JSON value kind %s", value.Kind())
	}
}

func stateChangeReason(message Message) string {
	return string(message.Role) + "_message_appended"
}

var _ StateView = (*stateView)(nil)
