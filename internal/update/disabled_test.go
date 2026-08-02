package update

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
)

// The self-updater overwrites the running binary in place. This build carries
// local document-guard patches that exist in no published release, so any
// successful update silently destroys them. These tests pin the kill switch and
// prove no exported entry point reaches the network while it is off.

// stubAPI points the package-level GitHub base URL at a server that records
// every request, so "no update happened" is proven by observed traffic rather
// than by a returned error alone.
func stubAPI(t *testing.T) *atomic.Int64 {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)

	prev := githubAPIBaseURL
	githubAPIBaseURL = srv.URL
	t.Cleanup(func() { githubAPIBaseURL = prev })
	return &hits
}

// seedUpgradeNotice arms the notice path so the guard is the only thing that can
// suppress it. Both pieces are required: under `go test` buildinfo.Version is
// "dev", which isDevVersion short-circuits before any cache is consulted, so
// without the version override these tests would pass whether the guard existed
// or not. The cache is written fresh and ahead of the current version so the
// notice fires while cacheStale stays false, keeping the test from spawning a
// real background process.
func seedUpgradeNotice(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("NM_HOME", root)

	prev := buildinfo.Version
	buildinfo.Version = "v1.41.2-no-slop-document-guard"
	t.Cleanup(func() { buildinfo.Version = prev })

	if err := writeCache(filepath.Join(root, "update-check.json"), &checkCache{
		CheckedAt:     time.Now(),
		LatestVersion: "v99.0.0",
	}); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
}

func TestSelfUpdateIsDisabled(t *testing.T) {
	if selfUpdateEnabled {
		t.Fatal("selfUpdateEnabled must stay false: an update would overwrite this binary and drop every local document-guard patch")
	}
}

// The updater must not aim at the upstream repository even if the kill switch
// is ever flipped, because upstream releases contain none of the local patches
// and every upstream tag sorts above this build's prerelease version.
func TestRepoNameIsNotUpstream(t *testing.T) {
	if strings.Contains(repoName, "kunchenguid") {
		t.Fatalf("repoName points at upstream (%q); an update would replace this build with an unpatched one", repoName)
	}
	if repoName != "Blakeolson21/no-slop" {
		t.Fatalf("repoName = %q, want Blakeolson21/no-slop", repoName)
	}
}

func TestRunRefusesAndMakesNoRequest(t *testing.T) {
	hits := stubAPI(t)

	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), &stdout, &stderr, RunOptions{Yes: true, Force: true})

	if !errors.Is(err, ErrSelfUpdateDisabled) {
		t.Fatalf("Run() error = %v, want ErrSelfUpdateDisabled", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("Run() made %d HTTP request(s); want 0", got)
	}
	if stdout.Len() != 0 {
		t.Fatalf("Run() wrote to stdout: %q", stdout.String())
	}
}

// A --yes run must fail rather than report success, so an agent scripting
// `no-mistakes update` sees a nonzero exit instead of a silent no-op.
func TestRunErrorExplainsHowToRebuild(t *testing.T) {
	msg := ErrSelfUpdateDisabled.Error()
	for _, want := range []string{"disabled", "go build", "Blakeolson21/no-slop"} {
		if !strings.Contains(msg, want) {
			t.Errorf("ErrSelfUpdateDisabled message missing %q:\n%s", want, msg)
		}
	}
}

// The notice is the footgun's delivery mechanism: it tells the reader to run
// the command that destroys the patches.
func TestMaybeNotifyAndCheckIsSilent(t *testing.T) {
	hits := stubAPI(t)
	seedUpgradeNotice(t)

	var stderr bytes.Buffer
	MaybeNotifyAndCheck([]string{"axi", "run"}, &stderr)

	if stderr.Len() != 0 {
		t.Fatalf("MaybeNotifyAndCheck() wrote an upgrade notice: %q", stderr.String())
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("MaybeNotifyAndCheck() made %d HTTP request(s); want 0", got)
	}
}

func TestCachedLatestVersionIsEmpty(t *testing.T) {
	seedUpgradeNotice(t)

	if got := CachedLatestVersion(); got != "" {
		t.Fatalf("CachedLatestVersion() = %q, want empty so the TUI shows no update badge", got)
	}
}

// The flag must still be consumed, or a stray background invocation would fall
// through to cobra as an unknown command.
func TestBackgroundCheckIsHandledWithoutNetwork(t *testing.T) {
	hits := stubAPI(t)

	handled, err := MaybeHandleBackgroundCheck([]string{backgroundFlag, "v1.41.2-no-slop-document-guard"})
	if !handled {
		t.Fatal("MaybeHandleBackgroundCheck() handled = false, want true so the flag is consumed")
	}
	if err != nil {
		t.Fatalf("MaybeHandleBackgroundCheck() error = %v, want nil", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("MaybeHandleBackgroundCheck() made %d HTTP request(s); want 0", got)
	}

	// Unrelated arguments must still pass through untouched.
	if handled, _ := MaybeHandleBackgroundCheck([]string{"axi", "run"}); handled {
		t.Fatal("MaybeHandleBackgroundCheck() claimed unrelated args")
	}
}
