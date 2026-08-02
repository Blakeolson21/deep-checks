package cli

import (
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/update"
)

// Self-update is disabled in this fork build: an update would overwrite the
// running binary with an upstream release carrying none of the local
// document-guard patches. These cases previously asserted the
// "self-update unavailable for development builds" message, which only appeared
// because buildinfo.Version is "dev" under `go test`. That made them silent
// about the real behavior, so a release build could have updated freely while
// they stayed green. The property worth pinning is that no flag combination
// reaches the update path.
func TestUpdateCommandRefusesRegardlessOfFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"bare", []string{"update"}},
		{"beta", []string{"update", "--beta"}},
		{"yes", []string{"update", "-y"}},
		{"force", []string{"update", "--force"}},
		{"yes and force", []string{"update", "-y", "--force"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateUpdateCommand(t)

			out, err := executeCmd(tc.args...)
			if err == nil {
				t.Fatalf("%v should refuse, got success\noutput: %s", tc.args, out)
			}
			if !errors.Is(err, update.ErrSelfUpdateDisabled) {
				t.Fatalf("%v error = %v, want ErrSelfUpdateDisabled\noutput: %s", tc.args, err, out)
			}
		})
	}
}

func isolateUpdateCommand(t *testing.T) {
	t.Helper()
	t.Setenv("NM_HOME", t.TempDir())
}
