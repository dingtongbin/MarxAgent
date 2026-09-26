// SPDX-License-Identifier: Apache-2.0

// Package cache holds the prefix cache part: what stays byte identical between
// turns so a provider can reuse it, how that is detected rather than assumed, and
// what is reported when the hit rate says the context is malformed.
//
// The design makes the prefix layered, in this order: system, tool declarations,
// the capability index, the append only message history, and finally the dynamic
// tail. Everything above the tail is a candidate for reuse and everything in the
// tail is deliberately volatile. A manager that cannot tell the two apart will
// either cache content that changes every turn or refuse to cache anything that
// does not.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/core"
)

// Layer names one part of the prefix. The order is the order of stability, and
// CacheKey derives from it, so a layer's position is part of its meaning.
type Layer string

const (
	// LayerSystem is the system prompt, which changes only when its version does.
	LayerSystem Layer = "system"
	// LayerTools is the tool declarations, kept in a stable order so the bytes
	// do not move.
	LayerTools Layer = "tools"
	// LayerIndex is the capability index, which is append only: a new tool adds a
	// line and never edits one.
	LayerIndex Layer = "index"
	// LayerHistory is the append only message history.
	LayerHistory Layer = "history"
	// LayerDynamic is content injected for one turn only, which must never be
	// cached and never enters a signature.
	LayerDynamic Layer = "dynamic"
)

// Order is the order of stability, from most to least stable.
var Order = []Layer{LayerSystem, LayerTools, LayerIndex, LayerHistory, LayerDynamic}

// layerOrder reports where a layer sits, and whether it is one this package
// knows. An unknown layer sorts last so it can never be mistaken for a stable
// one.
func layerOrder(layer Layer) int {
	for index, known := range Order {
		if known == layer {
			return index
		}
	}
	return len(Order)
}

// Stable reports whether a layer is a candidate for reuse. The dynamic tail is
// not: caching content that changes every turn costs money and gains nothing.
func Stable(layer Layer) bool {
	order := layerOrder(layer)
	return order >= 0 && order < layerOrder(LayerDynamic)
}

// Snapshot is one recorded state of the cacheable prefix.
type Snapshot struct {
	// Sequence is the turn this snapshot belongs to.
	Sequence int64 `json:"sequence"`
	// Signature identifies the cacheable prefix. An empty signature means
	// nothing is cacheable yet.
	Signature string `json:"signature,omitempty"`
	// At is when the signature was computed.
	At time.Time `json:"at"`
	// LayerSizes records how many bytes each layer contributed, which is what
	// makes it obvious which layer is churning.
	LayerSizes map[Layer]int `json:"layer_sizes"`
	// MessageCount is how many messages the history held.
	MessageCount int `json:"message_count"`
	// DynamicBytes is how much volatile content was excluded.
	DynamicBytes int `json:"dynamic_bytes"`
	// Reason explains a signature change, so a caller can tell an append from an
	// edit without diffing hashes.
	Reason string `json:"reason,omitempty"`
}

// Observation is what a provider reported for one request.
type Observation struct {
	// Sequence is the turn the observation belongs to.
	Sequence int64
	// ReadTokens, WriteTokens and InputTokens come from the provider's usage.
	ReadTokens  int
	WriteTokens int
	InputTokens int
	// At is when the request finished.
	At time.Time
}

// HitRate reports the share of input tokens served from the cache.
func (o Observation) HitRate() float64 {
	if o.InputTokens <= 0 {
		return 0
	}
	return float64(o.ReadTokens) / float64(o.InputTokens)
}

// ChangeKind classifies how a signature changed between two snapshots.
type ChangeKind string

const (
	// ChangeNone means the prefix is byte identical, which is the case worth
	// protecting.
	ChangeNone ChangeKind = "unchanged"
	// ChangeAppend means the prefix only grew, so the previous prefix is still a
	// valid cache entry and the provider can reuse it.
	ChangeAppend ChangeKind = "appended"
	// ChangeEdit means something in the middle changed, so the cached prefix no
	// longer matches and the cache has to be rebuilt from a new breakpoint.
	ChangeEdit ChangeKind = "edited"
	// ChangeReset means the prefix was deliberately rebuilt, as after compaction.
	ChangeReset ChangeKind = "reset"
)

