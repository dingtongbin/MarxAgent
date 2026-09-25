// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"errors"
	"strings"
	"testing"
)

func collectStream(body string) ([]StreamEvent, error) {
	events := make([]StreamEvent, 0, 4)
	for event := range DecodeStream(strings.NewReader(body)) {
		events = append(events, event)
	}
	return events, nil
}

func TestDecodeStream(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []StreamEvent
	}{
		{
			name: "empty body",
			body: "",
			want: nil,
		},
		{
			name: "unnamed event",
			body: "data: hello\n\n",
			want: []StreamEvent{{Data: "hello"}},
		},
		{
			name: "named event with id",
			body: "event: ping\nid: 42\ndata: {}\n\n",
			want: []StreamEvent{{Name: "ping", ID: "42", Data: "{}"}},
		},
		{
			name: "multi line data is joined",
			body: "data: one\ndata: two\n\n",
			want: []StreamEvent{{Data: "one\ntwo"}},
		},
		{
			name: "comments and unknown fields are ignored",
			body: ": keep-alive\nretry: 100\ndata: payload\n\n",
			want: []StreamEvent{{Data: "payload"}},
		},
		{
			name: "windows line endings",
			body: "event: done\r\ndata: {}\r\n\r\n",
			want: []StreamEvent{{Name: "done", Data: "{}"}},
		},
		{
			name: "value without a leading space",
			body: "data:tight\n\n",
			want: []StreamEvent{{Data: "tight"}},
		},
		{
			name: "empty value",
			body: "data:\n\n",
			want: []StreamEvent{{Data: ""}},
		},
		{
			name: "event without a data field is skipped",
			body: "event: only\n\ndata: real\n\n",
			want: []StreamEvent{{Data: "real"}},
		},
		{
			name: "trailing event without a blank line",
			body: "data: last",
			want: []StreamEvent{{Data: "last"}},
		},
		{
			name: "a blank line always ends an event",
			body: "data: {\n\ndata: }\n\n",
			want: []StreamEvent{{Data: "{"}, {Data: "}"}},
		},
		{
			name: "several events",
			body: "data: one\n\ndata: two\n\ndata: three\n\n",
			want: []StreamEvent{{Data: "one"}, {Data: "two"}, {Data: "three"}},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			events, err := collectStream(test.body)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != len(test.want) {
				t.Fatalf("events = %#v, want %#v", events, test.want)
			}
			for index := range events {
				if events[index] != test.want[index] {
					t.Fatalf("event %d = %#v, want %#v", index, events[index], test.want[index])
				}
			}
		})
	}
}

func TestDecodeStreamStopsOnDemand(t *testing.T) {
	seen := 0
	for range DecodeStream(strings.NewReader("data: one\n\ndata: two\n\ndata: three\n\n")) {
		seen++
		if seen == 2 {
			break
		}
	}
	if seen != 2 {
		t.Fatalf("consumed %d events after breaking", seen)
	}
}

func TestDecodeStreamRejectsOversizedLine(t *testing.T) {
	body := "data: " + strings.Repeat("x", MaxStreamLineBytes+16) + "\n\n"
	err := decodeStream(strings.NewReader(body), func(StreamEvent) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized line error = %v", err)
	}
}

func TestDecodeStreamRejectsNilBody(t *testing.T) {
	if err := decodeStream(nil, func(StreamEvent) error { return nil }); err == nil {
		t.Fatal("nil body accepted")
	}
}

func TestDecodeStreamPropagatesHandlerError(t *testing.T) {
	sentinel := errors.New("stop now")
	err := decodeStream(strings.NewReader("data: one\n\n"), func(StreamEvent) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("handler error = %v", err)
	}
}

func TestStreamWriterRoundTrip(t *testing.T) {
	writer := NewStreamWriter(&strings.Builder{})
	if err := writer.WriteEvent("chunk", map[string]string{"text": "hello"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteComment("keep-alive"); err != nil {
		t.Fatal(err)
	}
	multi := NewStreamWriter(&strings.Builder{})
	if err := multi.WriteEvent("", "line one\nline two"); err != nil {
		t.Fatal(err)
	}
	var builder strings.Builder
	if err := NewStreamWriter(&builder).WriteEvent("done", map[string]int{"index": 1}); err != nil {
		t.Fatal(err)
	}
	events, err := collectStream(builder.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Name != "done" || events[0].Data != `{"index":1}` {
		t.Fatalf("events = %#v", events)
	}
	if writer.writer == nil {
		t.Fatal("writer lost its target")
	}
	if multi.writer == nil {
		t.Fatal("multi-line writer lost its target")
	}
}

func TestStreamWriterRejectsUninitializedWriter(t *testing.T) {
	var writer *StreamWriter
	if err := writer.WriteEvent("x", nil); err == nil {
		t.Fatal("nil writer accepted an event")
	}
	if err := writer.WriteComment("x"); err == nil {
		t.Fatal("nil writer accepted a comment")
	}
	if err := NewStreamWriter(nil).WriteEvent("x", nil); err == nil {
		t.Fatal("writer without a target accepted an event")
	}
}

func TestReadErrorBody(t *testing.T) {
	if got := ReadErrorBody(strings.NewReader("  boom  ")); got != "boom" {
		t.Fatalf("error body = %q", got)
	}
	if got := ReadErrorBody(nil); got != "" {
		t.Fatalf("nil error body = %q", got)
	}
	long := strings.Repeat("e", MaxErrorBodyBytes*2)
	if got := ReadErrorBody(strings.NewReader(long)); len(got) != MaxErrorBodyBytes {
		t.Fatalf("truncated body length = %d", len(got))
	}
}
