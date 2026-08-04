package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// EmptyTreeSHA is the well-known SHA of an empty tree in git.
// Used as a base when there is no prior commit to diff against.
const EmptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// IsZeroSHA returns true if the SHA is the null/zero ref that git uses for
// new or deleted branches (40 zeros).
func IsZeroSHA(sha string) bool {
	return sha == "0000000000000000000000000000000000000000"
}

// Run executes a git command in the given directory and returns trimmed stdout.
// Returns an error that includes the command and stderr on failure.
//
// When dir is itself a bare repository (a gate repo), the repo is named
// explicitly via --git-dir instead of relying on cwd-based discovery, which
// safe.bareRepository=explicit forbids. Agent harnesses (e.g. Claude Code)
// and hardened CI inject that setting, so gate operations must never depend
// on discovering a bare repo from the working directory (issue #362).
func Run(ctx context.Context, dir string, args ...string) (string, error) {
	if isBareGitDir(dir) {
		return RunBare(ctx, dir, args...)
	}
	return runInDir(ctx, dir, args...)
}

// RunBare executes Git against exactly bareDir. Unlike Run, it never falls
// back to cwd-based repository discovery when bareDir is malformed. Gate
// recovery uses this after structural validation so an invalid directory under
// NM_HOME cannot discover or mutate an ancestor worktree.
func RunBare(ctx context.Context, bareDir string, args ...string) (string, error) {
	if bareDir == "" {
		return "", fmt.Errorf("bare git directory is empty")
	}
	return runInDir(ctx, bareDir, append([]string{"--git-dir=" + bareDir}, args...)...)
}

// A git invocation must always be bounded, even when the caller hands down a
// deadline-free context.
//
// Why: under `go test -race` on darwin, a fork child of the calling process can
// wedge inside the race runtime (__tsan::TraceSwitchPartImpl) before it ever
// reaches execve. fork clones only the calling thread, so that child has no
// working Go runtime - no scheduler, no sysmon, no timer thread - and therefore
// its own -test.timeout watchdog can never fire. Observed: three such children
// spinning on a full core each for 17+ hours against a 10m0s test timeout,
// after their parent's timeout panicked it and reparented them to init.
//
// What the ceiling reaches, and what it does not. It bounds every hang once the
// child is running: a git that never exits, a git that stops making progress, a
// cancelled git whose descendants linger. It does NOT reach the pre-execve
// wedge described above. exec.Cmd.Start consults the context exactly once, in a
// non-blocking select before the fork, and starts the goroutine that honors the
// deadline and the WaitDelay only after os.StartProcess has returned. A child
// that never reaches execve never closes its CLOEXEC status pipe, so the parent
// blocks in syscall.forkExec's read of that pipe - still inside Start, with
// c.Process nil and no cancellation watcher running - and no bound expressible
// here applies. That also matches the incident's shape better than a stuck Wait
// would: three simultaneous wedged children are three goroutines each stuck in
// their own Start. Start cannot be abandoned without leaking the pipe and the
// pid, so this limit is recorded rather than fixed; do not read the ceiling as
// having excluded that cause.
//
// For everything Start does return from, the bound cannot come from the child,
// so it comes from here. These are deliberately generous ceilings for a
// pathological case, not pacing: git plumbing in this codebase completes in
// seconds, and callers that need a tighter bound pass their own deadline
// (branchsync uses 15s for its fetch). They are vars so tests can shorten them.
var (
	defaultCommandTimeout  = 5 * time.Minute
	extendedCommandTimeout = 60 * time.Minute
)