// Change says what happened to the prefix.
type Change struct {
	Kind   ChangeKind
	Reason string
	// From and To are the signatures involved, empty when there was none before.
	From string
	To   string
}

// Config configures the manager.
type Config struct {
	// LowHitRateThreshold is the hit rate below which an alarm is raised. Zero
	// selects the default.
	LowHitRateThreshold float64
	// ObservationWindow is how many recent observations the rate is computed over.
	// Zero selects the default.
	ObservationWindow int
	// MaxHistory bounds the retained observations, so a long session cannot grow
	// the monitor without limit.
	MaxHistory int
}

const (
	// DefaultLowHitRateThreshold is the rate below which the design calls a
	// warning unprompted.
	DefaultLowHitRateThreshold = 0.5
	// DefaultObservationWindow is how many turns the rate is computed over.
	DefaultObservationWindow = 10
	// defaultMaxHistory bounds retained observations.
	defaultMaxHistory = 256
)

// Alarm is raised when the hit rate says something is wrong.
type Alarm struct {
	// At is when the alarm was raised.
	At time.Time
	// HitRate is the rate that triggered it.
	HitRate float64
	// Threshold is the rate it was compared against.
	Threshold float64
	// Window is how many observations the rate was computed over.
	Window int
	// Sequence is the turn the alarm belongs to.
	Sequence int64
	// Message explains what the rate means, so a log line is actionable.
	Message string
	// Change is how the prefix moved around the alarm, which is the usual cause.
	Change Change
}

// Manager owns the cacheable prefix.
type Manager struct {
	config Config

	mu           sync.RWMutex
	current      Snapshot
	history      []Observation
	changes      []Change
	alarms       []Alarm
	sequence     int64
	maxAlarm     int
	invalidated  bool
	invalidCount int
	// alarmActive records that the low rate alarm is already outstanding, so it
	// is raised on the collapse rather than on every turn of it.
	alarmActive bool
}

// New builds a manager.
func New(config Config) *Manager {
	if config.LowHitRateThreshold <= 0 {
		config.LowHitRateThreshold = DefaultLowHitRateThreshold
	}
	if config.ObservationWindow <= 0 {
		config.ObservationWindow = DefaultObservationWindow
	}
	if config.MaxHistory <= 0 {
		config.MaxHistory = defaultMaxHistory
	}
	return &Manager{config: config, maxAlarm: 32}
}

// Config reports the configuration in force.
func (m *Manager) Config() Config { return m.config }

// Signature computes the signature of the cacheable prefix and records it.
//
// The system prompt, the tool declarations and the capability index are hashed,
// because those are the layers that sit above the message history and therefore
// inside the bytes a provider caches. Leaving the index out would mean a changed
// index produced the same signature, and a provider would be asked to reuse an
// entry that no longer matches what is being sent.
//
// The history is deliberately not hashed: it only grows, and its length is not
// what identifies it. Growth is detected by the sequence instead, which is what
// keeps appending cheap.
func (m *Manager) Signature(system string, tools []core.ToolSpec, index string) string {
	// The tools are hashed in sorted order, so a map iteration or a registry
	// returning them in a different order cannot silently change the signature.
	sorted := make([]core.ToolSpec, len(tools))
	copy(sorted, tools)
	sort.SliceStable(sorted, func(first, second int) bool {
		return sorted[first].Name < sorted[second].Name
	})
	digest := sha256.New()
	writeField(digest, "system", system)
	for _, tool := range sorted {
		writeField(digest, "name", tool.Name)
		writeField(digest, "description", tool.Description)
		writeField(digest, "parameters", string(tool.Parameters))
	}
	writeField(digest, "index", index)
	// The system prompt is versioned, so an edit to it is a breaking change by
	// construction. Folding the count in means a tool set of a different size
	// cannot collide with one of the same tools in a different order.
	writeField(digest, "count", strconv.Itoa(len(sorted)))
	return hex.EncodeToString(digest.Sum(nil))
}

