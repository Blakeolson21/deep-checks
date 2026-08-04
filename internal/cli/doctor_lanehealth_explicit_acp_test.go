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

// A fallback lane configured as an explicit acp:<target> records its cooldown
// under that name, and doctor enumerates only the first-class agents and the
// registered aliases, so the renderer has to report what the store actually
// recorded rather than only the lanes it happens to list. Otherwise the pipeline
// skips the lane for its whole reset while doctor shows nothing at all.
func TestDoctorReportsAQuotaExhaustedExplicitACPTargetLane(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	t.Setenv("NM_HOME", t.TempDir())

	binDir := t.TempDir()
	writeFakeBinary(t, binDir, "claude")
	writeFakeBinary(t, binDir, "acpx")
	t.Setenv("PATH", binDir)

	p, err := paths.New()
	if err != nil {
		t.Fatalf("paths.New: %v", err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: [claude, acp:gemini]\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	until := time.Now().Add(72 * time.Hour).Truncate(time.Minute)
	store := lanehealth.NewStore(p.LaneHealthFile(), nil)
	if err := store.Mark(lanehealth.Outage{
		Lane:   "acp:gemini",
		Until:  until,
		Reason: "You've hit your usage limit",
	}); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}

	line := doctorAgentLine(t, out, "acp:gemini")
	if !strings.Contains(line, "quota-exhausted") {
		t.Fatalf("the acp:gemini lane must report its recorded cooldown:\n%s", line)
	}
	if !strings.Contains(line, until.Local().Format("2006-01-02 15:04 MST")) {
		t.Fatalf("the acp:gemini row must name the reset time:\n%s", line)
	}
}

// The recorded-state rows must not double up on a lane an enumerated row
// already reported, whatever name form the operator configured.
func TestDoctorReportsAMarkedAliasLaneExactlyOnce(t *testing.T) {
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
	until := time.Now().Add(72 * time.Hour).Truncate(time.Minute)
	store := lanehealth.NewStore(p.LaneHealthFile(), nil)
	if err := store.Mark(lanehealth.Outage{Lane: "acp:cursor", Until: until}); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}

	rows := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "quota-exhausted") && !strings.Contains(line, "gate validation") {
			rows++
		}
	}
	if rows != 1 {
		t.Fatalf("the marked cursor lane must produce exactly one agent row, got %d:\n%s", rows, out)
	}
}
