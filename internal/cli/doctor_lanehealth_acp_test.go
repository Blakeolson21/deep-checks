package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/lanehealth"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

// An ACP-driven lane records its outage under the identity the agent reports
// ("acp:<target>"), not under the alias the operator configured, so doctor has
// to resolve the configured name the same way before it can see the cooldown.
// Reading the row by the alias reports the parked lane as installed and
// runnable, which is the invisibility this surface exists to remove.
func TestDoctorReportsAQuotaExhaustedACPAliasLane(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	t.Setenv("NM_HOME", t.TempDir())

	binDir := t.TempDir()
	writeFakeBinary(t, binDir, "cursor-agent")
	writeFakeBinary(t, binDir, "acpx")
	t.Setenv("PATH", binDir)

	p, err := paths.New()
	if err != nil {
		t.Fatalf("paths.New: %v", err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: cursor\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	until := time.Now().Add(72 * time.Hour).Truncate(time.Minute)
	store := lanehealth.NewStore(p.LaneHealthFile(), nil)
	if err := store.Mark(lanehealth.Outage{
		Lane:   "acp:cursor",
		Until:  until,
		Reason: "You've hit your usage limit",
	}); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}

	agentLine := doctorAgentLine(t, out, "cursor")
	if !strings.Contains(agentLine, "quota-exhausted") {
		t.Fatalf("cursor row must report the quota cooldown recorded for acp:cursor:\n%s", agentLine)
	}
	if !strings.Contains(agentLine, until.Local().Format("2006-01-02 15:04 MST")) {
		t.Fatalf("cursor row must name the reset time:\n%s", agentLine)
	}

	gateLine := doctorLineContaining(t, out, "gate validation")
	if !strings.Contains(gateLine, "quota-exhausted") {
		t.Fatalf("gate validation must not report the parked alias lane as simply runnable:\n%s", gateLine)
	}
	if !strings.Contains(gateLine, until.Local().Format("2006-01-02 15:04 MST")) {
		t.Fatalf("gate validation must name the reset time:\n%s", gateLine)
	}
}