// Record stores a snapshot of the current prefix and reports how it changed.
func (m *Manager) Record(system string, tools []core.ToolSpec, index, dynamic string, messages []core.Message) (Snapshot, Change) {
	signature := m.Signature(system, tools, index)
	sizes := map[Layer]int{
		LayerSystem:  len(system),
		LayerTools:   estimateTools(tools),
		LayerIndex:   len(index),
		LayerHistory: estimateMessages(messages),
		LayerDynamic: len(dynamic),
	}
	snapshot := Snapshot{
		Sequence:     m.nextSequence(),
		Signature:    signature,
		At:           time.Now().UTC(),
		LayerSizes:   sizes,
		MessageCount: len(messages),
		DynamicBytes: len(dynamic),
	}

	m.mu.Lock()
	previous := m.current
	change := Change{From: previous.Signature, To: signature}
	// The order matters. The signature covers the layers above the history, so an
	// unchanged signature is the normal case for a turn that only grew its
	// history, and checking it first would label every append as "unchanged" and
	// hide the one case the whole design exists to protect.
	switch {
	case previous.Signature == "":
		change.Kind = ChangeNone
		change.Reason = "the first snapshot has nothing to compare against"
	case previous.Signature != signature:
		change.Kind = ChangeEdit
		change.Reason = "the system prompt, the tool declarations or the capability index changed"
	case snapshot.MessageCount > previous.MessageCount:
		change.Kind = ChangeAppend
		change.Reason = fmt.Sprintf("the history grew from %d to %d messages",
			previous.MessageCount, snapshot.MessageCount)
	case snapshot.MessageCount < previous.MessageCount:
		// A shrinking history is a compaction. The layers above it are byte
		// identical, so the signature cannot show it, but the cached prefix no
		// longer matches what would be sent, so it is reported rather than called
		// identical.
		change.Kind = ChangeEdit
		change.Reason = fmt.Sprintf("the history shrank from %d to %d messages, which rewrote the cached prefix",
			previous.MessageCount, snapshot.MessageCount)
	default:
		change.Kind = ChangeNone
		change.Reason = "the cacheable prefix is byte identical"
	}
	snapshot.Reason = change.Reason
	m.current = snapshot
	m.changes = append(m.changes, change)
	if len(m.changes) > m.config.MaxHistory {
		m.changes = m.changes[len(m.changes)-m.config.MaxHistory:]
	}
	m.mu.Unlock()

	return snapshot, change
}

// nextSequence hands out turn numbers.
func (m *Manager) nextSequence() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sequence++
	return m.sequence
}

// Current returns the last recorded snapshot.
func (m *Manager) Current() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

// Changes returns the retained changes, oldest first.
func (m *Manager) Changes() []Change {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Change, len(m.changes))
	copy(out, m.changes)
	return out
}

// CacheKey is the value written into a request, or an empty string when nothing
// is cacheable.
//
// It is a digest of the signature and the message count, because the count is
// what distinguishes two turns that share a prefix: a provider that caches by
// prefix bytes alone would report a hit for a prefix it cannot actually reuse.
func (m *Manager) CacheKey() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.current.Signature == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(
		m.current.Signature + ":" + strconv.FormatInt(int64(m.current.MessageCount), 10)))
	return hex.EncodeToString(digest[:])
}

// Invalidate discards the current prefix. It exists for the caller that changed
// something this package cannot see, and it is deliberately blunt: pretending a
// signature is still good when it is not would cost a full price round trip.
func (m *Manager) Invalidate(reason string) Change {
	m.mu.Lock()
	defer m.mu.Unlock()
	change := Change{
		Kind:   ChangeEdit,
		Reason: reason,
		From:   m.current.Signature,
	}
	m.current.Signature = ""
	m.invalidated = true
	m.invalidCount++
	m.changes = append(m.changes, change)
	if len(m.changes) > m.config.MaxHistory {
		m.changes = m.changes[len(m.changes)-m.config.MaxHistory:]
	}
	return change
}

// Reset records a deliberate rebuild of the prefix, which is what compaction
// does. A reset is not an edit: the caller knows why it happened, and the
// difference is what a diagnostic needs.
func (m *Manager) Reset(reason string) Change {
	m.mu.Lock()
	defer m.mu.Unlock()
	change := Change{
		Kind:   ChangeReset,
		Reason: reason,
		From:   m.current.Signature,
		To:     m.current.Signature,
	}
	m.invalidated = true
	m.invalidCount++
	m.changes = append(m.changes, change)
	if len(m.changes) > m.config.MaxHistory {
		m.changes = m.changes[len(m.changes)-m.config.MaxHistory:]
	}
	return change
}