// commandWaitDelay bounds cmd.Wait after the process has exited or Cancel has
// returned, so a surviving pipe holder - a credential helper, a hook that
// inherited stdout, or a fork child wedged before execve - cannot wedge Wait
// indefinitely. It costs nothing in the ordinary case, where git's exit closes
// the last pipe descriptor and Wait returns immediately.
//
// When the delay does expire, exec force-closes the parent's pipe descriptors,
// abandons anything the copying goroutines had not read yet, and reports
// exec.ErrWaitDelay even though git exited 0. It does not throw away what was
// already copied - Output still returns the buffer it filled - but this package
// returns ("", err) on any error, so a command that actually succeeded still
// reaches the caller as a failure. Callers here fail closed on that
// (`HasUncommittedChanges` failing reads as "not clean", which blocks a custody
// recovery), so the misreport is a correctness bug rather than a tolerable
// cost, and the delay must not be the thing that triggers it.
//
// Hence 60s rather than the 5s the shellenv helper uses for long-lived agent
// commands, and hence no attempt to pass the copied buffer through as success
// on ErrWaitDelay: the delay is also the only thing standing between a merely
// starved copying goroutine and a truncated read, so its expiry cannot be
// treated as proof that the output is complete. The delay has to clear any
// plausible scheduler starvation of that goroutine on a badly loaded host,
// which is the exact condition this whole fix concerns: the incident host sat
// at load 160. A legitimate pipe close after a short git command takes
// microseconds, so the headroom is free, and the pathological case it guards
// still ends in a bounded error instead of a permanent wedge. Kept a var so
// tests can shorten it.
var commandWaitDelay = 60 * time.Second

// networkSubcommands reach a remote and get the longer ceiling. "remote" and
// "submodule" are included because some of their forms are network operations;
// over-granting the ceiling is safe, under-granting breaks real work.
var networkSubcommands = map[string]struct{}{
	"clone":     {},
	"fetch":     {},
	"ls-remote": {},
	"pull":      {},
	"push":      {},
	"remote":    {},
	"submodule": {},
}

// treeSubcommands materialize or rewrite a working tree, so their cost scales
// with repository size and filesystem speed instead of finishing in seconds
// like the rest of this package's plumbing. `git worktree add --detach` builds
// every run's worktree from a deadline-free daemon context, and on a large
// repository - or on Windows, where each file creation is Defender-taxed by
// roughly the same factor the CI comment measures for process spawns - that
// checkout can legitimately outlast the plumbing ceiling. Killing it would fail
// the run outright, and the ceiling exists to bound a pathological wedge rather
// than to pace real work, so these share the network tier's headroom.
var treeSubcommands = map[string]struct{}{
	"add":       {},
	"apply":     {},
	"checkout":  {},
	"merge":     {},
	"read-tree": {},
	"rebase":    {},
	"reset":     {},
	"restore":   {},
	"stash":     {},
	"worktree":  {},
}

// hookSubcommands run repository-supplied hooks, so what governs their runtime
// is whatever the hook does rather than anything git does. The pipeline's
// commits pass no --no-verify and NonInteractiveEnv does not disable hooks, so
// a repository's own pre-commit hook - lint-staged, a typecheck, a test run -
// executes on every pipeline commit whenever core.hookspath resolves to the
// worktree's hooks instead of the gate's, which is the exact case
// IsolateHooksPath in hook.go exists to defend the gate against. A ceiling
// short enough to interrupt that would be pacing someone else's build.
var hookSubcommands = map[string]struct{}{
	"am":     {},
	"commit": {},
}

// extendedCeilingSubcommands collects the sets above under the one rule they
// share: the runtime is not git's own, so the plumbing ceiling would be pacing
// real work instead of bounding a wedge.
var extendedCeilingSubcommands = []map[string]struct{}{
	networkSubcommands,
	treeSubcommands,
	hookSubcommands,
}

// globalOptionsWithSeparateValue are the git global options whose value is its
// own argument, so that value must not be mistaken for the subcommand. The
// attached forms (--git-dir=<path>) need no entry, because they are a single
// argument that already starts with "-".
var globalOptionsWithSeparateValue = map[string]struct{}{
	"-C":          {},
	"-c":          {},
	"--git-dir":   {},
	"--work-tree": {},
	"--namespace": {},
}

// gitSubcommand returns the first non-flag argument, so neither a leading
// --git-dir= (added by RunBare) nor a `-C <dir>` pair hides the subcommand.
func gitSubcommand(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return a
		}
		if _, ok := globalOptionsWithSeparateValue[a]; ok {
			i++
		}
	}
	return ""
}

func commandTimeout(args []string) time.Duration {
	sub := gitSubcommand(args)
	for _, set := range extendedCeilingSubcommands {
		if _, ok := set[sub]; ok {
			return extendedCommandTimeout
		}
	}
	return defaultCommandTimeout
}

