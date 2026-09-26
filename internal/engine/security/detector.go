// SPDX-License-Identifier: Apache-2.0

// Package security holds the prompt injection defences: a detector for the input
// side, an envelope for the tool result side, and a policy primitive the
// assembly layer turns into a mode's actual gates.
//
// The three defences are independent on purpose. A detector that misses a phrasing
// does not help if tool output is trusted, and an envelope does not help if a
// dangerous tool is simply available, so none of the three is treated as the one
// that matters.
package security

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// Severity is how much a finding is trusted.
type Severity string

const (
	// SeverityLow is a pattern that also appears in ordinary text. It is worth
	// recording and not worth blocking on.
	SeverityLow Severity = "low"
	// SeverityMedium is a pattern that is unusual in ordinary text and is a strong
	// hint, but is not decisive alone.
	SeverityMedium Severity = "medium"
	// SeverityHigh is a pattern that has no innocent reading in an agent prompt.
	SeverityHigh Severity = "high"
)

// rank orders severities so the strongest finding wins.
func (s Severity) rank() int {
	switch s {
	case SeverityHigh:
		return 3
	case SeverityMedium:
		return 2
	case SeverityLow:
		return 1
	default:
		return 0
	}
}

// Rule is one detection pattern.
type Rule struct {
	// Name identifies the rule, so a finding is actionable rather than anonymous.
	Name string
	// Pattern matches the text under inspection.
	Pattern *regexp.Regexp
	// Severity is what a match is worth.
	Severity Severity
	// Explanation says what the pattern is doing, for the log line that accompanies
	// a block. A block with no explanation is one a user cannot argue with.
	Explanation string
}

// Finding is one match.
type Finding struct {
	Rule     string   `json:"rule"`
	Severity Severity `json:"severity"`
	// Excerpt is the matched text, truncated: enough to recognise, not enough to
	// replay a whole injection.
	Excerpt string `json:"excerpt"`
	// Explanation is the rule's reason.
	Explanation string `json:"explanation"`
}

// Verdict is the aggregate judgement.
type Verdict string

const (
	// VerdictClean means nothing matched.
	VerdictClean Verdict = "clean"
	// VerdictSuspicious means something matched that is worth reporting.
	VerdictSuspicious Verdict = "suspicious"
	// VerdictMalicious means a pattern matched that has no innocent reading.
	VerdictMalicious Verdict = "malicious"
)

// Report is what a scan found.
type Report struct {
	Verdict Verdict `json:"verdict"`
	// Severity is the strongest finding's severity.
	Severity Severity `json:"severity"`
	// Findings are every match, ordered strongest first.
	Findings []Finding `json:"findings,omitempty"`
	// Scanned is how many characters were inspected.
	Scanned int `json:"scanned"`
}

// Blocked reports whether the strongest finding is enough to refuse on its own.
func (r Report) Blocked() bool { return r.Verdict == VerdictMalicious }

// Names returns the names of the rules that matched, which is what a log line
// wants rather than a list of structures.
func (r Report) Names() []string {
	names := make([]string, 0, len(r.Findings))
	for _, finding := range r.Findings {
		names = append(names, finding.Rule)
	}
	return names
}

// maxExcerpt bounds what a report carries. An injection can be long, and a report
// that quotes all of it becomes a second copy of the attack in the logs.
const maxExcerpt = 120

// Detector inspects text for injection attempts.
type Detector struct {
	rules []Rule
}

// NewDetector builds a detector carrying the default rules.
func NewDetector() *Detector { return &Detector{rules: defaultRules()} }

// NewDetectorWith builds a detector from a caller supplied rule set. A nil or
// empty set falls back to the defaults rather than to a detector that finds
// nothing, because a silently empty detector looks exactly like a clean input.
func NewDetectorWith(rules []Rule) *Detector {
	if len(rules) == 0 {
		return NewDetector()
	}
	kept := make([]Rule, 0, len(rules))
	for _, rule := range rules {
		if rule.Pattern == nil || rule.Name == "" {
			continue
		}
		kept = append(kept, rule)
	}
	if len(kept) == 0 {
		return NewDetector()
	}
	return &Detector{rules: kept}
}

