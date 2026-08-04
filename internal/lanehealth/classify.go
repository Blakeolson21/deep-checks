// Package lanehealth records which configured agent lanes are currently
// unusable because the provider's quota is exhausted, so the pipeline's
// mid-run fallback can skip a dead lane instead of paying a full agent spawn
// to rediscover it.
//
// The classifier only ever sees the text of a FAILED invocation whose message
// the adapters build from the provider's stderr and structured error channel
// (see internal/agent/codex.go and claude.go) - never from agent-authored
// stdout. That matters because the banner strings below appear verbatim in
// reviewed source and in this package's own tests; matching agent output
// would let a repository under review mark a healthy lane dead. The one
// adapter error that does quote the agent's own message, *agent.OutputParseError,
// is excluded by the caller in internal/agent/lanehealth.go before it gets here.
package lanehealth

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultCooldown is how long a lane stays marked when the provider did not
// state a reset time, or stated one that could not be trusted.
//
// It is deliberately short. Being wrong-long parks a healthy lane for hours;
// being wrong-short costs exactly one wasted invocation per lane per hour and
// self-corrects, because the next failure re-marks the lane with a fresh
// deadline. An hour is enough to stop a burst of runs from each rediscovering
// the same dead lane, which is the failure this package exists to prevent.
const DefaultCooldown = time.Hour

// MaxCooldown bounds any parsed reset time. The longest real provider window
// is a weekly limit, so anything further out is a misparse and falls back to
// DefaultCooldown rather than parking a lane for months.
const MaxCooldown = 8 * 24 * time.Hour

// ProbeInterval is how often one invocation is let through a marked lane to
// test whether it recovered early.
//
// A mark can otherwise only be undone by the reset time it recorded, and the
// evidence that would undo it - a successful invocation - is exactly what the
// mark suppresses. So a stated reset days out is trusted from a single
// observation with nothing able to correct it, even though the provider's own
// remedy (buying credits, raising the plan) restores the same account
// immediately. One probe per interval caps that staleness at the interval and
// costs the same wasted spawn per lane per hour the wrong-short direction
// already accepts.
const ProbeInterval = DefaultCooldown

// maxReasonRunes bounds the banner excerpt kept for the failure message so a
// noisy stderr tail cannot grow the state file or the run error without limit.
const maxReasonRunes = 200

// Outage records that one agent lane is quota-exhausted until Until.
//
// LastProbeAt is when an invocation was last let through the mark to test for
// early recovery; it is zero until the first probe is claimed.
type Outage struct {
	Lane        string    `json:"lane"`
	Until       time.Time `json:"until"`
	Reason      string    `json:"reason,omitempty"`
	ObservedAt  time.Time `json:"observed_at"`
	LastProbeAt time.Time `json:"last_probe_at,omitempty"`
}

// quotaBanners are the provider strings that mean "this account cannot run
// anything until its quota resets". They stay narrow on purpose: both CLIs
// ship many unrelated "... limit reached" messages (concurrency, context,
// budget, subagent spawn, fast mode), and marking a lane dead for hours on one
// of those would be strictly worse than the bug this package fixes.
var quotaBanners = []*regexp.Regexp{
	// Codex, observed verbatim in ~/.no-mistakes/state.sqlite:
	// "You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage
	//  to purchase more credits or try again at Aug 7th, 2026 11:06 PM."
	regexp.MustCompile(`(?i)\byou'?ve hit your usage limit\b`),
	// Claude renders "You've hit your <label> limit", where <label> comes from a
	// fixed table: session (5-hour), weekly, Opus, Sonnet, usage credit, and the
	// spend variants. "fast limit" is deliberately absent - fast mode degrades
	// to the normal model rather than making the lane unusable.
	regexp.MustCompile(`(?i)\byou'?ve hit your (?:session|weekly|monthly|opus|sonnet|usage credit|individual usage|individual spend|org's monthly)\b[^.\n]{0,20}\blimit\b`),
	// Claude / Anthropic API terminal classifications.
	regexp.MustCompile(`(?i)\busage limit (?:reached|exceeded)\b`),
	regexp.MustCompile(`(?i)\byou'?re out of usage credits\b`),
	regexp.MustCompile(`(?i)\b(?:org|organization) is out of usage\b`),
	regexp.MustCompile(`(?i)\bexceeded your current quota\b`),
}

