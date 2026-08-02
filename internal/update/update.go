package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

const (
	appName = "no-mistakes"
	// This binary is built from the Blakeolson21/no-slop fork, which carries
	// local document-guard patches that exist in no published release. Its
	// version string (v1.41.2-no-slop-document-guard) is valid semver, so
	// isDevVersion does not classify it as a development build, and a
	// prerelease sorts below the plain release of the same core version. Left
	// pointed at the upstream repository, the self-updater would therefore see
	// every upstream tag as newer, download it, and overwrite this binary in
	// place, dropping every local patch with no warning.
	//
	// The fork cannot simply publish its own releases instead: release.yml's
	// build-darwin job fails closed without the upstream project's Apple
	// Developer ID secrets and pins their Team ID and bundle identifier, and
	// both the checksums and finalize jobs gate on that job succeeding. So
	// there is no release channel to point at, and the supported way to move
	// this build forward is to rebuild from source. Every exported entry point
	// below fails closed; repoName stays off upstream so that no code path,
	// including a directly constructed updater, can reach it.
	repoName           = "Blakeolson21/no-slop"
	backgroundFlag     = "--update-check"
	noUpdateCheckEnv   = "NO_MISTAKES_NO_UPDATE_CHECK"
	checksumsAssetName = "checksums.txt"
	cacheTTL           = 24 * time.Hour
	maxAPIResponseSize = 5 << 20
	maxDownloadSize    = 100 << 20
	maxExtractedSize   = 100 << 20
)

var allowInsecureDownloads bool
var githubAPIBaseURL = "https://api.github.com"
var currentGOOS = runtime.GOOS
var daemonIsRunning = daemon.IsRunning
var daemonExecutablePath = runningDaemonExecutablePath
var daemonStop = daemon.Stop
var daemonStart = daemon.Start
var windowsExecutablePathForPID = defaultWindowsExecutablePathForPID

type platformSpec struct {
	GOOS   string
	GOARCH string
}

type updater struct {
	appName            string
	repo               string
	currentVersion     string
	platform           platformSpec
	apiBaseURL         string
	httpClient         *http.Client
	cachePath          string
	executablePath     string
	stdin              io.Reader
	stdout             io.Writer
	stderr             io.Writer
	now                func() time.Time
	spawnBackground    func(currentVersion string) error
	resetDaemon        func() error
	paths              *paths.Paths
	disableBackground  bool
	noColor            bool
	includePrereleases bool
	assumeYes          bool
	force              bool
}

type RunOptions struct {
	Beta  bool
	Yes   bool
	Force bool
	Stdin io.Reader
}

// ErrSelfUpdateDisabled is returned by Run in place of performing an update.
// See the repoName comment for why this build has no release channel.
var ErrSelfUpdateDisabled = errors.New(`self-update is disabled in this build.

This is the Blakeolson21/no-slop fork, which carries local document-guard
patches that exist in no published release. Updating would download a release
archive and overwrite this binary in place, silently dropping every patch.

To move this build forward, rebuild from a checkout of
github.com/Blakeolson21/no-slop:

    go build -o "$(command -v no-mistakes)" ./cmd/no-mistakes`)

// selfUpdateEnabled gates every exported entry point in this package. It is a
// var rather than a const so the update machinery below stays reachable to the
// compiler and to tests, which still exercise it directly against a stub
// GitHub API. See the repoName comment for why it is false.
var selfUpdateEnabled = false

func Run(ctx context.Context, stdout, stderr io.Writer, opts RunOptions) error {
	if !selfUpdateEnabled {
		return ErrSelfUpdateDisabled
	}
	u, err := defaultUpdater(stdout, stderr)
	if err != nil {
		return err
	}
	u.includePrereleases = opts.Beta
	u.assumeYes = opts.Yes
	u.force = opts.Force
	if opts.Stdin != nil {
		u.stdin = opts.Stdin
	}
	return u.run(ctx)
}

func MaybeHandleBackgroundCheck(args []string) (bool, error) {
	if len(args) != 2 || args[0] != backgroundFlag {
		return false, nil
	}
	// Still report the flag as handled so a stray background invocation exits
	// quietly instead of falling through to the CLI as an unknown command.
	if !selfUpdateEnabled {
		return true, nil
	}
	u, err := defaultUpdater(io.Discard, io.Discard)
	if err != nil {
		return true, err
	}
	u.currentVersion = args[1]
	return true, u.refreshCache(context.Background())
}

func MaybeNotifyAndCheck(args []string, stderr io.Writer) {
	// Without this guard every command would spawn a background probe and print
	// an upgrade notice inviting the reader to run the very command that would
	// destroy this build's local patches.
	if !selfUpdateEnabled {
		return
	}
	u, err := defaultUpdater(io.Discard, stderr)
	if err != nil {
		return
	}
	u.maybeNotifyAndCheck(args)
}

func CachedLatestVersion() string {
	// Suppresses the TUI's "update available" badge for the same reason.
	if !selfUpdateEnabled {
		return ""
	}
	u, err := defaultUpdater(io.Discard, io.Discard)
	if err != nil {
		return ""
	}
	return u.cachedLatestVersion()
}