// Rules reports the active rules.
func (d *Detector) Rules() []Rule {
	out := make([]Rule, len(d.rules))
	copy(out, d.rules)
	return out
}

// Scan inspects text.
func (d *Detector) Scan(text string) Report {
	report := Report{Verdict: VerdictClean, Severity: SeverityLow, Scanned: len(text)}
	if strings.TrimSpace(text) == "" {
		return report
	}
	// Homoglyphs and zero width characters are stripped before matching, because
	// a defence that can be bypassed by an invisible character is not a defence.
	normalized := normalize(text)
	// Two candidates are matched: the text with its spacing squeezed, which is what
	// ordinary prose looks like, and the text with its spacing intact, because the
	// run of spaces between letters is the signal deSpaced needs to recover a
	// hidden word.
	//
	// The second candidate is only built when a double space is present at all.
	// That is the one signal deSpaced can recover from, so a message without one
	// has nothing to gain, and tokenising a megabyte of ordinary prose on every
	// turn is a cost with no return.
	candidates := []string{collapseSpacing(normalized)}
	if strings.Contains(normalized, "  ") {
		candidates = append(candidates, deSpaced(normalized))
	}
	for _, candidate := range candidates {
		for _, rule := range d.rules {
			match := rule.Pattern.FindStringIndex(candidate)
			if match == nil {
				continue
			}
			report.Findings = append(report.Findings, Finding{
				Rule:        rule.Name,
				Severity:    rule.Severity,
				Excerpt:     excerpt(normalized, match[0], match[1]),
				Explanation: rule.Explanation,
			})
			if rule.Severity.rank() > report.Severity.rank() {
				report.Severity = rule.Severity
			}
		}
	}
	// A word recovered from a spaced out run matches twice, once per candidate,
	// and one finding is the truth: there was one attempt, not two.
	report.Findings = dedupeFindings(report.Findings)
	if len(report.Findings) == 0 {
		return report
	}
	sortFindings(report.Findings)
	// A high severity finding is decisive on its own. A medium one is not, which
	// is why the rules are graded rather than counted: a message that merely
	// discusses prompt injection is not an attack.
	if report.Severity == SeverityHigh {
		report.Verdict = VerdictMalicious
	} else {
		report.Verdict = VerdictSuspicious
	}
	return report
}

// dedupeFindings keeps the first finding per rule, in the order they were found.
func dedupeFindings(findings []Finding) []Finding {
	seen := make(map[string]bool, len(findings))
	kept := findings[:0]
	for _, finding := range findings {
		if seen[finding.Rule] {
			continue
		}
		seen[finding.Rule] = true
		kept = append(kept, finding)
	}
	return kept
}

func sortFindings(findings []Finding) {
	// A stable sort by severity, so the strongest finding is first and two equal
	// findings keep the order the rules were declared in.
	for outer := 1; outer < len(findings); outer++ {
		for inner := outer; inner > 0; inner-- {
			if findings[inner].Severity.rank() <= findings[inner-1].Severity.rank() {
				break
			}
			findings[inner], findings[inner-1] = findings[inner-1], findings[inner]
		}
	}
}

func excerpt(text string, start, end int) string {
	value := text[start:end]
	// The excerpt is taken from the normalized text, so the invisible characters
	// are gone from the report too.
	if len(value) > maxExcerpt {
		return value[:maxExcerpt] + "..."
	}
	return value
}