// notOutage suppresses banner matches that are advisory rather than terminal.
// The grace-window notice is injected into a still-running agent's context, so
// it means "wrap up", not "this lane cannot run".
var notOutage = []*regexp.Regexp{
	regexp.MustCompile(`(?i)usage limit reached[^.\n]{0,24}grace window`),
}

// Classify reports whether a failed invocation's error text is a provider
// quota-exhaustion banner, and if so when the lane is expected to recover.
func Classify(lane, text string, now time.Time) (Outage, bool) {
	if strings.TrimSpace(text) == "" {
		return Outage{}, false
	}
	for _, suppress := range notOutage {
		if suppress.MatchString(text) {
			return Outage{}, false
		}
	}
	matched := false
	for _, banner := range quotaBanners {
		if banner.MatchString(text) {
			matched = true
			break
		}
	}
	if !matched {
		return Outage{}, false
	}

	until, ok := parseResetTime(text, now)
	if !ok || !until.After(now) || until.After(now.Add(MaxCooldown)) {
		until = now.Add(DefaultCooldown)
	}
	return Outage{
		Lane:       lane,
		Until:      until,
		Reason:     excerpt(text),
		ObservedAt: now,
	}, true
}

var (
	// "resets in 2h 15m"
	relativeResetRE = regexp.MustCompile(`(?i)\bresets? in ((?:\d+\s*[hm]\s*)+)`)
	// "try again at Aug 7th, 2026 11:06 PM" / "resets Aug 7, 11:06 PM"
	datedResetRE = regexp.MustCompile(`(?i)\b(?:try again at|resets?(?: at)?)\s+([A-Za-z]{3,9})\.?\s+(\d{1,2})(?:st|nd|rd|th)?,?\s*(\d{4})?,?\s*(\d{1,2})(?::(\d{2}))?\s*([AaPp])\.?[Mm]\b`)
	// "try again at 9:15 PM" / "resets 3pm"
	clockResetRE   = regexp.MustCompile(`(?i)\b(?:try again at|resets?(?: at)?)\s+(\d{1,2})(?::(\d{2}))?\s*([AaPp])\.?[Mm]\b`)
	relativePartRE = regexp.MustCompile(`(?i)(\d+)\s*([hm])`)
	// Claude prints the reset zone right after the clock time, as in the real
	// banner "You've hit your session limit - resets 10:50am (America/Chicago)".
	// Only the slashed IANA form and UTC/GMT are accepted so a parenthesized
	// word or path in surrounding stderr cannot be read as a zone.
	bannerZoneRE = regexp.MustCompile(`\((UTC|GMT|[A-Za-z_]+/[A-Za-z_+0-9-]+(?:/[A-Za-z_+0-9-]+)?)\)`)
)

// bannerZoneWindow bounds how far past the reset phrase a stated zone may sit.
// The provider prints it immediately after the time; anything further away
// belongs to unrelated stderr.
const bannerZoneWindow = 40

// resetLocation returns the zone the banner states for a reset it printed at
// matchEnd, falling back to the caller's zone when none is stated or the host
// cannot load it. Discarding a stated zone is not neutral: reading a reset that
// has already passed in its own zone as "not yet today" rolls it forward a full
// day, which is exactly the wrong-long direction DefaultCooldown rejects.
func resetLocation(text string, matchEnd int, fallback *time.Location) *time.Location {
	if matchEnd < 0 || matchEnd > len(text) {
		return fallback
	}
	tail := text[matchEnd:]
	if len(tail) > bannerZoneWindow {
		tail = tail[:bannerZoneWindow]
	}
	m := bannerZoneRE.FindStringSubmatch(tail)
	if m == nil {
		return fallback
	}
	loc, err := time.LoadLocation(m[1])
	if err != nil || loc == nil {
		return fallback
	}
	return loc
}