// newCommand builds every git subprocess this package launches. It is the
// single place the bound above is applied, because a bound that lives in only
// one helper is not a bound: FindGitRoot and FindMainRepoRoot are the first
// statements of branchsync's inspect and were themselves in the incident's
// stack, so a wedge there is the same permanent orphan the deadline exists to
// prevent. The returned cancel must be deferred by the caller.
//
// Cancelling the context kills git's own PID, which is exactly the process that
// wedges in the case above. A grandchild of git that outlives it is a different
// leak class, owned by the transitive process-tree reap in
// docs/superpowers/specs/2026-07-25-process-tree-reap-design.md; commandWaitDelay
// keeps such a survivor from wedging Wait here in the meantime.
func newCommand(ctx context.Context, args ...string) *boundedCommand {
	cancel := context.CancelFunc(func() {})
	var ceiling time.Duration
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		ceiling = commandTimeout(args)
		ctx, cancel = context.WithTimeout(ctx, ceiling)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.WaitDelay = commandWaitDelay
	winproc.Harden(cmd)
	return &boundedCommand{cmd: cmd, ctx: ctx, cancel: cancel, ceiling: ceiling}
}

// boundedCommand is a git subprocess carrying the bound this package supplied,
// so the constructor that applies the bound is also the one that explains it.
// ceiling is zero when the caller's own context already carried a deadline.
//
// The *exec.Cmd is held rather than embedded on purpose: embedding promotes
// Run, Output, and CombinedOutput, and a call site that reached for one of them
// would still get the ceiling and the WaitDelay but would silently skip
// explain, compiling cleanly and reinstating the undiagnosable failure this
// type exists to prevent. Keeping it unexported makes that a compile error.
type boundedCommand struct {
	cmd     *exec.Cmd
	ctx     context.Context
	cancel  context.CancelFunc
	ceiling time.Duration
}

func (c *boundedCommand) setDir(dir string) { c.cmd.Dir = dir }

func (c *boundedCommand) setEnv(env []string) { c.cmd.Env = env }

func (c *boundedCommand) close() { c.cancel() }

// explain names the bound that produced a failure, because both bounds
// otherwise surface as something the caller cannot act on.
//
// A ceiling expiry reaches the caller two ways. Before the fork it is
// context.DeadlineExceeded straight out of Start; after it, exec kills git and
// Wait prefers the process's own *exec.ExitError over the watcher's error, so
// the context error is dropped and "signal: killed" reads as if git died on its
// own. The ceiling branch therefore joins context.DeadlineExceeded back in, so
// both paths carry the same cause and unwrap the same way.
//
// An expired WaitDelay arrives as exec.ErrWaitDelay after git exited 0,
// carrying whatever exec had already copied but no guarantee that it is
// complete. Callers in this package fail closed on error, so both bounds need
// to say which one fired: the whole point of bounding these subprocesses is to
// turn a permanent unexplained wedge into a diagnosable error.
func (c *boundedCommand) explain(err error) error {
	if err == nil {
		return nil
	}
	if c.ceiling > 0 && errors.Is(c.ctx.Err(), context.DeadlineExceeded) {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("exceeded the %s internal/git ceiling: %w", c.ceiling, err)
		}
		return fmt.Errorf("exceeded the %s internal/git ceiling: %w: %w", c.ceiling, context.DeadlineExceeded, err)
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		return fmt.Errorf("git exited but a surviving child held its output pipe past the %s wait delay, so its output could not be confirmed complete: %w", commandWaitDelay, err)
	}
	return err
}

func (c *boundedCommand) run() error { return c.explain(c.cmd.Run()) }

func (c *boundedCommand) output() ([]byte, error) {
	out, err := c.cmd.Output()
	return out, c.explain(err)
}

func (c *boundedCommand) combinedOutput() ([]byte, error) {
	out, err := c.cmd.CombinedOutput()
	return out, c.explain(err)
}

