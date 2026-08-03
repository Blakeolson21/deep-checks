package git

import (
	"fmt"
	"strings"
)

// BranchRef normalizes a recorded run branch to a full ref name. Runs store
// either shape, and every ref-advancing caller must agree on one.
func BranchRef(branch string) string {
	if !strings.HasPrefix(branch, "refs/") {
		return "refs/heads/" + branch
	}
	return branch
}

// RefRunner runs a Git command against an already-chosen repository. It exists
// so the adoption policy below has exactly one implementation while each caller
// keeps its own command environment (a pipeline step carries step-scoped PATH
// and credential environment; the executor does not).
type RefRunner func(args ...string) (string, error)

// AdoptBranchRef moves a run's branch ref in the gate onto a head that run
// produced, and is the single owner of that move for every writer of a run
// head: the pipeline worktree is detached, so a head recorded on the run but
// never adopted on a ref becomes unreachable when the worktree is removed and
// strands the branch in pipeline custody with no working recovery.
//
// The move is anchored the same way the force push is (see forcepush.go):
// refuse whenever it would drop a commit the ref already holds. The ref may
// legitimately be behind the recorded head (a writer that ran before the
// adoption existed), already at the new head, or exactly at the recorded head
// that a rebase is about to rewrite - none of those lose anything. What must
// never happen is a move away from a commit this run never saw: a second push
// landing on the branch mid-run moves the ref, and the rebase adoption rewrites
// it non-fast-forward by construction, so an unguarded write there destroys
// that commit outright and recreates the very gate/head mismatch the adoption
// exists to remove.
//
// The write itself compare-and-swaps against the value just read, so a push
// that lands inside the decision window fails the caller rather than racing it.
// When the ref does not resolve, the create is asserted rather than assumed:
// the empty old value makes Git itself refuse when the ref does exist, which
// covers both a branch created inside the decision window and a resolution
// failure that only looked like absence.
func AdoptBranchRef(run RefRunner, branch, newHeadSHA, recordedHeadSHA string) error {
	ref := BranchRef(branch)
	current, err := run("rev-parse", "--verify", "--quiet", ref+"^{commit}")
	current = strings.TrimSpace(current)
	if err != nil || current == "" {
		if _, createErr := run("update-ref", ref, newHeadSHA, ""); createErr != nil {
			return fmt.Errorf("create local branch ref %s at %s: %w", ref, newHeadSHA, createErr)
		}
		return nil
	}
	if current == newHeadSHA {
		return nil
	}
	recorded := strings.TrimSpace(recordedHeadSHA)
	if current != recorded && !refIsAncestor(run, current, newHeadSHA) {
		return fmt.Errorf("refusing to move branch ref %s from %s to %s: %s is neither the pipeline's recorded head %s nor an ancestor of the new head, so another push landed on this branch and its commit would be lost",
			ref, current, newHeadSHA, current, recorded)
	}
	if _, err := run("update-ref", ref, newHeadSHA, current); err != nil {
		return fmt.Errorf("update local branch ref %s to %s: %w", ref, newHeadSHA, err)
	}
	return nil
}

func refIsAncestor(run RefRunner, ancestor, descendant string) bool {
	if ancestor == "" || descendant == "" {
		return false
	}
	_, err := run("merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}
