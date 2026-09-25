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

type messageContentLayout struct {
	offset int
	length int
}

type state struct {
	mu                 sync.RWMutex
	data               StateSnapshot
	contentStorage     []ContentBlock
	contentLayout      []messageContentLayout
	contentHasRawJSON  bool
	messageHasMetadata bool
}

type stateView struct {
	snapshot StateSnapshot
}

func newState(config Config) *state {
	messages, storage, layout := cloneMessagesWithStorage(config.InitialMessages)
	hasRawJSON, hasMetadata := messageFeatures(config.InitialMessages)
	return &state{
		data: StateSnapshot{
			Messages:  messages,
			Variables: cloneMetadata(config.Variables),
			AgentID:   config.AgentID,
			ParentID:  config.ParentID,
			Metadata:  cloneMetadata(config.Metadata),
		},
		contentStorage:     storage,
		contentLayout:      layout,
		contentHasRawJSON:  hasRawJSON,
		messageHasMetadata: hasMetadata,
	}
}

func (s *state) appendMessage(message Message) {
	s.mu.Lock()
	s.appendMessageLocked(message)
	s.mu.Unlock()
}

func (s *state) appendMessages(messages []Message) {
	s.mu.Lock()
	for _, message := range messages {
		s.appendMessageLocked(message)
	}
	s.mu.Unlock()
}

func (s *state) appendMessageLocked(message Message) {
	cloned := message
	cloned.Metadata = cloneMetadata(message.Metadata)
	if message.Metadata != nil {
		s.messageHasMetadata = true
	}
	for _, block := range message.Content {
		if block.Input != nil || block.Output != nil {
			s.contentHasRawJSON = true
		}
	}
	if message.Content == nil {
		s.data.Messages = append(s.data.Messages, cloned)
		s.contentLayout = append(s.contentLayout, messageContentLayout{})
		return
	}

	required := len(s.contentStorage) + len(message.Content)
	if required > cap(s.contentStorage) {
		storage := make([]ContentBlock, len(s.contentStorage), required+required/2+1)
		copy(storage, s.contentStorage)
		s.contentStorage = storage
		s.rebuildContentPointersLocked()
	}
	offset := len(s.contentStorage)
	s.contentStorage = append(s.contentStorage, message.Content...)
	end := len(s.contentStorage)
	for index := offset; index < end; index++ {
		if s.contentStorage[index].Input != nil {
			s.contentStorage[index].Input = cloneRawJSON(s.contentStorage[index].Input)
		}
		if s.contentStorage[index].Output != nil {
			s.contentStorage[index].Output = cloneRawJSON(s.contentStorage[index].Output)
		}
	}
	cloned.Content = s.contentStorage[offset:end:end]
	s.data.Messages = append(s.data.Messages, cloned)
	s.contentLayout = append(s.contentLayout, messageContentLayout{offset: offset, length: len(message.Content)})
}

func (s *state) rebuildContentPointersLocked() {
	for index, layout := range s.contentLayout {
		if s.data.Messages[index].Content == nil {
			continue
		}
		end := layout.offset + layout.length
		s.data.Messages[index].Content = s.contentStorage[layout.offset:end:end]
	}
}

func (s *state) snapshot() StateSnapshot {
	s.mu.RLock()
	messages, _ := cloneStateMessages(
		s.data.Messages,
		s.contentStorage,
		s.contentLayout,
		s.contentHasRawJSON,
		s.messageHasMetadata,
	)
	snapshot := StateSnapshot{
		Messages:  messages,
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

func cloneMessages(messages []Message) []Message {
	cloned, _, _ := cloneMessagesWithStorage(messages)
	return cloned
}

func cloneMessagesWithStorage(messages []Message) ([]Message, []ContentBlock, []messageContentLayout) {
	if messages == nil {
		return nil, nil, nil
	}
	cloned := make([]Message, len(messages))
	storage := make([]ContentBlock, countContentBlocks(messages))
	layout := make([]messageContentLayout, len(messages))
	offset := 0
	for index, message := range messages {
		cloned[index] = message
		if message.Metadata != nil {
			cloned[index].Metadata = cloneMetadata(message.Metadata)
		}
		if message.Content == nil {
			continue
		}
		end := offset + len(message.Content)
		content := storage[offset:end:end]
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
		layout[index] = messageContentLayout{offset: offset, length: len(message.Content)}
		offset = end
	}
	return cloned, storage, layout
}

func cloneStateMessages(
	messages []Message,
	storage []ContentBlock,
	layout []messageContentLayout,
	copyRawJSON bool,
	copyMetadata bool,
) ([]Message, []ContentBlock) {
	if messages == nil {
		return nil, nil
	}
	cloned := make([]Message, len(messages))
	copy(cloned, messages)
	clonedStorage := make([]ContentBlock, len(storage))
	copy(clonedStorage, storage)
	if copyMetadata {
		for index, message := range messages {
			if message.Metadata != nil {
				cloned[index].Metadata = cloneMetadata(message.Metadata)
			}
		}
	}
	if copyRawJSON {
		for index := range clonedStorage {
			if clonedStorage[index].Input != nil {
				clonedStorage[index].Input = cloneRawJSON(clonedStorage[index].Input)
			}
			if clonedStorage[index].Output != nil {
				clonedStorage[index].Output = cloneRawJSON(clonedStorage[index].Output)
			}
		}
	}
	for index, message := range messages {
		if message.Content == nil {
			continue
		}
		contentLayout := layout[index]
		end := contentLayout.offset + contentLayout.length
		cloned[index].Content = clonedStorage[contentLayout.offset:end:end]
	}
	return cloned, clonedStorage
}

func messageFeatures(messages []Message) (bool, bool) {
	hasRawJSON := false
	hasMetadata := false
	for _, message := range messages {
		if message.Metadata != nil {
			hasMetadata = true
		}
		for _, block := range message.Content {
			if block.Input != nil || block.Output != nil {
				hasRawJSON = true
			}
		}
	}
	return hasRawJSON, hasMetadata
}

func countContentBlocks(messages []Message) int {
	count := 0
	for _, message := range messages {
		count += len(message.Content)
	}
	return count
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