// runInDir executes git and returns trimmed stdout.
func runInDir(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := newCommand(ctx, args...)
	defer cmd.close()
	cmd.setDir(dir)
	cmd.setEnv(NonInteractiveEnv(dir))
	out, err := cmd.output()
	if err != nil {
		stderr := ""
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("git %s: %w: %s", safeurl.RedactText(strings.Join(args, " ")), err, safeurl.RedactText(stderr))
	}
	return strings.TrimSpace(string(out)), nil
}

// ValidateBareRepository verifies both the filesystem shape and Git's own bare
// repository classification. The Git query is explicitly scoped with
// --git-dir, so validation itself cannot discover an ancestor repository.
func ValidateBareRepository(ctx context.Context, bareDir string) error {
	if !isBareGitDir(bareDir) {
		return fmt.Errorf("not a structurally valid bare repository: %s", bareDir)
	}
	out, err := RunBare(ctx, bareDir, "rev-parse", "--is-bare-repository")
	if err != nil {
		return fmt.Errorf("validate bare repository: %w", err)
	}
	if strings.TrimSpace(out) != "true" {
		return fmt.Errorf("repository is not bare: %s", bareDir)
	}
	return nil
}

// LooksLikeBareRepository performs the non-mutating structural half of bare
// repository validation. Call ValidateBareRepository before mutating an
// unstamped directory discovered from the filesystem.
func LooksLikeBareRepository(dir string) bool {
	return isBareGitDir(dir)
}

// isBareGitDir reports whether dir is itself a git directory (a bare repo),
// as opposed to a working tree or linked worktree, which carry a .git entry
// and keep using normal discovery. The check mirrors git's own git-dir
// heuristic: a HEAD file plus an objects directory.
func isBareGitDir(dir string) bool {
	if dir == "" {
		return false
	}
	root, err := os.Lstat(dir)
	if err != nil || !root.IsDir() || root.Mode()&os.ModeSymlink != 0 {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return false
	}
	if fi, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil || fi.IsDir() {
		return false
	}
	fi, err := os.Stat(filepath.Join(dir, "objects"))
	return err == nil && fi.IsDir()
}

// InitBare creates a new bare git repository at the given path.
func InitBare(ctx context.Context, path string) error {
	cmd := newCommand(ctx, "init", "--bare", path)
	defer cmd.close()
	out, err := cmd.combinedOutput()
	if err != nil {
		return fmt.Errorf("git init --bare: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// AddRemote adds a named remote to the repo at dir.
func AddRemote(ctx context.Context, dir, name, url string) error {
	_, err := Run(ctx, dir, "remote", "add", name, url)
	return err
}

// EnsureRemote sets the named remote to url, adding it when absent and
// updating its URL when it already exists. Idempotent, so it is safe to call
// when repairing or re-running an init.
func EnsureRemote(ctx context.Context, dir, name, url string) error {
	if _, err := GetRemoteURL(ctx, dir, name); err == nil {
		_, err := Run(ctx, dir, "remote", "set-url", name, url)
		return err
	}
	return AddRemote(ctx, dir, name, url)
}

// RemoveRemote removes a named remote from the repo at dir.
func RemoveRemote(ctx context.Context, dir, name string) error {
	_, err := Run(ctx, dir, "remote", "remove", name)
	return err
}

// GetRemoteURL returns the URL of a named remote.
func GetRemoteURL(ctx context.Context, dir, name string) (string, error) {
	return Run(ctx, dir, "remote", "get-url", name)
}

// GetConfiguredRemoteURL returns the literal remote URL from git config,
// without applying url.*.insteadOf rewrites.
func GetConfiguredRemoteURL(ctx context.Context, dir, name string) (string, error) {
	return Run(ctx, dir, "config", "--get", "remote."+name+".url")
}

// GetConfiguredRemoteURLs returns every literal URL configured for a remote.
// Callers that require an authoritative source can reject zero or multiple
// values rather than letting git silently select one.
func GetConfiguredRemoteURLs(ctx context.Context, dir, name string) ([]string, error) {
	out, err := Run(ctx, dir, "config", "--null", "--get-all", "remote."+name+".url")
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(out, "\x00"), "\x00"), nil
}

// HasRemote reports whether a remote named name is configured in the repo at
// dir, returning an error if the remote list cannot be read.
func HasRemote(ctx context.Context, dir, name string) (bool, error) {
	out, err := Run(ctx, dir, "remote")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == name {
			return true, nil
		}
	}
	return false, nil
}

// FindGitRoot walks up from path to find the git repository root.
// Resolves symlinks for consistency on macOS (e.g. /tmp -> /private/tmp).
func FindGitRoot(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	cmd := newCommand(context.Background(), "rev-parse", "--show-toplevel")
	defer cmd.close()
	cmd.setDir(abs)
	out, err := cmd.output()
	if err != nil {
		return "", fmt.Errorf("not a git repository: %s: %w", abs, err)
	}
	root := strings.TrimSpace(string(out))
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return root, nil
	}
	return resolved, nil
}

