package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// The document step runs after review, is the last step to rewrite file
// content before push, and is never itself re-reviewed. It receives the user
// intent with the same AUTHORITATIVE framing review gets (intent_prompt.go),
// but not intentConformanceReviewClause, which is the only directive telling a
// step to park an intent contradiction rather than resolve it. Combined with
// its own scope discipline ("plus direct contradictions that analysis
// reveals"), that let a document pass delete what four review ask-user rounds
// had settled on, in order to re-align the prose with the stale --intent.
//
// Observed 2026-07-29, Blakeolson21/Remote-Comp run 01KYREV53AQ2AFMNEZBF08YJ9X:
// review commit efd09d44 recorded a GitHub concurrency eviction hazard;
// document commit fd5c65f0 removed it and reported zero unresolved items.
func TestDocumentPrompt_ForbidsRewritingCommittedContentToMatchIntent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = "Place the Family harness in the billing-readiness-web concurrency group."
	sctx.IntentSource = "agent"

	prompt := (&DocumentStep{}).buildPrompt(sctx, baseSHA, "none", true)

	if !strings.Contains(prompt, "Intent is context here, not authority to rewrite (required)") {
		t.Error("document prompt must carry the intent supersession clause")
	}
	if !strings.Contains(prompt, "MUST NOT delete, revert, weaken, or reword committed content") {
		t.Error("document prompt must forbid rewriting committed content to match the intent")
	}
	if !strings.Contains(prompt, "ask-user") {
		t.Error("document prompt must route an intent contradiction to an ask-user finding")
	}
}

// The clause is inert without an intent, so runs with no --intent and no
// inferred summary keep their existing prompt byte-for-byte.
func TestIntentSupersessionClause_EmptyWithoutIntent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})

	sctx.UserIntent = ""
	if got := intentSupersessionClause(sctx); got != "" {
		t.Errorf("clause must be empty without an intent, got %q", got)
	}

	// An inferred hint still gets the clause: the document step must not
	// litigate the intent regardless of how the intent was obtained.
	sctx.UserIntent = "some inferred goal"
	sctx.IntentSource = "claude"
	if got := intentSupersessionClause(sctx); got == "" {
		t.Error("clause must apply to inferred intent too")
	}
}