// parseResetTime extracts the provider's stated recovery time. A bare clock
// time means the next occurrence of that local time, which is how both CLIs
// abbreviate a reset later today.
func parseResetTime(text string, now time.Time) (time.Time, bool) {
	if m := relativeResetRE.FindStringSubmatch(text); m != nil {
		var total time.Duration
		for _, part := range relativePartRE.FindAllStringSubmatch(m[1], -1) {
			value, err := strconv.Atoi(part[1])
			if err != nil {
				continue
			}
			if strings.EqualFold(part[2], "h") {
				total += time.Duration(value) * time.Hour
			} else {
				total += time.Duration(value) * time.Minute
			}
		}
		if total > 0 {
			return now.Add(total), true
		}
	}

	if idx := datedResetRE.FindStringSubmatchIndex(text); idx != nil {
		m := submatches(text, idx)
		loc := resetLocation(text, idx[1], now.Location())
		if month, ok := parseMonth(m[1]); ok {
			day, dayErr := strconv.Atoi(m[2])
			hour, hourOK := parseClockHour(m[4], m[6])
			minute := 0
			if m[5] != "" {
				parsed, err := strconv.Atoi(m[5])
				if err == nil {
					minute = parsed
				}
			}
			if dayErr == nil && hourOK {
				year := now.In(loc).Year()
				if m[3] != "" {
					if parsed, err := strconv.Atoi(m[3]); err == nil {
						year = parsed
					}
				}
				candidate := time.Date(year, month, day, hour, minute, 0, 0, loc)
				// A dateless banner near a year boundary means the next occurrence.
				if m[3] == "" && candidate.Before(now) {
					candidate = candidate.AddDate(1, 0, 0)
				}
				return candidate, true
			}
		}
	}

	if idx := clockResetRE.FindStringSubmatchIndex(text); idx != nil {
		m := submatches(text, idx)
		hour, ok := parseClockHour(m[1], m[3])
		if ok {
			loc := resetLocation(text, idx[1], now.Location())
			minute := 0
			if m[2] != "" {
				if parsed, err := strconv.Atoi(m[2]); err == nil {
					minute = parsed
				}
			}
			there := now.In(loc)
			candidate := time.Date(there.Year(), there.Month(), there.Day(), hour, minute, 0, 0, loc)
			if !candidate.After(now) {
				candidate = candidate.AddDate(0, 0, 1)
			}
			return candidate, true
		}
	}

	return time.Time{}, false
}

// submatches rebuilds the string groups FindStringSubmatch would have returned
// from an index match, so a single pass yields both the groups and the offset
// the stated zone is searched from.
func submatches(text string, idx []int) []string {
	groups := make([]string, len(idx)/2)
	for i := range groups {
		start, end := idx[2*i], idx[2*i+1]
		if start >= 0 && end >= start {
			groups[i] = text[start:end]
		}
	}
	return groups
}

func parseClockHour(hourText, meridiem string) (int, bool) {
	hour, err := strconv.Atoi(hourText)
	if err != nil || hour < 1 || hour > 12 {
		return 0, false
	}
	if strings.EqualFold(meridiem, "p") {
		if hour != 12 {
			hour += 12
		}
	} else if hour == 12 {
		hour = 0
	}
	return hour, true
}

var months = map[string]time.Month{
	"jan": time.January, "feb": time.February, "mar": time.March,
	"apr": time.April, "may": time.May, "jun": time.June,
	"jul": time.July, "aug": time.August, "sep": time.September,
	"oct": time.October, "nov": time.November, "dec": time.December,
}

func parseMonth(name string) (time.Month, bool) {
	if len(name) < 3 {
		return 0, false
	}
	month, ok := months[strings.ToLower(name[:3])]
	return month, ok
}

// excerpt collapses the banner to a single bounded line for display.
func excerpt(text string) string {
	collapsed := strings.Join(strings.Fields(text), " ")
	runes := []rune(collapsed)
	if len(runes) <= maxReasonRunes {
		return collapsed
	}
	return string(runes[:maxReasonRunes-3]) + "..."
}
