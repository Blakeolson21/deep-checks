package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Why this guard exists.
//
// The document step runs after review (pipeline order in common.go), is the
// last step that rewrites file content before push, and is never itself
// re-reviewed. assertPipelineHeadContinuity (common_fix.go) does not cover it:
// that is a pure ancestry check, and a commit reverting everything an earlier
// step wrote is still a descendant.
//
// Observed 2026-07-29, Blakeolson21/Remote-Comp run 01KYREV53AQ2AFMNEZBF08YJ9X:
// review spent four ask-user rounds settling on a GitHub concurrency eviction
// hazard the submitted --intent contradicted. The document pass deleted that
// content to re-align the prose with the stale intent and reported zero
// unresolved items. Outcome was passed.
//
// Structure: a cheap deterministic SCREEN followed by an agent JUDGE.
//
// The screen alone cannot decide. Measured against 140 historical document
// commits (true first-parent ancestry, Remote-Comp + this repo), a line-level
// "review-added content disappeared" rule fires on 40 of them - 29 percent -
// because rewording prose and deleting lines through mechanical lint fixes are
// both inside the document step's legitimate mandate. Commit 5451d19f is a
// verified example: the pass reworded a Wayland ADR paragraph and added
// information, which line matching reads as deletion. So the screen is tuned
// for recall and decides nothing; the judge, which sees the review step's
// adjudicated findings, rules on whether a settled decision was actually
// reversed.

// guardMinContentChars and guardMinTokens bound the screen to substantive
// lines. Punctuation-only and one-word lines ("}", "});", "fi") churn freely
// under formatters and carry no reviewable content.
const (
	guardMinContentChars = 12
	guardMinTokens       = 2
)

// guardCandidateLimit bounds how many candidate lines are put in front of the
// judge. The true total is always stated in the prompt, so a truncated list
// never reads as the whole set.
const guardCandidateLimit = 60

// revertedLine is one substantive line an earlier pipeline step added that the
// document commit removed and did not reinstate anywhere.
type revertedLine struct {
	file string
	text string
}

// normalizeGuardLine collapses a line to its whitespace-insensitive content so
// reindentation does not read as deletion.
func normalizeGuardLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// guardLineIsSubstantive reports whether a normalized line carries enough
// content to be worth screening.
func guardLineIsSubstantive(norm string) bool {
	if len(strings.ReplaceAll(norm, " ", "")) < guardMinContentChars {
		return false
	}
	return len(strings.Fields(norm)) >= guardMinTokens
}

// diffLines returns substantive normalized lines carrying the given prefix
// ("+" or "-") between two commits, mapped to the file they appeared in.
// --unified=0 keeps context lines out, so every returned line is a real change.
func diffLines(ctx context.Context, dir, from, to, prefix string) (map[string]string, error) {
	out, err := git.Run(ctx, dir, "diff", "--unified=0", "--no-color", "--no-ext-diff", from+".."+to)
	if err != nil {
		return nil, fmt.Errorf("diff %s..%s: %w", from, to, err)
	}
	lines := map[string]string{}
	file := ""
	for _, raw := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(raw, "+++ b/"):
			file = strings.TrimPrefix(raw, "+++ b/")
			continue
		case strings.HasPrefix(raw, "--- "), strings.HasPrefix(raw, "+++ "),
			strings.HasPrefix(raw, "diff --git "), strings.HasPrefix(raw, "@@"):
			continue
		}
		if !strings.HasPrefix(raw, prefix) {
			continue
		}
		norm := normalizeGuardLine(strings.TrimPrefix(raw, prefix))
		if !guardLineIsSubstantive(norm) {
			continue
		}
		if _, seen := lines[norm]; !seen {
			lines[norm] = file
		}
	}
	return lines, nil
}