// FindMainRepoRoot returns the root of the main working tree for a git
// repository. Three layouts are supported:
//
//  1. A regular repository or a linked worktree: the git common dir is
//     <root>/.git, so the main working tree is filepath.Dir(commonDir).
//  2. An absorbed submodule (including nested .../modules/a/modules/b):
//     the git common dir lives under the superproject's .git/modules/...
//     and is detached from its working tree. Git writes core.worktree
//     when it absorbs a submodule, pointing at the working tree whose
//     remote.origin.url is the submodule's own origin (which is what
//     callers like init and eject need).
//  3. Exotic GIT_DIR layouts without a core.worktree: fall back to
//     `git rev-parse --show-toplevel` from the original path, the same
//     answer FindGitRoot returns.
//
// In every branch the returned path is run through filepath.EvalSymlinks
// when possible so callers can compare it against other symlink-resolved
// paths (notably on macOS, where /tmp and /private/tmp refer to the same
// directory). Symlink resolution failures fall back to the unresolved
// path, matching the historical behavior.
func FindMainRepoRoot(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}

	// Resolve the git common dir.
	commonDirCmd := newCommand(context.Background(), "rev-parse", "--git-common-dir")
	defer commonDirCmd.close()
	commonDirCmd.setDir(abs)
	commonDirOut, err := commonDirCmd.output()
	if err != nil {
		return "", fmt.Errorf("not a git repository: %s: %w", abs, err)
	}
	commonDir := strings.TrimSpace(string(commonDirOut))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(abs, commonDir)
	}

	// Branch 1: regular repo or linked worktree. Linked worktrees share
	// the main repo's <root>/.git, so the common dir's basename is still
	// ".git" and its parent is the main working tree.
	if filepath.Base(commonDir) == ".git" {
		return resolveMainRoot(filepath.Dir(commonDir))
	}

	// Branch 2: detached git dir (absorbed submodule). Ask the git dir
	// itself for its core.worktree, which git writes when it absorbs a
	// submodule's git dir. The value is typically relative (e.g.
	// "../../../sub"); resolve it against the common dir.
	worktreeCmd := newCommand(context.Background(), "--git-dir", commonDir, "config", "--get", "core.worktree")
	defer worktreeCmd.close()
	if worktreeOut, err := worktreeCmd.output(); err == nil {
		worktree := strings.TrimSpace(string(worktreeOut))
		if worktree != "" {
			if !filepath.IsAbs(worktree) {
				worktree = filepath.Join(commonDir, worktree)
			}
			return resolveMainRoot(worktree)
		}
	}

	// Branch 3: exotic GIT_DIR without a usable core.worktree. Defer to
	// `git rev-parse --show-toplevel` from the original path.
	topCmd := newCommand(context.Background(), "rev-parse", "--show-toplevel")
	defer topCmd.close()
	topCmd.setDir(abs)
	topOut, err := topCmd.output()
	if err != nil {
		return "", fmt.Errorf("not a git repository: %s: %w", abs, err)
	}
	return resolveMainRoot(strings.TrimSpace(string(topOut)))
}

// resolveMainRoot applies filepath.EvalSymlinks to path, falling back to
// the unresolved path when symlink resolution fails.
func resolveMainRoot(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path, nil
	}
	return resolved, nil
}

// Diff returns the unified diff between two commits.
func Diff(ctx context.Context, dir, base, head string) (string, error) {
	return Run(ctx, dir, "diff", base+".."+head)
}

