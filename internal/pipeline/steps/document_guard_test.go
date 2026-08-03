package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// reviewEstablished stands in for content a review round settled on: the
// concurrency-eviction fact run 01KYREV53AQ2AFMNEZBF08YJ9X lost.
const reviewEstablished = "A GitHub concurrency group retains only one pending member, so cancel-in-progress false protects only the running job."

// docGuardHarness drives DocumentStep against a tree that already carries a
// review commit, routing the document pass and the reversal judge to separate
// callbacks so each can be asserted independently.
type docGuardHarness struct {
	dir         string
	sctx        *pipeline.StepContext
	judgeCalls  int
	judgePrompt string
}

func newDocGuardHarness(t *testing.T, docPass func(dir string), judge func() (*agent.Result, error)) *docGuardHarness {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	h := &docGuardHarness{dir: dir}
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if opts.Purpose == "document-reversal-check" {
				h.judgeCalls++
				h.judgePrompt = opts.Prompt
				return judge()
			}
			docPass(dir)
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"align documentation"}`)}, nil
		},
	}
	h.sctx = newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	return h
}

// stageReviewCommit reproduces the state the document step inherits: the run
// was submitted at submittedSHA, then an earlier gate step committed on top.
func (h *docGuardHarness) stageReviewCommit(t *testing.T, submittedSHA, path, body string) {
	t.Helper()
	full := filepath.Join(h.dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, h.dir, "add", "-A")
	gitCmd(t, h.dir, "commit", "-m", "no-mistakes(review): record the concurrency defect")
	head := strings.TrimSpace(gitCmd(t, h.dir, "rev-parse", "HEAD"))

	h.sctx.Run.SubmittedHeadSHA = &submittedSHA
	h.sctx.Run.HeadSHA = head
	if err := h.sctx.DB.UpdateRunHeadSHA(h.sctx.Run.ID, head); err != nil {
		t.Fatal(err)
	}
}

func verdictResult(reversed bool, file, content, why string) func() (*agent.Result, error) {
	return func() (*agent.Result, error) {
		v := map[string]any{
			"reversed":  reversed,
			"summary":   "checked the pass",
			"reversals": []any{},
		}
		if reversed {
			v["reversals"] = []any{map[string]string{"file": file, "content": content, "why": why}}
		}
		raw, _ := json.Marshal(v)
		return &agent.Result{Output: raw}, nil
	}
}

// A confirmed reversal must fail the run. An ask-user finding would not do:
// `--yes` funds another fix round, and a parked gate exits 0 with no
// `outcome:` line.
func TestDocumentStep_FailsWhenJudgeConfirmsReversal(t *testing.T) {
	t.Parallel()
	h := newDocGuardHarness(t,
		func(dir string) {
			os.WriteFile(filepath.Join(dir, "docs/NOTES.md"),
				[]byte("# Notes\n\nUse the shared concurrency group; it serializes the lanes.\n"), 0o644)
		},
		verdictResult(true, "docs/NOTES.md", reviewEstablished, "the eviction hazard is absent from the final tree"),
	)
	h.stageReviewCommit(t, strings.TrimSpace(gitCmd(t, h.dir, "rev-parse", "HEAD")), "docs/NOTES.md", "# Notes\n\n"+reviewEstablished+"\n")

	_, err := (&DocumentStep{}).Execute(h.sctx)
	if err == nil {
		t.Fatal("expected the run to fail when the judge confirmed a reversal")
	}
	if h.judgeCalls != 1 {
		t.Errorf("expected the judge to run once, got %d", h.judgeCalls)
	}
	if !strings.Contains(err.Error(), "docs/NOTES.md") {
		t.Errorf("error should name the file: %v", err)
	}
	if !strings.Contains(err.Error(), "eviction hazard is absent") {
		t.Errorf("error should carry the judge's reasoning: %v", err)
	}
}

// The screen over-reports by design: rewording prose and mechanical lint fixes
// both delete lines legitimately. An acquittal must let the run continue.
func TestDocumentStep_PassesWhenJudgeAcquitsReword(t *testing.T) {
	t.Parallel()
	h := newDocGuardHarness(t,
		func(dir string) {
			os.WriteFile(filepath.Join(dir, "docs/NOTES.md"),
				[]byte("# Notes\n\nOnly one member of a GitHub concurrency group may be pending, so a false cancel-in-progress guards the running job alone.\n"), 0o644)
		},
		verdictResult(false, "", "", ""),
	)
	h.stageReviewCommit(t, strings.TrimSpace(gitCmd(t, h.dir, "rev-parse", "HEAD")), "docs/NOTES.md", "# Notes\n\n"+reviewEstablished+"\n")

	if _, err := (&DocumentStep{}).Execute(h.sctx); err != nil {
		t.Fatalf("an acquitted reword must not fail the run: %v", err)
	}
	if h.judgeCalls != 1 {
		t.Errorf("expected the judge to run once, got %d", h.judgeCalls)
	}
}

// Relocating content to its owner document is what the placement policy asks
// for. The screen must absorb it so the judge is never paid for.
func TestDocumentStep_ScreenAbsorbsRelocation(t *testing.T) {
	t.Parallel()
	h := newDocGuardHarness(t,
		func(dir string) {
			os.WriteFile(filepath.Join(dir, "docs/NOTES.md"), []byte("# Notes\n\nSee README.md.\n"), 0o644)
			os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Project\n\n"+reviewEstablished+"\n"), 0o644)
		},
		func() (*agent.Result, error) { t.Fatal("judge must not run"); return nil, nil },
	)
	h.stageReviewCommit(t, strings.TrimSpace(gitCmd(t, h.dir, "rev-parse", "HEAD")), "docs/NOTES.md", "# Notes\n\n"+reviewEstablished+"\n")

	if _, err := (&DocumentStep{}).Execute(h.sctx); err != nil {
		t.Fatalf("relocation must not fail the run: %v", err)
	}
	if h.judgeCalls != 0 {
		t.Errorf("screen should have absorbed relocation, judge ran %d time(s)", h.judgeCalls)
	}
}

// Re-wrapping and re-indenting changes every line but loses nothing.
func TestDocumentStep_ScreenAbsorbsReflow(t *testing.T) {
	t.Parallel()
	reflowed := "# Notes\n\n  A GitHub concurrency group retains only one\n  pending member, so cancel-in-progress false\n  protects only the running job.\n"
	h := newDocGuardHarness(t,
		func(dir string) { os.WriteFile(filepath.Join(dir, "docs/NOTES.md"), []byte(reflowed), 0o644) },
		func() (*agent.Result, error) { t.Fatal("judge must not run"); return nil, nil },
	)
	h.stageReviewCommit(t, strings.TrimSpace(gitCmd(t, h.dir, "rev-parse", "HEAD")), "docs/NOTES.md", "# Notes\n\n"+reviewEstablished+"\n")

	if _, err := (&DocumentStep{}).Execute(h.sctx); err != nil {
		t.Fatalf("reflow must not fail the run: %v", err)
	}
	if h.judgeCalls != 0 {
		t.Errorf("screen should have absorbed reflow, judge ran %d time(s)", h.judgeCalls)
	}
}

// The guard is one-directional: author-submitted content is the document
// step's to correct. Only earlier gate steps' content is protected.
func TestDocumentStep_IgnoresEditsToAuthorSubmittedContent(t *testing.T) {
	t.Parallel()
	h := newDocGuardHarness(t,
		func(dir string) {
			os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Rewritten by the document pass\n"), 0o644)
		},
		func() (*agent.Result, error) { t.Fatal("judge must not run"); return nil, nil },
	)
	h.stageReviewCommit(t, strings.TrimSpace(gitCmd(t, h.dir, "rev-parse", "HEAD")), "docs/NOTES.md", "# Notes\n\n"+reviewEstablished+"\n")

	if _, err := (&DocumentStep{}).Execute(h.sctx); err != nil {
		t.Fatalf("editing author content must not fail the run: %v", err)
	}
	if h.judgeCalls != 0 {
		t.Errorf("judge ran %d time(s) for author-content edits", h.judgeCalls)
	}
}

// A judge that cannot answer must not let the pass through: a guard that
// silently passes the thing it exists to catch is worse than no guard.
func TestDocumentStep_FailsClosedWhenJudgeCannotAnswer(t *testing.T) {
	t.Parallel()
	h := newDocGuardHarness(t,
		func(dir string) {
			os.WriteFile(filepath.Join(dir, "docs/NOTES.md"), []byte("# Notes\n\nreplaced entirely by other prose here.\n"), 0o644)
		},
		func() (*agent.Result, error) { return nil, fmt.Errorf("agent transport failed") },
	)
	h.stageReviewCommit(t, strings.TrimSpace(gitCmd(t, h.dir, "rev-parse", "HEAD")), "docs/NOTES.md", "# Notes\n\n"+reviewEstablished+"\n")

	_, err := (&DocumentStep{}).Execute(h.sctx)
	if err == nil {
		t.Fatal("an unanswerable judge must fail the run, not pass it")
	}
	if !strings.Contains(err.Error(), "preserved earlier pipeline decisions") {
		t.Errorf("error should say the check could not be completed: %v", err)
	}
}

// A verdict the schema shape does not satisfy is also unanswerable.
func TestDocumentStep_FailsClosedOnUnparsableVerdict(t *testing.T) {
	t.Parallel()
	h := newDocGuardHarness(t,
		func(dir string) {
			os.WriteFile(filepath.Join(dir, "docs/NOTES.md"), []byte("# Notes\n\nreplaced entirely by other prose here.\n"), 0o644)
		},
		func() (*agent.Result, error) { return &agent.Result{Output: nil, Text: "sure thing"}, nil },
	)
	h.stageReviewCommit(t, strings.TrimSpace(gitCmd(t, h.dir, "rev-parse", "HEAD")), "docs/NOTES.md", "# Notes\n\n"+reviewEstablished+"\n")

	if _, err := (&DocumentStep{}).Execute(h.sctx); err == nil {
		t.Fatal("a verdict with no structured output must fail the run")
	}
}

// With no earlier pipeline commit there is nothing to protect, so neither the
// screen nor the judge should cost anything.
func TestDocumentStep_GuardInertWithoutEarlierPipelineCommits(t *testing.T) {
	t.Parallel()
	h := newDocGuardHarness(t,
		func(dir string) {
			os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Replaced wholesale\n"), 0o644)
		},
		func() (*agent.Result, error) { t.Fatal("judge must not run"); return nil, nil },
	)

	if _, err := (&DocumentStep{}).Execute(h.sctx); err != nil {
		t.Fatalf("guard must be inert without an earlier pipeline commit: %v", err)
	}
	if h.judgeCalls != 0 {
		t.Errorf("judge ran %d time(s) with no earlier pipeline commit", h.judgeCalls)
	}
}

// The judge must see what review settled and be told to acquit on doubt.
// Without the review history it cannot tell an adjudicated decision from
// ordinary prose - which is the context the document step itself never had.
func TestDocumentStep_JudgePromptCarriesReviewAdjudication(t *testing.T) {
	t.Parallel()
	h := newDocGuardHarness(t,
		func(dir string) {
			os.WriteFile(filepath.Join(dir, "docs/NOTES.md"), []byte("# Notes\n\nreplaced entirely by other prose here.\n"), 0o644)
		},
		verdictResult(false, "", "", ""),
	)
	h.stageReviewCommit(t, strings.TrimSpace(gitCmd(t, h.dir, "rev-parse", "HEAD")), "docs/NOTES.md", "# Notes\n\n"+reviewEstablished+"\n")

	// A settled review round for this run.
	reviewStep, err := h.sctx.DB.InsertStepResult(h.sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"review-1","severity":"error","description":"Record the concurrency eviction hazard","action":"ask-user"}],"summary":"concurrency"}`
	if _, err := h.sctx.DB.InsertStepRound(reviewStep.ID, 1, "initial", &findings, nil, 0); err != nil {
		t.Fatal(err)
	}
	h.sctx.UserIntent = "Join the Family harness to the billing-readiness-web group."
	h.sctx.IntentSource = "agent"

	if _, err := (&DocumentStep{}).Execute(h.sctx); err != nil {
		t.Fatalf("unexpected failure: %v", err)
	}
	if h.judgeCalls != 1 {
		t.Fatalf("expected the judge to run once, got %d", h.judgeCalls)
	}
	for _, want := range []string{
		"What the review step adjudicated in this run",
		"Record the concurrency eviction hazard",
		"When you are unsure, answer false",
		"Re-aligning content to the stated user intent is NOT a justification",
		reviewEstablished,
	} {
		if !strings.Contains(h.judgePrompt, want) {
			t.Errorf("judge prompt missing %q", want)
		}
	}
}