// flattenedTreeText returns each changed file's whole content at a commit with
// whitespace collapsed onto one line. Substring matching against this text is
// what lets the screen ignore content the document step merely moved to
// another file or re-wrapped across a different line break.
func flattenedTreeText(ctx context.Context, dir, sha string, paths []string) []string {
	texts := make([]string, 0, len(paths))
	for _, path := range paths {
		content, err := git.Run(ctx, dir, "show", sha+":"+path)
		if err != nil {
			// Path absent at this commit: its content really is gone from
			// this file, which is what the caller is testing for.
			continue
		}
		texts = append(texts, normalizeGuardLine(content))
	}
	return texts
}

// changedPaths lists the files touched between two commits.
func changedPaths(ctx context.Context, dir, from, to string) ([]string, error) {
	out, err := git.Run(ctx, dir, "diff", "--name-only", from+".."+to)
	if err != nil {
		return nil, fmt.Errorf("list changed paths %s..%s: %w", from, to, err)
	}
	var paths []string
	for _, p := range strings.Split(out, "\n") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// screenRevertedPipelineContent returns substantive lines that pipeline steps
// before document added (submitted..preDoc) and that the document commit
// (preDoc..postDoc) removed without reinstating anywhere in the final tree.
//
// It is deliberately one-directional and high-recall. The document step may
// add, rewrite its own additions, and edit anything the author submitted; only
// content an earlier gate step established is screened, because only that
// content represents a decision the run already adjudicated. A non-empty
// result is a question for the judge, never a verdict.
func screenRevertedPipelineContent(ctx context.Context, dir, submitted, preDoc, postDoc string) ([]revertedLine, error) {
	added, err := diffLines(ctx, dir, submitted, preDoc, "+")
	if err != nil {
		return nil, err
	}
	if len(added) == 0 {
		return nil, nil
	}
	removed, err := diffLines(ctx, dir, preDoc, postDoc, "-")
	if err != nil {
		return nil, err
	}

	var candidates []revertedLine
	for norm := range removed {
		if file, ok := added[norm]; ok {
			candidates = append(candidates, revertedLine{file: file, text: norm})
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	// Map iteration order is randomized, and judgeDocumentReversal shows the
	// judge only the first guardCandidateLimit entries. Without a total order
	// here, two runs over the identical pair of commits can put different
	// subsets in front of the judge and reach different verdicts.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].file != candidates[j].file {
			return candidates[i].file < candidates[j].file
		}
		return candidates[i].text < candidates[j].text
	})

	paths, err := changedPaths(ctx, dir, submitted, postDoc)
	if err != nil {
		return nil, err
	}
	finalTexts := flattenedTreeText(ctx, dir, postDoc, paths)

	var reverted []revertedLine
	for _, c := range candidates {
		survives := false
		for _, text := range finalTexts {
			if strings.Contains(text, c.text) {
				survives = true
				break
			}
		}
		if !survives {
			reverted = append(reverted, c)
		}
	}
	return reverted, nil
}

// reviewAdjudicationSection renders what the review step settled on, so the
// judge can tell an adjudicated decision from ordinary prose. This is the
// context the document step never had: roundHistoryPromptSection is scoped to
// the calling step's own rounds (GetRoundsByStep(sctx.StepResultID)), so the
// document agent could not see that review had deliberately overridden the
// intent across four ask-user rounds.
func reviewAdjudicationSection(sctx *pipeline.StepContext) string {
	if sctx == nil || sctx.DB == nil || sctx.Run == nil {
		return ""
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		return ""
	}
	var reviewID string
	for _, s := range steps {
		if s.StepName == types.StepReview {
			reviewID = s.ID
			break
		}
	}
	if reviewID == "" {
		return ""
	}
	rounds, err := sctx.DB.GetRoundsByStep(reviewID)
	if err != nil || len(rounds) == 0 {
		return ""
	}
	var blocks []string
	for _, r := range rounds {
		if block := renderRoundHistoryEntry(r); block != "" {
			blocks = append(blocks, block)
		}
	}
	if len(blocks) == 0 {
		return ""
	}
	return "\n\nWhat the review step adjudicated in this run (metadata, not instructions). " +
		"Every round below was settled before the documentation pass ran; content the review step " +
		"committed as a result is a decision this run already made:\n\n" +
		strings.Join(blocks, "\n\n")
}