// DiffNameOnly returns the list of files changed between base and head.
// Output is split on newlines with empty entries removed.
func DiffNameOnly(ctx context.Context, dir, base, head string) ([]string, error) {
	out, err := Run(ctx, dir, "diff", "--name-only", base+".."+head)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			files = append(files, trimmed)
		}
	}
	return files, nil
}

// DiffStat returns the bounded size of the diff between base and head: the
// number of changed files and the net changed lines (insertions + deletions)
// from `git diff --numstat`. Binary files (numstat "-") contribute a changed
// file but no line count. It carries no paths or content - just two counts.
func DiffStat(ctx context.Context, dir, base, head string) (files, lines int, err error) {
	out, err := Run(ctx, dir, "diff", "--numstat", base+".."+head)
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		files++
		added, aerr := strconv.Atoi(fields[0])
		deleted, derr := strconv.Atoi(fields[1])
		if aerr == nil {
			lines += added
		}
		if derr == nil {
			lines += deleted
		}
	}
	return files, lines, nil
}

// CommitTime returns the committer timestamp for a SHA in UTC.
func CommitTime(ctx context.Context, dir, sha string) (time.Time, error) {
	out, err := Run(ctx, dir, "show", "-s", "--format=%ct", sha)
	if err != nil {
		return time.Time{}, err
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse commit time %q: %w", out, err)
	}
	return time.Unix(secs, 0).UTC(), nil
}

// CommitAuthorEmail returns the author email for a SHA.
func CommitAuthorEmail(ctx context.Context, dir, sha string) (string, error) {
	return Run(ctx, dir, "show", "-s", "--format=%ae", sha)
}

// DiffHead returns the unified diff between HEAD and the working tree
// (both staged and unstaged changes).
func DiffHead(ctx context.Context, dir string) (string, error) {
	return Run(ctx, dir, "diff", "HEAD")
}

// Log returns oneline log entries between two commits.
func Log(ctx context.Context, dir, base, head string) (string, error) {
	return Run(ctx, dir, "log", "--oneline", base+".."+head)
}

// HeadSHA returns the full SHA of HEAD.
func HeadSHA(ctx context.Context, dir string) (string, error) {
	return Run(ctx, dir, "rev-parse", "HEAD")
}

// CurrentBranch returns the current branch name.
func CurrentBranch(ctx context.Context, dir string) (string, error) {
	return Run(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
}

// IsDetachedHEAD reports whether the working tree is in a detached-HEAD state
// (HEAD points at a commit rather than a branch ref). Uses `git symbolic-ref`
// which fails cleanly when HEAD is not a symbolic ref.
func IsDetachedHEAD(ctx context.Context, dir string) (bool, error) {
	cmd := newCommand(ctx, "symbolic-ref", "-q", "HEAD")
	defer cmd.close()
	cmd.setDir(dir)
	if err := cmd.run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// Exit 1 means HEAD is not a symbolic ref — detached.
			if ee.ExitCode() == 1 {
				return true, nil
			}
		}
		return false, fmt.Errorf("git symbolic-ref: %w", err)
	}
	return false, nil
}

// DefaultBranch queries a remote to determine its default branch name.
// Uses git ls-remote --symref to read the remote's HEAD symref.
// Falls back to "main" if detection fails (e.g. empty remote, unreachable).
func DefaultBranch(ctx context.Context, dir, remote string) string {
	out, err := Run(ctx, dir, "ls-remote", "--symref", remote, "HEAD")
	if err != nil {
		return "main"
	}
	// Output format: "ref: refs/heads/main\tHEAD\n<sha>\tHEAD\n"
	// Fields splits: ["ref:", "refs/heads/main", "HEAD"]
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "ref: refs/heads/") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				return strings.TrimPrefix(parts[1], "refs/heads/")
			}
		}
	}
	return "main"
}

// FetchRemoteBranch fetches a single branch into a remote-tracking ref.
// Uses a force-update refspec (+) so non-fast-forward updates (e.g. after
// a force push on the remote) are accepted instead of silently rejected.
func FetchRemoteBranch(ctx context.Context, dir, remote, branch string) error {
	refspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", branch, remote, branch)
	_, err := Run(ctx, dir, "fetch", "--no-tags", remote, refspec)
	return err
}

