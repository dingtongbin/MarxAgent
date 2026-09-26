// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// RecordTypeRecoveryAction is the audit trail a recovery run leaves behind, so
// the repair itself is inspectable instead of silent.
const RecordTypeRecoveryAction = "recovery_action"

// RecoveryStream is the stream a recovery run writes its actions to.
const RecoveryStream = "recovery:startup"

// SessionRecovery reports what happened to one stream during recovery.
type SessionRecovery struct {
	Stream string `json:"stream"`
	// Journal records found after repair.
	Records int `json:"records"`
	// Bytes of a half written trailing line that were removed.
	TruncatedBytes int `json:"truncated_bytes"`
	// Records that could not be parsed and were skipped.
	Skipped int `json:"skipped"`
	// Records replayed into the ledger, which is the normal case of a journal
	// that ran ahead of the checkpoint.
	Replayed int `json:"replayed"`
	// Records already present in the ledger, so recovery is idempotent.
	SkippedReplay int `json:"skipped_replay"`
	// Broker messages left unclaimed, which is how a dead sub agent's work is
	// filed for audit instead of being redelivered.
	Orphaned int `json:"orphaned"`
	// Messages that carry the incomplete flag from an interrupted stream.
	Incomplete int `json:"incomplete"`
	// True when the ledger was ahead of the journal, which means the journal
	// was lost rather than merely interrupted.
	LedgerAhead bool     `json:"ledger_ahead"`
	Warnings    []string `json:"warnings,omitempty"`
}

// RecoveryResult is the whole recovery run.
type RecoveryResult struct {
	Sessions  []SessionRecovery `json:"sessions"`
	StartedAt time.Time         `json:"started_at"`
	// Actions is the number of audit records written for the run itself.
	Actions int `json:"actions"`
}

// Recover repairs and replays a scope after an abrupt shutdown.
//
// The order matters: the trailing half line goes first, because every record
// after it would be unparseable. Replay is driven by the checkpoint, so running
// recovery twice converges on the same state. Broker messages are never
// delivered: a restarted process has no sub agents to receive them, and
// resurrecting them would restart work nobody asked for.
func Recover(ctx context.Context, root string, audit *AuditSink) (RecoveryResult, error) {
	result := RecoveryResult{StartedAt: time.Now().UTC()}
	if strings.TrimSpace(root) == "" {
		return result, fmt.Errorf("storage: recovery root must not be empty")
	}
	journals, err := discoverJournals(root)
	if err != nil {
		return result, err
	}
	var actions []Record
	for _, journal := range journals {
		session, sessionActions, err := recoverStream(ctx, journal.path, journal.stream, audit)
		if err != nil {
			return result, err
		}
		result.Sessions = append(result.Sessions, session)
		actions = append(actions, sessionActions...)
	}
	sort.Slice(result.Sessions, func(first, second int) bool {
		return result.Sessions[first].Stream < result.Sessions[second].Stream
	})
	if len(actions) > 0 {
		if audit != nil {
			if err := audit.Write(ctx, actions); err != nil {
				return result, err
			}
		}
		result.Actions = len(actions)
	}
	return result, nil
}