// Invalidated reports whether the prefix has been discarded, and how many times.
func (m *Manager) Invalidated() (bool, int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.invalidated, m.invalidCount
}

// Observe records what a provider reported and raises an alarm when the hit rate
// says the context is malformed.
//
// A low hit rate is not a performance footnote. It is the signal that the prompt
// is being rewritten, the tools are arriving in a different order, or something
// is injecting into the wrong place, and each of those is invisible from the
// request alone.
func (m *Manager) Observe(observation Observation) (Alarm, bool) {
	if observation.At.IsZero() {
		observation.At = time.Now().UTC()
	}
	if observation.Sequence == 0 {
		observation.Sequence = m.current.Sequence
	}
	m.mu.Lock()
	m.history = append(m.history, observation)
	if len(m.history) > m.config.MaxHistory {
		m.history = m.history[len(m.history)-m.config.MaxHistory:]
	}
	rate, window, measured := m.rateLocked()
	change := m.lastChangeLocked()
	recovered := m.alarmActive && rate >= m.config.LowHitRateThreshold
	m.mu.Unlock()

	if window < m.config.ObservationWindow {
		// A rate over too few turns says nothing, and alarming on it would train
		// a reader to ignore the alarm.
		return Alarm{}, false
	}
	if !measured {
		// A window full of reports that carried no token counts is a provider that
		// does not report cache usage, not a cache that stopped working. Raising an
		// alarm here would be crying wolf on every such provider.
		return Alarm{}, false
	}
	if rate >= m.config.LowHitRateThreshold {
		if recovered {
			// The next collapse is a new event, so the alarm is armed again.
			m.mu.Lock()
			m.alarmActive = false
			m.mu.Unlock()
		}
		return Alarm{}, false
	}
	if m.alarmActive {
		// The alarm is already raised and nothing has recovered. Repeating it every
		// turn would bury the one line that matters under the ones that do not.
		return Alarm{}, false
	}
	alarm := Alarm{
		At:        observation.At,
		HitRate:   rate,
		Threshold: m.config.LowHitRateThreshold,
		Window:    window,
		Sequence:  observation.Sequence,
		Change:    change,
		Message: fmt.Sprintf(
			"the prefix cache hit rate fell to %.0f%% over %d turns, "+
				"which usually means the system prompt or the tool declarations are changing between turns",
			rate*100, window),
	}
	m.mu.Lock()
	m.alarms = append(m.alarms, alarm)
	m.alarmActive = true
	if len(m.alarms) > m.maxAlarm {
		m.alarms = m.alarms[len(m.alarms)-m.maxAlarm:]
	}
	m.mu.Unlock()
	return alarm, true
}

// rateLocked computes the hit rate over the window, and reports whether the
// window held any token counts at all.
func (m *Manager) rateLocked() (rate float64, window int, measured bool) {
	window = m.config.ObservationWindow
	if len(m.history) < window {
		window = len(m.history)
	}
	if window == 0 {
		return 0, 0, false
	}
	recent := m.history[len(m.history)-window:]
	read, input := 0, 0
	for _, observation := range recent {
		read += observation.ReadTokens
		input += observation.InputTokens
	}
	if input <= 0 {
		return 0, window, false
	}
	return float64(read) / float64(input), window, true
}

func (m *Manager) lastChangeLocked() Change {
	if len(m.changes) == 0 {
		return Change{}
	}
	return m.changes[len(m.changes)-1]
}

// HitRate reports the current rate and how many turns it covers.
func (m *Manager) HitRate() (float64, int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rate, window, _ := m.rateLocked()
	return rate, window
}

// Alarms returns the retained alarms, oldest first.
func (m *Manager) Alarms() []Alarm {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Alarm, len(m.alarms))
	copy(out, m.alarms)
	return out
}

// Stats summarizes the observations.
type Stats struct {
	// Turns is how many observations were recorded.
	Turns int
	// ReadTokens, WriteTokens and InputTokens are the totals.
	ReadTokens  int
	WriteTokens int
	InputTokens int
	// HitRate is over the observation window, not over every turn, so a long
	// healthy run is not diluted by a bad start.
	HitRate float64
	// Window is how many turns the rate covers.
	Window int
	// Appends, Edits and Resets count the prefix changes by kind.
	Appends int
	Edits   int
	Resets  int
	// Alarms is how many alarms were raised.
	Alarms int
}

