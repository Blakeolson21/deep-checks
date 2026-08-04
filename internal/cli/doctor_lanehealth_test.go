package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/lanehealth"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

// A lane parked by a quota cooldown is installed and looks healthy, so without
// this row the operator has no way to see why the pipeline is not using it.
func TestDoctorReportsAQuotaExhaustedAgentLane(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	home := t.TempDir()
	t.Setenv("NM_HOME", home)

	binDir := t.TempDir()
	writeFakeBinary(t, binDir, "codex")
	t.Setenv("PATH", binDir)

	p, err := paths.New()
	if err != nil {
		t.Fatalf("paths.New: %v", err)
	}
	until := time.Now().Add(72 * time.Hour).Truncate(time.Minute)
	store := lanehealth.NewStore(p.LaneHealthFile(), nil)
	if err := store.Mark(lanehealth.Outage{
		Lane:   "codex",
		Until:  until,
		Reason: "You've hit your usage limit",
	}); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}

	line := doctorAgentLine(t, out, "codex")
	if !strings.Contains(line, "quota-exhausted") {
		t.Fatalf("codex row must report the quota cooldown:\n%s", line)
	}
	if !strings.Contains(line, until.Local().Format("2006-01-02 15:04 MST")) {
		t.Fatalf("codex row must name the reset time:\n%s", line)
	}
}

func TestDoctorReportsAnExpiredMarkAsHealthy(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	home := t.TempDir()
	t.Setenv("NM_HOME", home)

	binDir := t.TempDir()
	codexPath := writeFakeBinary(t, binDir, "codex")
	t.Setenv("PATH", binDir)

	p, err := paths.New()
	if err != nil {
		t.Fatalf("paths.New: %v", err)
	}
	// Written by a clock in the past so the mark is already expired when doctor
	// reads it with the real clock.
	past := time.Now().Add(-2 * time.Hour)
	store := lanehealth.NewStore(p.LaneHealthFile(), func() time.Time { return past.Add(-time.Hour) })
	if err := store.Mark(lanehealth.Outage{Lane: "codex", Until: past, Reason: "usage limit"}); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}

	line := doctorAgentLine(t, out, "codex")
	if strings.Contains(line, "quota-exhausted") {
		t.Fatalf("an expired mark must not park the lane:\n%s", line)
	}
	if !strings.Contains(line, codexPath) {
		t.Fatalf("codex row must report the resolved binary:\n%s", line)
	}
}