func FetchRemoteBranchToRef(ctx context.Context, dir, remote, branch, localRef string) error {
	refspec := fmt.Sprintf("+refs/heads/%s:%s", branch, localRef)
	_, err := Run(ctx, dir, "fetch", "--no-tags", remote, refspec)
	return err
}

// FetchRemoteBranchToPrivateRef fetches one branch into a caller-owned private
// ref without touching FETCH_HEAD or ordinary remote-tracking refs.
func FetchRemoteBranchToPrivateRef(ctx context.Context, dir, remote, branch, localRef string) error {
	refspec := fmt.Sprintf("+refs/heads/%s:%s", branch, localRef)
	_, err := Run(ctx, dir, "fetch", "--no-tags", "--no-write-fetch-head", remote, refspec)
	return err
}

// Push pushes HEAD to a remote ref. If forceWithLease is true, it uses an
// explicit expected remote SHA for safe force-push.
func Push(ctx context.Context, dir, remote, ref, expectedSHA string, forceWithLease bool) error {
	return PushWithOptions(ctx, dir, remote, ref, expectedSHA, forceWithLease, nil)
}

// PushCommit pushes one immutable commit object to a remote ref. Unlike Push,
// a concurrent worktree HEAD move cannot change the source selected by git.
func PushCommit(ctx context.Context, dir, remote, commitSHA, ref, expectedSHA string, forceWithLease bool) error {
	return pushSourceWithOptions(ctx, dir, remote, commitSHA, ref, expectedSHA, forceWithLease, nil)
}

// PushWithOptions pushes HEAD to a remote with per-push options.
func PushWithOptions(ctx context.Context, dir, remote, ref, expectedSHA string, forceWithLease bool, pushOptions []string) error {
	return pushSourceWithOptions(ctx, dir, remote, "HEAD", ref, expectedSHA, forceWithLease, pushOptions)
}

func pushSourceWithOptions(ctx context.Context, dir, remote, source, ref, expectedSHA string, forceWithLease bool, pushOptions []string) error {
	args := []string{"push"}
	for _, option := range pushOptions {
		args = append(args, "-o", option)
	}
	args = append(args, remote)
	if forceWithLease {
		if expectedSHA != "" {
			args = append(args, fmt.Sprintf("--force-with-lease=%s:%s", ref, expectedSHA))
		} else {
			args = append(args, "--force-with-lease")
		}
	}
	args = append(args, source+":"+ref)
	_, err := Run(ctx, dir, args...)
	return err
}

// LsRemote returns the SHA of a ref on a remote. Returns empty string if the ref doesn't exist.
func LsRemote(ctx context.Context, dir, remote, ref string) (string, error) {
	out, err := Run(ctx, dir, "ls-remote", remote, ref)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", nil
	}
	// Output format: "<sha>\t<ref>"
	parts := strings.Fields(out)
	if len(parts) < 1 {
		return "", nil
	}
	return parts[0], nil
}

