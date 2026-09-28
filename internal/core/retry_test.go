// SPDX-License-Identifier: Apache-2.0

package core

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fastBackoff waits the retry path out in milliseconds, so a test exercises
// the same pauses the production turn waits through without spending the
// seconds the growing wait costs.
func fastBackoff(t *testing.T) {
	t.Helper()
	previous := streamBackoff
	streamBackoff = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { streamBackoff = previous })
}

// A model call that failed on something that may come back is a pause the
// turn waits through, not a failure that ends it: the waiting event is
// announced, the retry is made, and the turn answers as it would have.
func TestARecoverableCallFailureIsWaitedThroughAndRetried(t *testing.T) {
	fastBackoff(t)
	provider := &recordingProvider{
		errors: []error{errors.New("connection reset by peer")},
		streams: [][]StreamChunk{
			{},
			{{Type: StreamTypeText, Text: "answered anyway"}, {Type: StreamTypeDone}},
		},
	}
	agent := newTestAgent(t, provider, Config{Model: "model"})
	events, err := agent.Run(context.Background(), textInput("hello"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	collected := collectRunEvents(t, events)

	var waiting, done, errored int
	var waitingMessage string
	for _, event := range collected {
		switch event.Type {
		case EventTypeWaiting:
			waiting++
			waitingMessage = event.Data
		case EventTypeDone:
			done++
		case EventTypeError:
			errored++
		}
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d, want the failure and the retry", provider.calls)
	}
	if waiting != 1 {
		t.Fatalf("waiting events = %d, want one", waiting)
	}
	if !strings.Contains(waitingMessage, "重试") {
		t.Fatalf("waiting event does not say what it is: %s", waitingMessage)
	}
	if done != 1 || errored != 0 {
		t.Fatalf("turn ended as %d done and %d errors, want the retry's answer", done, errored)
	}
}

// A failure the endpoint meant as an answer is not retried: the turn says so
// and stops, without waiting out the same refusal three times.
func TestAnUnrecoverableCallFailureIsReportedAtOnce(t *testing.T) {
	provider := &recordingProvider{
		errors: []error{errors.New("the model does not support this parameter")},
	}
	agent := newTestAgent(t, provider, Config{Model: "model"})
	events, err := agent.Run(context.Background(), textInput("hello"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	collected := collectRunEvents(t, events)

	waiting, errored := 0, 0
	for _, event := range collected {
		switch event.Type {
		case EventTypeWaiting:
			waiting++
		case EventTypeError:
			errored++
		}
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want one with no retry", provider.calls)
	}
	if waiting != 0 || errored != 1 {
		t.Fatalf("waiting = %d errors = %d, want the failure reported at once", waiting, errored)
	}
}

// The retries are finite: a failing endpoint is reported as a failure after
// the last one, and each retry before that was announced while it waited.
func TestRetriesRunOutAndReportTheFailure(t *testing.T) {
	fastBackoff(t)
	provider := &recordingProvider{
		errors: []error{
			errors.New("502 bad gateway"),
			errors.New("502 bad gateway"),
			errors.New("502 bad gateway"),
			errors.New("502 bad gateway"),
		},
	}
	agent := newTestAgent(t, provider, Config{Model: "model"})
	events, err := agent.Run(context.Background(), textInput("hello"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	collected := collectRunEvents(t, events)

	waiting, errored := 0, 0
	for _, event := range collected {
		switch event.Type {
		case EventTypeWaiting:
			waiting++
		case EventTypeError:
			errored++
		}
	}
	if provider.calls != maxStreamRetries+1 {
		t.Fatalf("provider calls = %d, want the first try plus %d retries",
			provider.calls, maxStreamRetries)
	}
	if waiting != maxStreamRetries || errored != 1 {
		t.Fatalf("waiting = %d errors = %d, want one wait per retry and one failure",
			waiting, errored)
	}
}

// A stream that died half way is retried under the same message: the waiting
// event names the message so a window can clear the half it drew, and a retry
// that answers completes the turn without the half answer on the record.
func TestAStreamThatDiesHalfWayIsRetried(t *testing.T) {
	fastBackoff(t)
	provider := &recordingProvider{
		streams: [][]StreamChunk{
			{
				{Type: StreamTypeText, Text: "half an"},
				{Err: errors.New("read tcp: connection reset")},
			},
			{{Type: StreamTypeText, Text: "the whole answer"}, {Type: StreamTypeDone}},
		},
	}
	agent := newTestAgent(t, provider, Config{Model: "model"})
	events, err := agent.Run(context.Background(), textInput("hello"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	collected := collectRunEvents(t, events)

	var waiting, done int
	for _, event := range collected {
		switch event.Type {
		case EventTypeWaiting:
			waiting++
		case EventTypeDone:
			done++
		}
	}
	if waiting != 1 || done != 1 {
		t.Fatalf("waiting = %d done = %d, want the pause and then the answer", waiting, done)
	}
	// The record holds one answer: the half-drawn message was never appended,
	// because a retry starts it over.
	if len(agent.State().Messages()) != 2 {
		t.Fatalf("messages = %d, want the question and the whole answer",
			len(agent.State().Messages()))
	}
}