// normalize removes the tricks that make a pattern unmatchable.
//
// Line structure is preserved. A rule anchored at the start of a line is how a
// forged role header or a forged document section is recognised, so collapsing
// newlines into spaces would quietly disable the rules that matter most.
func normalize(text string) string {
	var builder strings.Builder
	builder.Grow(len(text))
	for _, character := range text {
		switch {
		case character == '\u200b' || character == '\u200c' || character == '\u200d' ||
			character == '\ufeff' || character == '\u2060':
			// Zero width characters carry no meaning in a prompt and exist here only
			// to hide one.
			continue
		case unicode.Is(unicode.Cf, character):
			// Other format characters, including the right to left overrides used to
			// reverse how a sentence reads.
			continue
		case character == '\r':
			// A carriage return is normalised to a line feed so a rule anchored on a
			// line works the same on every platform's line ending.
			builder.WriteByte('\n')
		default:
			builder.WriteRune(character)
		}
	}
	// Line structure is preserved. A rule anchored at the start of a line is how a
	// forged role header or a forged document section is recognised, so collapsing
	// newlines into spaces would quietly disable the rules that matter most.
	//
	// Runs of spaces are preserved as well, because deSpaced needs them as the
	// boundary signal that recovers a word written one letter at a time.
	return builder.String()
}

// collapseSpacing squeezes runs of spaces and tabs, which defeats a pattern
// padded with whitespace without touching where the lines are.
func collapseSpacing(text string) string {
	var builder strings.Builder
	builder.Grow(len(text))
	space := false
	for _, character := range text {
		if character == ' ' || character == '\t' {
			space = true
			continue
		}
		if space && builder.Len() > 0 && !strings.HasSuffix(builder.String(), "\n") {
			builder.WriteByte(' ')
		}
		space = false
		builder.WriteRune(character)
	}
	return builder.String()
}

// deSpaced rebuilds words that were spelled out one letter at a time.
//
// A run of two or more spaces is treated as a hard word boundary, because that is
// the one signal left once the letters are separated. Within each segment the
// consecutive single letter tokens are joined back into words, so
// "I g n o r e   a l l" becomes "Ignore all" rather than one fused string that no
// rule could match.
//
// The threshold is three letters. A shorter run is left alone because ordinary
// text does contain isolated single letters, and because a wrong join is only
// ever used for matching, never shown to a reader.
//
// The limit is worth stating: a message that spaces every letter with single
// spaces destroys the only boundary signal there is, and this does not recover
// it, so the caller only builds the candidate when a double space is present. The
// zero width forms it does handle are the ones that appear in practice.
func deSpaced(text string) string {
	segments := strings.Split(text, "  ")
	joinedAny := false
	for index, segment := range segments {
		fields := strings.Fields(segment)
		if len(fields) < minSpacedRun {
			continue
		}
		var builder strings.Builder
		runLength := 0
		longest := 0
		for position, field := range fields {
			if len([]rune(field)) == 1 {
				builder.WriteString(field)
				runLength++
				if runLength > longest {
					longest = runLength
				}
				continue
			}
			// A multi letter token closes the run, so it needs a space before it
			// unless it is the first thing in the segment.
			if position > 0 {
				builder.WriteByte(' ')
			}
			builder.WriteString(field)
			runLength = 0
		}
		if longest >= minSpacedRun {
			segments[index] = builder.String()
			joinedAny = true
		}
	}
	if !joinedAny {
		return text
	}
	return strings.Join(segments, " ")
}

// minSpacedRun is the shortest run of single letter tokens treated as a hidden
// word.
const minSpacedRun = 3

// mustCompile builds a pattern that is known good at build time, so a typo is a
// compile error rather than a rule that silently never matches.
func mustCompile(pattern string) *regexp.Regexp {
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		panic(fmt.Sprintf("security: the rule pattern %q does not compile: %v", pattern, err))
	}
	return compiled
}