func TestGuardLineIsSubstantive(t *testing.T) {
	t.Parallel()
	cases := []struct {
		line string
		want bool
	}{
		{"}", false},
		{"});", false},
		{"fi", false},
		{"import os", false},
		{"if (x == null) throw new Error();", true},
		{"cancel-in-progress protects only the running job", true},
	}
	for _, c := range cases {
		if got := guardLineIsSubstantive(normalizeGuardLine(c.line)); got != c.want {
			t.Errorf("guardLineIsSubstantive(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

// The screen collects candidates out of a map, and judgeDocumentReversal shows
// the judge only the first guardCandidateLimit of them. Without a total order,
// the same pair of commits can put a different subset in front of the judge on
// every run and reach a different verdict.
func TestScreenRevertedPipelineContent_CandidateOrderIsDeterministic(t *testing.T) {
	t.Parallel()
	dir, _, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	submitted := strings.TrimSpace(gitCmd(t, dir, "rev-parse", "HEAD"))

	// diffLines keys on normalized text, so each file needs its own distinct
	// lines to land more than guardCandidateLimit candidates across two files.
	for _, name := range []string{"docs/beta.md", "docs/alpha.md"} {
		var established []string
		for i := 0; i < 40; i++ {
			established = append(established, fmt.Sprintf("%s: the gate refuses request %d because its lease anchor never advanced.", name, i))
		}
		if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Join(established, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "no-mistakes(review): record the lease defect")
	preDoc := strings.TrimSpace(gitCmd(t, dir, "rev-parse", "HEAD"))

	for _, name := range []string{"docs/beta.md", "docs/alpha.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("placeholder documentation body\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "no-mistakes(document): rewrite the docs")
	postDoc := strings.TrimSpace(gitCmd(t, dir, "rev-parse", "HEAD"))

	first, err := screenRevertedPipelineContent(context.Background(), dir, submitted, preDoc, postDoc)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) < guardCandidateLimit {
		t.Fatalf("need more than guardCandidateLimit candidates to prove truncation is stable, got %d", len(first))
	}
	for i := 1; i < len(first); i++ {
		prev, cur := first[i-1], first[i]
		if prev.file > cur.file || (prev.file == cur.file && prev.text >= cur.text) {
			t.Fatalf("candidates are not totally ordered at %d: %+v then %+v", i, prev, cur)
		}
	}
	for attempt := 0; attempt < 5; attempt++ {
		again, err := screenRevertedPipelineContent(context.Background(), dir, submitted, preDoc, postDoc)
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != len(first) {
			t.Fatalf("attempt %d returned %d candidates, want %d", attempt, len(again), len(first))
		}
		for i := range again {
			if again[i] != first[i] {
				t.Fatalf("attempt %d differs at %d: %+v, want %+v", attempt, i, again[i], first[i])
			}
		}
	}
}

// The truncated line reaches both the judge prompt and runs.error, so a cut
// through a multi-byte rune would store invalid UTF-8 in both.
func TestTruncateGuardLine_CutsOnRuneBoundary(t *testing.T) {
	t.Parallel()
	for _, filler := range []string{"é", "字", "🚀"} {
		line := strings.Repeat(filler, 400)
		got := truncateGuardLine(line)
		if !utf8.ValidString(got) {
			t.Fatalf("truncateGuardLine(%q repeated) produced invalid UTF-8: %q", filler, got)
		}
		if !strings.HasSuffix(got, "...") {
			t.Fatalf("truncateGuardLine(%q repeated) did not truncate: %q", filler, got)
		}
		if len(got) > 203 {
			t.Fatalf("truncateGuardLine(%q repeated) returned %d bytes, want at most 203", filler, len(got))
		}
	}
	short := "already short enough to survive"
	if got := truncateGuardLine(short); got != short {
		t.Fatalf("truncateGuardLine(%q) = %q, want it unchanged", short, got)
	}
}