func recoverStream(ctx context.Context, path, stream string, audit *AuditSink) (SessionRecovery, []Record, error) {
	outcome := SessionRecovery{Stream: stream}
	records, partial, err := ReadJournal(path)
	if err != nil {
		return outcome, nil, err
	}
	if len(partial) > 0 {
		if err := TruncatePartialJournal(path, partial); err != nil {
			return outcome, nil, err
		}
		outcome.TruncatedBytes = len(partial)
	}

	var checkpoint int64
	if audit != nil {
		// The checkpoint is per stream: a session's message stream and event
		// stream advance independently, so sharing one sequence between them
		// would make one of them skip records.
		checkpoint, err = audit.LastAppliedSeq(ctx, stream)
		if err != nil {
			return outcome, nil, err
		}
	}
	highest, err := highestSequence(records)
	if err != nil {
		return outcome, nil, err
	}
	if audit != nil && checkpoint > highest {
		// The journal is behind the checkpoint, so the journal lost writes the
		// ledger already has. The ledger is the audited copy and wins.
		outcome.LedgerAhead = true
		outcome.Warnings = append(outcome.Warnings,
			"the ledger is ahead of the journal; the journal was truncated or lost")
	}

	var replay []Record
	for _, record := range records {
		outcome.Records++
		if record.Type == "" || record.Stream == "" {
			outcome.Skipped++
			outcome.Warnings = append(outcome.Warnings, "a record had no type or stream")
			continue
		}
		if record.Seq > checkpoint {
			replay = append(replay, record)
		} else {
			outcome.SkippedReplay++
		}
		switch record.Type {
		case RecordBrokerMessage:
			outcome.Orphaned++
		case RecordPartialStream:
			outcome.Incomplete++
		}
	}

	var actions []Record
	if outcome.TruncatedBytes > 0 {
		actions = append(actions, recoveryAction("truncated_partial", map[string]any{
			"stream": stream, "bytes": outcome.TruncatedBytes,
		}))
	}
	if outcome.Skipped > 0 {
		actions = append(actions, recoveryAction("skipped_records", map[string]any{
			"stream": stream, "count": outcome.Skipped,
		}))
	}
	if outcome.LedgerAhead {
		actions = append(actions, recoveryAction("ledger_ahead", map[string]any{
			"stream": stream, "checkpoint": checkpoint, "journal": highest,
		}))
	}
	if outcome.Orphaned > 0 {
		actions = append(actions, recoveryAction("orphan_broker_messages", map[string]any{
			"stream": stream, "count": outcome.Orphaned,
		}))
	}
	if audit != nil && len(replay) > 0 {
		if err := audit.Write(ctx, replay); err != nil {
			return outcome, nil, err
		}
		outcome.Replayed = len(replay)
	}
	return outcome, actions, nil
}

// IncompleteMessage turns a partial stream record into a message payload that
// keeps the interrupted marker, so the model sees a truncated message as
// interrupted rather than as a complete one it may have to trust.
func IncompleteMessage(record Record) (json.RawMessage, bool) {
	if record.Type != RecordPartialStream {
		return nil, false
	}
	var payload struct {
		Stream   string          `json:"stream"`
		Text     string          `json:"text"`
		Thinking string          `json:"thinking"`
		Message  json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(record.Data, &payload); err != nil {
		// A partial record is by nature unparseable, so the text is recovered
		// leniently. Handing the model the raw JSON scaffolding as if it were
		// content would be worse than a short fragment.
		payload.Text = lenientText(record.Data)
	}
	content := payload.Text
	if content == "" && payload.Thinking != "" {
		content = payload.Thinking
	}
	message := map[string]any{
		"id":         fmt.Sprintf("partial-%d", record.Seq),
		"role":       "assistant",
		"content":    content,
		"created_at": record.Ts,
		"incomplete": true,
	}
	if len(payload.Message) > 0 {
		var original map[string]any
		if err := json.Unmarshal(payload.Message, &original); err == nil {
			for key, value := range original {
				message[key] = value
			}
			message["incomplete"] = true
		}
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		return nil, false
	}
	return encoded, true
}

// lenientText pulls the value of a text field out of a truncated JSON object.
func lenientText(data []byte) string {
	marker := []byte(`"text":"`)
	index := strings.Index(string(data), string(marker))
	if index < 0 {
		return ""
	}
	rest := strings.TrimLeft(string(data[index+len(marker):]), " ")
	rest = strings.TrimSuffix(rest, `","}`)
	rest = strings.TrimSuffix(rest, `"}`)
	rest = strings.TrimSuffix(rest, `",`)
	rest = strings.TrimSuffix(rest, `"`)
	return rest
}

func highestSequence(records []Record) (int64, error) {
	var highest int64
	for _, record := range records {
		if record.Seq < 0 {
			return 0, fmt.Errorf("storage: record sequence %d is negative", record.Seq)
		}
		if record.Seq > highest {
			highest = record.Seq
		}
	}
	return highest, nil
}

func recoveryAction(action string, detail map[string]any) Record {
	detail["action"] = action
	encoded, err := json.Marshal(detail)
	if err != nil {
		encoded = json.RawMessage(`{"action":"` + action + `"}`)
	}
	return Record{
		Stream: RecoveryStream,
		Type:   RecordTypeRecoveryAction,
		Data:   encoded,
		Ts:     time.Now().UTC(),
	}
}

// streamID reports the identifier part of a stream, which is what the message
// and event streams of one session share.
func streamID(stream string) string {
	if _, id, found := strings.Cut(stream, ":"); found && id != "" {
		return id
	}
	return stream
}