// defaultRules are the patterns the design names, plus the shapes that have
// recurred against agents since.
func defaultRules() []Rule {
	return []Rule{
		{
			Name:        "instruction_override",
			Pattern:     mustCompile(`(?i)\b(ignore|disregard|forget|override)\b[^.]{0,40}\b(previous|prior|earlier|above|all|any)\b[^.]{0,20}\b(instruction|instructions|prompt|prompts|rule|rules|direction|directions)\b`),
			Severity:    SeverityHigh,
			Explanation: "an attempt to void the instructions already given",
		},
		{
			Name:        "fake_system_header",
			Pattern:     mustCompile(`(?i)(^|\n)\s*(system|assistant|developer)\s*:`),
			Severity:    SeverityHigh,
			Explanation: "a forged role header pretending the text came from the system",
		},
		{
			Name:        "chat_template_marker",
			Pattern:     mustCompile(`<\|(im_start|im_end|system|endoftext|start_header_id)\|>|<<\s*SYS\s*>>|\[\s*/\s*INST\s*\]`),
			Severity:    SeverityHigh,
			Explanation: "a chat template marker, which a user has no reason to send",
		},
		{
			Name:        "system_prompt_exfiltration",
			Pattern:     mustCompile(`(?i)\b(reveal|repeat|print|show|output|display|dump|tell me)\b[^.]{0,40}\b(system prompt|initial instruction|instructions above|your instructions|your prompt)\b`),
			Severity:    SeverityHigh,
			Explanation: "an attempt to extract the instructions the model was given",
		},
		{
			Name:        "instruction_leak_suppression",
			Pattern:     mustCompile(`(?i)\b(do not|don'?t|never)\b[^.]{0,30}\b(tell|inform|mention|show|reveal|disclose|notify)\b[^.]{0,20}\b(the )?(user|human)\b`),
			Severity:    SeverityHigh,
			Explanation: "an attempt to stop the model from reporting what it is doing",
		},
		{
			Name:        "persona_override",
			Pattern:     mustCompile(`(?i)\byou are (now|no longer)\b|\bfrom now on you\b|\bact as (if you|a)\b|\bpretend (to be|you are)\b|\bdeveloper mode\b|\bjailbreak\b`),
			Severity:    SeverityHigh,
			Explanation: "an attempt to replace the model's role",
		},
		{
			Name:        "section_forgery",
			Pattern:     mustCompile(`(?im)^\s*#{2,}\s*(system|instruction|instructions|new instructions|important)\b`),
			Severity:    SeverityMedium,
			Explanation: "a forged document section that reads like a system directive",
		},
		{
			Name:        "delimiter_forgery",
			Pattern:     mustCompile(`(?i)(begin|end)\s+(system|instruction|admin)\s+(prompt|mode|block)\b`),
			Severity:    SeverityHigh,
			Explanation: "a forged delimiter pretending to open a privileged region",
		},
		{
			Name:        "encoded_payload",
			Pattern:     mustCompile(`\b[A-Za-z0-9+/]{120,}={0,2}\b|\b[0-9a-fA-F]{120,}\b`),
			Severity:    SeverityMedium,
			Explanation: "a long encoded run, which is how an instruction is smuggled past a reader",
		},
		{
			Name:        "encoded_instructions",
			Pattern:     mustCompile(`(?i)\b(base64|rot13|hex)\s*(decode|decoded|encoded)?\b[^.]{0,30}\b(follow|execute|run|do|instruction|command)\b`),
			Severity:    SeverityMedium,
			Explanation: "an instruction to decode something and act on it",
		},
		{
			Name:        "tool_use_pressure",
			Pattern:     mustCompile(`(?i)\b(call|invoke|run|execute)\b[^.]{0,30}\b(tool|function|command)\b[^.]{0,30}\b(with(out)? (asking|confirmation|permission))\b`),
			Severity:    SeverityMedium,
			Explanation: "pressure to use a tool without the confirmation the mode requires",
		},
		{
			Name:        "urgent_authority",
			Pattern:     mustCompile(`(?i)\b(important|urgent|critical|attention)\b\s*[:!]\s*(you must|do not|ignore|disregard|new instruction)`),
			Severity:    SeverityMedium,
			Explanation: "false urgency wrapped around an instruction, a shape user text rarely has",
		},
		{
			Name:        "injection_discussion",
			Pattern:     mustCompile(`(?i)\b(prompt injection|ignore previous instructions)\b`),
			Severity:    SeverityLow,
			Explanation: "text discussing injection rather than performing it, recorded so a reader can see it",
		},
	}
}
