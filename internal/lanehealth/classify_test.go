package lanehealth

import (
	"strings"
	"testing"
	"time"
)

func mustTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", value, time.Local)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return parsed
}

// The banner text below is copied verbatim from the runs recorded in
// ~/.no-mistakes/state.sqlite during the 2026-08-04 incident, so recognition is
// pinned to what the providers actually emit rather than to a paraphrase.
const codexDatedBanner = "codex exited: exit status 1: You've hit your usage limit. " +
	"Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Aug 7th, 2026 11:06 PM.; " +
	"Reading additional input from stdin..."

const codexTimeOnlyBanner = "codex exited: exit status 1: You've hit your usage limit. " +
	"Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at 9:15 PM.; " +
	"Reading additional input from stdin..."

func TestClassifyRecognizesCodexDatedBanner(t *testing.T) {
	now := mustTime(t, "2026-08-04 03:44")
	outage, ok := Classify("codex", codexDatedBanner, now)
	if !ok {
		t.Fatalf("expected codex quota banner to be recognized")
	}
	if outage.Lane != "codex" {
		t.Fatalf("lane = %q, want codex", outage.Lane)
	}
	want := mustTime(t, "2026-08-07 23:06")
	if !outage.Until.Equal(want) {
		t.Fatalf("Until = %s, want %s", outage.Until, want)
	}
	if !strings.Contains(outage.Reason, "usage limit") {
		t.Fatalf("Reason = %q, want it to quote the banner", outage.Reason)
	}
}

func TestClassifyRecognizesCodexTimeOnlyBannerAsNextOccurrence(t *testing.T) {
	now := mustTime(t, "2026-08-04 03:44")
	outage, ok := Classify("codex", codexTimeOnlyBanner, now)
	if !ok {
		t.Fatalf("expected codex time-only quota banner to be recognized")
	}
	want := mustTime(t, "2026-08-04 21:15")
	if !outage.Until.Equal(want) {
		t.Fatalf("Until = %s, want the next 9:15 PM (%s)", outage.Until, want)
	}
}

func TestClassifyRecognizesCodexTimeOnlyBannerRollingToTomorrow(t *testing.T) {
	now := mustTime(t, "2026-08-04 22:30")
	outage, ok := Classify("codex", codexTimeOnlyBanner, now)
	if !ok {
		t.Fatalf("expected codex time-only quota banner to be recognized")
	}
	want := mustTime(t, "2026-08-05 21:15")
	if !outage.Until.Equal(want) {
		t.Fatalf("Until = %s, want tomorrow's 9:15 PM (%s)", outage.Until, want)
	}
}

func TestClassifyRecognizesClaudeBanners(t *testing.T) {
	now := mustTime(t, "2026-08-04 03:44")
	cases := []struct {
		name string
		text string
		want time.Time
	}{
		{
			name: "session limit with clock reset",
			text: "claude exited: exit status 1: You've hit your session limit · resets 9:15 PM",
			want: mustTime(t, "2026-08-04 21:15"),
		},
		{
			// Captured verbatim from claude-acct at 2026-08-04 10:24 CDT, when
			// this change's own gate run found every lane exhausted. Note the
			// lowercase meridiem with no space and the trailing zone in
			// parentheses, neither of which the synthesized cases cover.
			name: "session limit exactly as the CLI printed it",
			text: "You've hit your session limit · resets 10:50am (America/Chicago)",
			want: mustTime(t, "2026-08-04 10:50"),
		},
		{
			name: "weekly limit with dated reset",
			text: "claude exited: exit status 1: You've hit your weekly limit · resets Aug 7, 11:06 PM",
			want: mustTime(t, "2026-08-07 23:06"),
		},
		{
			name: "opus limit with relative reset",
			text: "claude exited: exit status 1: You've hit your Opus limit · resets in 2h 15m",
			want: now.Add(2*time.Hour + 15*time.Minute),
		},
		{
			name: "usage limit reached without reset time",
			text: "claude exited: exit status 1: error: usage limit reached",
			want: now.Add(DefaultCooldown),
		},
		{
			name: "out of usage credits",
			text: "claude exited: exit status 1: You're out of usage credits",
			want: now.Add(DefaultCooldown),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outage, ok := Classify("claude", tc.text, now)
			if !ok {
				t.Fatalf("expected %q to be recognized as a quota banner", tc.text)
			}
			if !outage.Until.Equal(tc.want) {
				t.Fatalf("Until = %s, want %s", outage.Until, tc.want)
			}
		})
	}
}

// The Claude CLI ships many unrelated "... limit reached" strings. Marking a
// lane dead for hours on one of those is strictly worse than the bug this
// package fixes, so recognition must stay narrow.
func TestClassifyRejectsNonQuotaLimitText(t *testing.T) {
	now := mustTime(t, "2026-08-04 03:44")
	cases := []string{
		"claude exited: exit status 1: Concurrent subagent limit reached. You can run 3 subagents at once.",
		"claude exited: exit status 1: Context limit reached",
		"claude exited: exit status 1: Budget limit reached ($5.00 spent of the $5.00 maximum)",
		"claude exited: exit status 1: You've hit your fast limit · resets in 4m",
		"claude exited: exit status 1: Fast mode disabled · usage credit limit reached",
		"claude exited: exit status 1: Subagent spawn limit reached (10 of 10 agents spawned)",
		"codex exited: exit status 1: stream disconnected before completion",
		"[Usage limit reached — grace window active. Wrap up: finish or checkpoint.]",
	}
	for _, text := range cases {
		if outage, ok := Classify("claude", text, now); ok {
			t.Fatalf("text %q must not be a quota outage (got until %s)", text, outage.Until)
		}
	}
}

func TestClassifyIgnoresEmptyAndUnrelatedErrors(t *testing.T) {
	now := mustTime(t, "2026-08-04 03:44")
	for _, text := range []string{"", "codex exited: exit status 1: panic: nil map"} {
		if _, ok := Classify("codex", text, now); ok {
			t.Fatalf("text %q must not be a quota outage", text)
		}
	}
}

// A garbled or absurd reset time must never park a lane past the longest real
// provider window; a reset already in the past means the parse was wrong.
func TestClassifyClampsImplausibleResetTimes(t *testing.T) {
	now := mustTime(t, "2026-08-04 03:44")
	far := "codex exited: exit status 1: You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage " +
		"to purchase more credits or try again at Aug 7th, 2126 11:06 PM."
	outage, ok := Classify("codex", far, now)
	if !ok {
		t.Fatalf("expected banner to be recognized")
	}
	if !outage.Until.Equal(now.Add(DefaultCooldown)) {
		t.Fatalf("Until = %s, want the default cooldown %s", outage.Until, now.Add(DefaultCooldown))
	}
}

func TestClassifyBoundsReasonLength(t *testing.T) {
	now := mustTime(t, "2026-08-04 03:44")
	noisy := codexDatedBanner + strings.Repeat(" trailing stderr noise", 200)
	outage, ok := Classify("codex", noisy, now)
	if !ok {
		t.Fatalf("expected banner to be recognized")
	}
	if len([]rune(outage.Reason)) > maxReasonRunes {
		t.Fatalf("Reason is %d runes, want at most %d", len([]rune(outage.Reason)), maxReasonRunes)
	}
}
