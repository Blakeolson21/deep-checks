package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/lanehealth"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The pipeline agent must consume persisted lane health, otherwise every run
// rediscovers an exhausted lane by spawning it - the 2026-08-04 incident, where
// a dozen consecutive runs each failed on the same dead Codex quota. Marking
// every lane also proves the terminal message names each lane's reset time
// instead of failing bare.
func TestNewPipelineAgentSkipsQuotaExhaustedLanesAndNamesEveryResetTime(t *testing.T) {
	now := time.Now()
	store := lanehealth.NewStore(
		filepath.Join(t.TempDir(), "lane-health.json"),
		func() time.Time { return now },
	)
	codexUntil := now.Add(72 * time.Hour)
	claudeUntil := now.Add(4 * time.Hour)
	for _, outage := range []lanehealth.Outage{
		{Lane: string(types.AgentCodex), Until: codexUntil, Reason: "You've hit your usage limit"},
		{Lane: string(types.AgentClaude), Until: claudeUntil, Reason: "You've hit your session limit"},
	} {
		if err := store.Mark(outage); err != nil {
			t.Fatalf("Mark %s: %v", outage.Lane, err)
		}
	}

	// Point both lanes at binaries that cannot exist, so if the cooldown were
	// NOT consumed the failure would be a spawn error naming those paths.
	missing := filepath.Join(t.TempDir(), "definitely-not-installed")
	cfg := &config.Config{
		Agent:  types.AgentCodex,
		Agents: []types.AgentName{types.AgentCodex, types.AgentClaude},
		AgentPathOverride: map[string]string{
			string(types.AgentCodex):  missing,
			string(types.AgentClaude): missing,
		},
	}
	ag, err := newPipelineAgent(context.Background(), cfg, fakeLookPath, store)
	if err != nil {
		t.Fatalf("newPipelineAgent: %v", err)
	}
	defer func() { _ = ag.Close() }()

	_, runErr := ag.Run(context.Background(), agent.RunOpts{Prompt: "x", CWD: t.TempDir()})
	if runErr == nil {
		t.Fatalf("expected the run to fail with every lane exhausted")
	}
	msg := runErr.Error()
	if !strings.Contains(msg, "every configured agent lane is quota-exhausted") {
		t.Fatalf("error %q must report that no lane can run", msg)
	}
	for _, want := range []string{
		"codex until " + codexUntil.Local().Format("2006-01-02 15:04 MST"),
		"claude until " + claudeUntil.Local().Format("2006-01-02 15:04 MST"),
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q must contain %q", msg, want)
		}
	}
	if strings.Contains(msg, missing) {
		t.Fatalf("no marked lane may be spawned, but the error names the binary: %q", msg)
	}
}

func TestNewPipelineAgentToleratesNoLaneHealthStore(t *testing.T) {
	cfg := &config.Config{Agent: types.AgentCodex}
	ag, err := newPipelineAgent(context.Background(), cfg, fakeLookPath, nil)
	if err != nil {
		t.Fatalf("newPipelineAgent without a store: %v", err)
	}
	_ = ag.Close()
}