// HasUncommittedChanges reports whether the working tree or index differs from HEAD.
// Returns true if any tracked file is modified, staged, or deleted, or if there are
// untracked files. Equivalent to a non-empty `git status --porcelain`.
func HasUncommittedChanges(ctx context.Context, dir string) (bool, error) {
	out, err := Run(ctx, dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// CreateBranch creates a new branch with the given name and switches to it.
// Fails if the branch already exists.
func CreateBranch(ctx context.Context, dir, name string) error {
	_, err := Run(ctx, dir, "checkout", "-b", name)
	return err
}

// CommitAll stages every change in the working tree and creates a single commit
// with the given message. Fails if there are no changes to commit.
func CommitAll(ctx context.Context, dir, message string) error {
	if _, err := Run(ctx, dir, "add", "-A"); err != nil {
		return err
	}
	dirty, err := HasUncommittedChanges(ctx, dir)
	if err != nil {
		return err
	}
	if !dirty {
		return fmt.Errorf("no changes to commit")
	}
	_, err = Run(ctx, dir, "commit", "-m", message)
	return err
}

// CopyLocalUserIdentity copies local user.name and user.email from srcDir into
// dstDir. Missing values in srcDir are ignored.
//
// The write into dstDir uses per-worktree scope (`git config --worktree`) when
// the repository has worktree config enabled. dstDir is typically a linked
// worktree of the shared gate bare repo, where an unscoped `git config --local`
// write lands in the bare's shared config and takes <bare>/config.lock. Two
// runs starting concurrently on different branches of the same repo then race
// on that single lock and one fails with "could not lock config file ...
// config: File exists". Writing per-worktree puts each run's identity in its own
// <bare>/worktrees/<id>/config.worktree, so concurrent startups never contend.
// Older Git without `--worktree` support falls back to `--local`.
func CopyLocalUserIdentity(ctx context.Context, srcDir, dstDir string) error {
	for _, key := range []string{"user.name", "user.email"} {
		value, err := Run(ctx, srcDir, "config", "--local", "--get", "--default", "", key)
		if err != nil {
			return err
		}
		if value == "" {
			continue
		}
		if _, err := Run(ctx, dstDir, "config", "--worktree", key, value); err != nil {
			if !isWorktreeConfigWriteUnavailable(err) {
				return err
			}
			// Per-worktree config is not usable here (Git too old for the
			// flag, or the repo has multiple worktrees without
			// extensions.worktreeConfig enabled). Fall back to the shared
			// local config. Such gates also lack per-worktree isolation, so
			// this matches the legacy behavior.
			if _, err := Run(ctx, dstDir, "config", "--local", key, value); err != nil {
				return err
			}
		}
	}
	return nil
}

// isWorktreeConfigWriteUnavailable reports whether a `git config --worktree`
// write failed because per-worktree config cannot be used on this repo: either
// the installed Git is too old for the flag (isWorktreeConfigUnsupported), or
// the repo has more than one worktree without extensions.worktreeConfig enabled
// ("--worktree cannot be used with multiple working trees unless the config
// extension worktreeConfig is enabled"). Both mean the caller should fall back
// to the shared --local config.
func isWorktreeConfigWriteUnavailable(err error) bool {
	if isWorktreeConfigUnsupported(err) {
		return true
	}
	return strings.Contains(err.Error(), "worktreeConfig")
}

// WorktreeAdd creates a detached worktree at wtPath checked out to the given SHA.
func WorktreeAdd(ctx context.Context, repoDir, wtPath, sha string) error {
	_, err := Run(ctx, repoDir, "worktree", "add", "--detach", wtPath, sha)
	return err
}

// WorktreeRemove removes a worktree at the given path.
func WorktreeRemove(ctx context.Context, repoDir, wtPath string) error {
	_, err := Run(ctx, repoDir, "worktree", "remove", "--force", wtPath)
	return err
}

// ResolveRef returns the commit SHA that ref resolves to via
// `git rev-parse --verify <ref>^{commit}`. Use it to pin an exact commit
// (e.g. the default-branch tip just fetched) before reading a file from it,
// so a shared-ref worktree cannot serve a stale remote-tracking ref. Returns
// an error if the ref does not resolve to a commit.
func ResolveRef(ctx context.Context, dir, ref string) (string, error) {
	out, err := Run(ctx, dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve ref %s: %w", ref, err)
	}
	return out, nil
}

// RefExists reports whether the given ref resolves to a commit. It uses
// `git rev-parse --verify --quiet` so a missing ref is a clean (nil, false)
// result rather than a loud error.
func RefExists(ctx context.Context, dir, ref string) (bool, error) {
	cmd := newCommand(ctx, "-C", dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	defer cmd.close()
	cmd.setEnv(NonInteractiveEnv(dir))
	if err := cmd.run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("git rev-parse %s: %w", ref, err)
	}
	return true, nil
}

// ShowFile returns the content of path as stored at the given ref (e.g.
// "HEAD", "origin/main", or a SHA) via `git show <ref>:<path>`. A failure
// (e.g. the path is absent at the ref) is returned as the underlying git
// error from Run; callers that need to distinguish "absent" from a real
// failure should check RefExists first or inspect the error text.
func ShowFile(ctx context.Context, dir, ref, path string) (string, error) {
	out, err := Run(ctx, dir, "show", fmt.Sprintf("%s:%s", ref, path))
	if err != nil {
		return "", err
	}
	return out, nil
}