func defaultUpdater(stdout, stderr io.Writer) (*updater, error) {
	p, err := paths.New()
	if err != nil {
		return nil, fmt.Errorf("resolve paths: %w", err)
	}
	execPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable: %w", err)
	}
	return &updater{
		appName:         appName,
		repo:            repoName,
		currentVersion:  buildinfo.CurrentVersion(),
		platform:        platformSpec{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH},
		apiBaseURL:      githubAPIBaseURL,
		httpClient:      &http.Client{Timeout: 30 * time.Second},
		cachePath:       p.UpdateCheckFile(),
		executablePath:  execPath,
		stdin:           os.Stdin,
		stdout:          stdout,
		stderr:          stderr,
		now:             time.Now,
		paths:           p,
		spawnBackground: defaultSpawnBackground,
		resetDaemon: func() error {
			return defaultResetDaemon(p)
		},
	}, nil
}

func (u *updater) refreshCache(ctx context.Context) error {
	plan, err := u.checkLatest(ctx)
	if err != nil {
		return err
	}
	return writeCache(u.cachePath, &checkCache{
		CheckedAt:     u.now(),
		LatestVersion: plan.LatestVersion,
	})
}

func (u *updater) maybeNotifyAndCheck(args []string) {
	if u.disableBackground || isDevVersion(u.currentVersion) || os.Getenv(noUpdateCheckEnv) == "1" {
		return
	}
	// Informational commands must be side-effect-free probes: `update` and the
	// background refresh must not re-enter it, and a version query (`--version`
	// / `-v`) must never print a notice or spawn a background refresh so that
	// supervision scripts can call it as an innocuous health check (#401).
	if len(args) > 0 && (args[0] == "update" || args[0] == backgroundFlag || args[0] == "--version" || args[0] == "-v") {
		return
	}
	cache := readCache(u.cachePath)
	if cache != nil {
		cmp, err := compareVersions(u.currentVersion, cache.LatestVersion)
		if err == nil && cmp < 0 {
			fmt.Fprintf(u.stderrWriter(), "%sA new version of %s is available: %s -> %s\nRun \"%s update\" to update%s\n", u.yellow(), u.appName, u.currentVersion, cache.LatestVersion, u.appName, u.reset())
		}
	}
	if cacheStale(cache, u.currentVersion, u.now()) && u.spawnBackground != nil {
		_ = u.spawnBackground(u.currentVersion)
	}
}

func (u *updater) cachedLatestVersion() string {
	if u == nil || u.disableBackground || isDevVersion(u.currentVersion) || os.Getenv(noUpdateCheckEnv) == "1" {
		return ""
	}
	cache := readCache(u.cachePath)
	if cache == nil {
		return ""
	}
	cmp, err := compareVersions(u.currentVersion, cache.LatestVersion)
	if err != nil || cmp >= 0 {
		return ""
	}
	return cache.LatestVersion
}

func (u *updater) run(ctx context.Context) error {
	if isDevVersion(u.currentVersion) {
		fmt.Fprintf(u.stdoutWriter(), "self-update unavailable for development builds (%s)\n", u.currentVersion)
		return nil
	}
	plan, err := u.checkLatest(ctx)
	if err != nil {
		return err
	}
	if err := writeCache(u.cachePath, &checkCache{CheckedAt: u.now(), LatestVersion: plan.LatestVersion}); err != nil {
		return err
	}
	if !plan.UpdateAvailable {
		fmt.Fprintf(u.stdoutWriter(), "%s is already up to date (%s)\n", u.appName, u.currentVersion)
		return nil
	}
	if err := u.confirmActiveRunsBeforeUpdate(); err != nil {
		return err
	}
	if err := u.ensureDaemonUsesCurrentExecutable(); err != nil {
		return err
	}

	archiveData, err := u.downloadAsset(ctx, plan.Archive.BrowserDownloadURL, maxDownloadSize)
	if err != nil {
		return err
	}
	checksumsData, err := u.downloadAsset(ctx, plan.Checksums.BrowserDownloadURL, maxDownloadSize)
	if err != nil {
		return err
	}
	checksums, err := parseChecksums(checksumsData)
	if err != nil {
		return err
	}
	want, ok := checksums[plan.ArchiveName]
	if !ok {
		return fmt.Errorf("checksum not found for %s", plan.ArchiveName)
	}
	if err := verifyChecksum(archiveData, want); err != nil {
		return err
	}
	binaryData, err := u.extractBinary(archiveData)
	if err != nil {
		return err
	}
	if err := replaceExecutable(u.executablePath, binaryData); err != nil {
		return err
	}
	if u.resetDaemon != nil {
		if err := u.resetDaemon(); err != nil {
			var resetErr *daemonResetError
			if errors.As(err, &resetErr) && resetErr.daemonOffline {
				return fmt.Errorf("updated %s to %s, but daemon is offline: %w", u.appName, plan.LatestVersion, err)
			}
			return fmt.Errorf("updated %s to %s, but failed to reset daemon: %w", u.appName, plan.LatestVersion, err)
		}
	}
	fmt.Fprintf(u.stdoutWriter(), "updated %s from %s to %s\n", u.appName, u.currentVersion, plan.LatestVersion)
	return nil
}

func (u *updater) stdoutWriter() io.Writer {
	if u.stdout == nil {
		return io.Discard
	}
	return u.stdout
}

func (u *updater) stderrWriter() io.Writer {
	if u.stderr == nil {
		return io.Discard
	}
	return u.stderr
}

func (u *updater) yellow() string {
	if u.noColor {
		return ""
	}
	return "\033[33m"
}

func (u *updater) reset() string {
	if u.noColor {
		return ""
	}
	return "\033[0m"
}