// Stats summarizes the manager.
func (m *Manager) Stats() Stats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	stats := Stats{Turns: len(m.history), Alarms: len(m.alarms)}
	for _, observation := range m.history {
		stats.ReadTokens += observation.ReadTokens
		stats.WriteTokens += observation.WriteTokens
		stats.InputTokens += observation.InputTokens
	}
	stats.HitRate, stats.Window, _ = m.rateLocked()
	for _, change := range m.changes {
		switch change.Kind {
		case ChangeAppend:
			stats.Appends++
		case ChangeEdit:
			stats.Edits++
		case ChangeReset:
			stats.Resets++
		}
	}
	return stats
}

// StablePrefix is what a caller hands to a provider mapper: the system prompt and
// the tool declarations, in a stable order, with the dynamic tail excluded.
//
// The layers are returned separately rather than concatenated because a mapper
// needs to place a breakpoint between them, and a single blob would have lost
// that boundary.
type StablePrefix struct {
	System  string
	Tools   []core.ToolSpec
	Index   string
	History []core.Message
	// Dynamic is the volatile content, returned so a caller can be shown that it
	// was excluded rather than having to trust that it was.
	Dynamic string
	// Signature is the signature of the layers above the history.
	Signature string
	// Key is the cache key to write into a request, empty when nothing is
	// cacheable.
	Key string
}

// Build assembles the stable prefix and its cache key.
func (m *Manager) Build(system string, tools []core.ToolSpec, index string, history []core.Message, dynamic string) StablePrefix {
	_, _ = m.Record(system, tools, index, dynamic, history)
	// The tools are ordered the same way they were hashed, so the bytes a mapper
	// writes match the bytes the signature was computed from.
	sorted := make([]core.ToolSpec, len(tools))
	copy(sorted, tools)
	sort.SliceStable(sorted, func(first, second int) bool {
		return sorted[first].Name < sorted[second].Name
	})
	current := m.Current()
	return StablePrefix{
		System:    system,
		Tools:     sorted,
		Index:     index,
		History:   cloneMessages(history),
		Dynamic:   dynamic,
		Signature: current.Signature,
		Key:       m.CacheKey(),
	}
}

// writeField writes a length prefixed field into a hash, so two different field
// sets cannot produce the same digest.
func writeField(digest interface{ Write([]byte) (int, error) }, name, value string) {
	fmt.Fprintf(digest, "%d:%s=%d:%s\n", len(name), name, len(value), value)
}

func estimateTools(tools []core.ToolSpec) int {
	total := 0
	for _, tool := range tools {
		total += len(tool.Name) + len(tool.Description) + len(tool.Parameters)
	}
	return total
}

func estimateMessages(messages []core.Message) int {
	total := 0
	for _, message := range messages {
		for _, block := range message.Content {
			total += len(block.Text) + len(block.Output) + len(block.Input) + len(block.Thinking)
		}
	}
	return total
}

func cloneMessages(messages []core.Message) []core.Message {
	if messages == nil {
		return nil
	}
	out := make([]core.Message, 0, len(messages))
	for _, message := range messages {
		copied := message
		if message.Content != nil {
			copied.Content = make([]core.ContentBlock, len(message.Content))
			copy(copied.Content, message.Content)
		}
		out = append(out, copied)
	}
	return out
}

// Describe renders a snapshot for a log line, so a cache problem can be read
// without a debugger.
func (s Snapshot) Describe() string {
	layers := make([]string, 0, len(Order))
	for _, layer := range Order {
		layers = append(layers, fmt.Sprintf("%s=%d", layer, s.LayerSizes[layer]))
	}
	return fmt.Sprintf("turn %d signature %s messages %d %s",
		s.Sequence, shortSignature(s.Signature), s.MessageCount, strings.Join(layers, " "))
}

func shortSignature(signature string) string {
	if signature == "" {
		return "none"
	}
	if len(signature) <= 12 {
		return signature
	}
	return signature[:12]
}

// MarshalSnapshot renders a snapshot as JSON, for the log or a bug report.
func (s Snapshot) MarshalSnapshot() (string, error) {
	encoded, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("cache: encode the snapshot: %w", err)
	}
	return string(encoded), nil
}