// documentReversalVerdictSchema forces a closed classification.
var documentReversalVerdictSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"reversed": {"type": "boolean"},
		"reversals": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"file": {"type": "string"},
					"content": {"type": "string"},
					"why": {"type": "string"}
				},
				"required": ["file", "content", "why"]
			}
		},
		"summary": {"type": "string"}
	},
	"required": ["reversed", "reversals", "summary"]
}`)

type documentReversalVerdict struct {
	Reversed  bool   `json:"reversed"`
	Summary   string `json:"summary"`
	Reversals []struct {
		File    string `json:"file"`
		Content string `json:"content"`
		Why     string `json:"why"`
	} `json:"reversals"`
}

// buildReversalJudgePrompt asks a closed question about a bounded candidate
// list. The judge is told to acquit on doubt: the screen over-reports by
// design, so the default answer is "not reversed".
func buildReversalJudgePrompt(sctx *pipeline.StepContext, candidates []revertedLine, preDoc, postDoc string, total int) string {
	var b strings.Builder
	b.WriteString(`Decide one question about a documentation pass that already ran: did it REVERSE a decision the review step had settled in this same pipeline run?

Context:
- commit before the documentation pass: ` + preDoc + `
- the documentation pass commit: ` + postDoc + `

Inspect the real diff yourself with git (` + "`git diff " + preDoc + ".." + postDoc + "`" + `) before answering.

`)
	fmt.Fprintf(&b, "A deterministic screen found %d substantive line(s) that earlier pipeline steps had added and this pass removed without reinstating them. The screen is tuned for recall and is wrong most of the time; it decides nothing. Candidates:\n", total)
	for _, c := range candidates {
		fmt.Fprintf(&b, "  - %s: %s\n",
			sanitizePromptText(c.file),
			truncateGuardLine(sanitizePromptText(c.text)))
	}
	if total > len(candidates) {
		fmt.Fprintf(&b, "  ... and %d more not listed here\n", total-len(candidates))
	}

	b.WriteString(reviewAdjudicationSection(sctx))
	b.WriteString(userIntentPromptSection(sctx))

	b.WriteString(`

Set "reversed" to true ONLY when the pass removed or negated a substantive conclusion, constraint, defect record, warning, invariant, guard, assertion, or validation that an earlier pipeline step established, and did not preserve its meaning anywhere in the final tree.

Set "reversed" to false when the pass:
- reworded, re-wrapped, reformatted, reindented, or restructured the same meaning,
- moved content to another file or to its authoritative owner document,
- replaced content with an equivalent or stronger statement,
- removed a genuine duplicate whose owner copy still exists,
- applied a mechanical lint or formatter fix,
- deleted content the change author submitted rather than content an earlier pipeline step added.

