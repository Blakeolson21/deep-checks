//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// writeQuotaExhaustedCodex replaces the codex lane with a stub that emits the
// exact banner recorded in ~/.no-mistakes/state.sqlite during the 2026-08-04
// incident and exits non-zero, and that appends one line per invocation so the
// test can prove a later run never launched it.
func writeQuotaExhaustedCodex(t *testing.T, h *Harness, callLog string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the quota stub is a POSIX shell script")
	}
	path := filepath.Join(h.BinDir, "codex")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove codex symlink: %v", err)
	}
	script := "#!/bin/sh\n" +
		"echo invoked >> " + shellQuote(callLog) + "\n" +
		`printf "You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage ` +
		`to purchase more credits or try again at Aug 7th, 2026 11:06 PM.\n" >&2` + "\n" +
		"exit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write codex quota stub: %v", err)
	}
	return path
}

func codexCallCount(t *testing.T, callLog string) int {
	t.Helper()
	data, err := os.ReadFile(callLog)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read codex call log: %v", err)
	}
	return len(strings.Fields(string(data)))
}

// TestAgentLaneQuotaCooldownJourney is the regression for the 2026-08-04
// incident (runs 01KZ5DX4Y4R9Z0AQN3B53STP5Y, 01KZ5DHW3R6H8KWFCN29KCBA61,
// 01KZ5BV1Z2W2DVV7018PZM7CC0 and a dozen more): every run fell onto an
// exhausted Codex account and burned a full agent launch rediscovering it.
//
// It drives the real binary end to end: the first run must fail over to the
// healthy lane and persist the outage, and the second run must skip the dead
// lane without launching it at all.
func TestAgentLaneQuotaCooldownJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})

	callLog := filepath.Join(t.TempDir(), "codex-calls.log")
	codexPath := writeQuotaExhaustedCodex(t, h, callLog)

	// Order the lanes codex-first so every agent invocation starts on the
	// exhausted lane, which is the shape the incident had.
	globalConfig := filepath.Join(h.NMHome, "config.yaml")
	claudePath := filepath.Join(h.BinDir, "claude")
	source := "agent: [codex, claude]\n" +
		"log_level: debug\n" +
		"agent_path_override:\n" +
		"  codex: " + shellQuote(codexPath) + "\n" +
		"  claude: " + shellQuote(claudePath) + "\n" +
		"auto_fix:\n" +
		"  rebase: 0\n  lint: 0\n  test: 0\n  review: 0\n  document: 0\n  ci: 0\n"
	if err := os.WriteFile(globalConfig, []byte(source), 0o644); err != nil {
		t.Fatalf("write global config: %v", err)
	}

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	const firstBranch = "feature/quota-lane-first"
	h.CommitChange(firstBranch, "first.txt", "first\n", "add first feature")
	h.PushToGate(firstBranch)
	first := h.WaitForRun(firstBranch, 120*time.Second)
	if first.Status != types.RunCompleted {
		t.Fatalf("first run status = %s, want completed (error=%v)", first.Status, first.Error)
	}

	discovered := codexCallCount(t, callLog)
	if discovered == 0 {
		t.Fatalf("the first run must actually try the codex lane before marking it")
	}

	// The outage is persisted, so it outlives this run and this process.
	statePath := filepath.Join(h.NMHome, "lane-health.json")
	stateData, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read lane-health state: %v", err)
	}
	var state struct {
		Lanes map[string]struct {
			Until  time.Time `json:"until"`
			Reason string    `json:"reason"`
		} `json:"lanes"`
	}
	if err := json.Unmarshal(stateData, &state); err != nil {
		t.Fatalf("parse lane-health state %q: %v", stateData, err)
	}
	outage, marked := state.Lanes["codex"]
	if !marked {
		t.Fatalf("codex must be marked quota-exhausted, got %s", stateData)
	}
	// The banner names Aug 7th 2026 11:06 PM, so the parsed reset must be that
	// instant rather than the conservative default.
	wantUntil := time.Date(2026, 8, 7, 23, 6, 0, 0, time.Local)
	if !outage.Until.Equal(wantUntil) {
		t.Fatalf("codex reset time = %s, want the banner's stated %s", outage.Until, wantUntil)
	}
	if !strings.Contains(outage.Reason, "usage limit") {
		t.Fatalf("recorded reason %q must quote the provider banner", outage.Reason)
	}

	// doctor is the operator-visible read surface for the same state.
	doctorOut, err := h.Run("doctor")
	if err != nil && !strings.Contains(doctorOut, "quota-exhausted") {
		t.Fatalf("doctor: %v\n%s", err, doctorOut)
	}
	if !strings.Contains(doctorOut, "quota-exhausted") {
		t.Fatalf("doctor must report the parked codex lane:\n%s", doctorOut)
	}

	// The whole point: the next run pays nothing to learn what the first run
	// already recorded.
	const secondBranch = "feature/quota-lane-second"
	h.CommitChange(secondBranch, "second.txt", "second\n", "add second feature")
	h.PushToGate(secondBranch)
	second := h.WaitForRun(secondBranch, 120*time.Second)
	if second.Status != types.RunCompleted {
		t.Fatalf("second run status = %s, want completed (error=%v)", second.Status, second.Error)
	}
	if after := codexCallCount(t, callLog); after != discovered {
		t.Fatalf("codex was launched %d more times on the second run; the marked lane must be skipped", after-discovered)
	}

	t.Logf("first run launched the exhausted codex lane %d time(s) and then failed over to claude", discovered)
	t.Logf("persisted outage: codex until %s", outage.Until.Local().Format("2006-01-02 15:04 MST"))
	t.Logf("second run launched codex 0 more times and completed on claude")
}