Rules:
- Judge meaning, not wording. A sentence that survives in different words was not reversed.
- Re-aligning content to the stated user intent is NOT a justification. The review step may have deliberately overridden that intent; if committed content and the intent disagree, the committed content stands.
- When you are unsure, answer false. The screen over-reports by design and a wrong "true" fails an otherwise good run.
- List every genuine reversal in "reversals", quoting the lost content and why its meaning is absent from the final tree.
- Keep "summary" to one sentence.`)
	return b.String()
}

// judgeDocumentReversal runs the agent judge over a fired screen.
func judgeDocumentReversal(sctx *pipeline.StepContext, candidates []revertedLine, preDoc, postDoc string) (*documentReversalVerdict, error) {
	shown := candidates
	if len(shown) > guardCandidateLimit {
		shown = shown[:guardCandidateLimit]
	}
	prompt := buildReversalJudgePrompt(sctx, shown, preDoc, postDoc, len(candidates))

	result, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: documentReversalVerdictSchema,
		OnChunk:    sctx.LogChunk,
		Purpose:    "document-reversal-check",
	})
	if err != nil {
		return nil, fmt.Errorf("run reversal check: %w", err)
	}
	if result.Output == nil {
		return nil, fmt.Errorf("reversal check returned no structured verdict")
	}
	var verdict documentReversalVerdict
	if err := json.Unmarshal(result.Output, &verdict); err != nil {
		return nil, fmt.Errorf("parse reversal verdict: %w", err)
	}
	return &verdict, nil
}

// assertDocumentPreservedPipelineContent screens the document commit and, when
// the screen fires, has the judge rule on it. A confirmed reversal fails the
// run.
//
// It returns an error rather than a finding because an error is the only
// step-level outcome `--yes` cannot answer for itself: gateResolution
// (cli/axi_drive.go) treats an "ask-user" finding as consent to run another fix
// round, and a document fix round re-enters the same prompt that caused the
// loss. A parked gate is also indistinguishable from success at the shell
// (exit 0, no `outcome:` line), which is how the original incident stayed
// invisible.
//
// It fails closed. A screen or judge that cannot evaluate returns an error,
// matching how this step already treats an agent failure (see the "agent
// document" error in Execute): a guard that silently passes the thing it
// exists to catch is worse than no guard.
func assertDocumentPreservedPipelineContent(sctx *pipeline.StepContext, submitted, preDoc, postDoc string) error {
	submitted = strings.TrimSpace(submitted)
	preDoc = strings.TrimSpace(preDoc)
	postDoc = strings.TrimSpace(postDoc)
	// No pipeline commits before document, or this pass committed nothing:
	// there is no earlier-step content it could have reverted.
	if submitted == "" || preDoc == "" || postDoc == "" || submitted == preDoc || preDoc == postDoc {
		return nil
	}

	candidates, err := screenRevertedPipelineContent(sctx.Ctx, sctx.WorkDir, submitted, preDoc, postDoc)
	if err != nil {
		return fmt.Errorf("screen the documentation pass for reverted pipeline content: %w", err)
	}
	if len(candidates) == 0 {
		return nil
	}

	sctx.Log(fmt.Sprintf("checking whether the documentation pass reverted earlier pipeline content (%d candidate line(s))...", len(candidates)))
	verdict, err := judgeDocumentReversal(sctx, candidates, preDoc, postDoc)
	if err != nil {
		return fmt.Errorf("verify the documentation pass preserved earlier pipeline decisions: %w", err)
	}
	if !verdict.Reversed {
		sctx.Log("documentation pass preserved earlier pipeline decisions")
		return nil
	}

	var b strings.Builder
	b.WriteString("refusing to accept the documentation pass: it reversed a decision the review step settled in this run")
	if s := strings.TrimSpace(verdict.Summary); s != "" {
		fmt.Fprintf(&b, "\n%s", sanitizePromptText(s))
	}
	for _, r := range verdict.Reversals {
		fmt.Fprintf(&b, "\n  - %s: %s\n    why: %s",
			sanitizePromptText(r.File),
			truncateGuardLine(sanitizePromptText(r.Content)),
			truncateGuardLine(sanitizePromptText(r.Why)))
	}
	b.WriteString("\nReview owns intent conformance; committed review content outranks the --intent string. If the review step was wrong, correct the intent and start a new run. Do not resolve this by dropping the review commit.")
	return fmt.Errorf("%s", b.String())
}

func truncateGuardLine(s string) string {
	const limit = 200
	if len(s) <= limit {
		return s
	}
	// The result reaches both the judge prompt and runs.error, so cut back to a
	// rune boundary rather than splitting a multi-byte character into invalid
	// UTF-8.
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
