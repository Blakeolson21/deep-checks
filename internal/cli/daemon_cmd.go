package cli

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/lifecycle"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

var (
	daemonRun               = daemon.Run
	daemonStartFn           = daemon.Start
	daemonStopFn            = daemon.Stop
	daemonIsRunningFn       = daemon.IsRunning
	daemonProvablyStoppedFn = daemon.ProvablyStopped
)

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the no-mistakes daemon",
	}

	cmd.AddCommand(newDaemonStartCmd())
	cmd.AddCommand(newDaemonStopCmd())
	cmd.AddCommand(newDaemonRestartCmd())
	cmd.AddCommand(newDaemonStatusCmd())
	cmd.AddCommand(newDaemonRunCmd())
	cmd.AddCommand(newDaemonAdmitPushCmd())
	cmd.AddCommand(newDaemonNotifyPushCmd())

	return cmd
}

func newDaemonAdmitPushCmd() *cobra.Command {
	var gate string
	cmd := &cobra.Command{
		Use:    "admit-push",
		Short:  "Authorize a managed gate ref update",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			gatePath, err := normalizeNotifyGatePath(gate)
			if err != nil {
				return err
			}
			p, err := paths.New()
			if err != nil {
				return err
			}
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				return fmt.Errorf("connect to daemon: %w", err)
			}
			defer client.Close()
			var result ipc.AdmitPushResult
			if err := client.Call(ipc.MethodAdmitPush, &ipc.AdmitPushParams{Gate: gatePath}, &result); err != nil {
				return err
			}
			if !result.Context.Nested {
				return nil
			}
			return emitGateContextRefusal(cmd, gatecontext.Result{
				Nested:           result.Context.Nested,
				ManagedGit:       result.Context.ManagedGit,
				AgentDescendant:  result.Context.AgentDescendant,
				DaemonDescendant: result.Context.DaemonDescendant,
				MarkerPresent:    result.Context.MarkerPresent,
				RunID:            result.Context.RunID,
				Phase:            result.Context.Phase,
			})
		},
	}
	cmd.Flags().StringVar(&gate, "gate", "", "bare repo path that is about to receive a push")
	_ = cmd.MarkFlagRequired("gate")
	return cmd
}

func newDaemonNotifyPushCmd() *cobra.Command {
	var gate string
	var ref string
	var oldSHA string
	var newSHA string
	var pushOptions []string

	cmd := &cobra.Command{
		Use:    "notify-push",
		Short:  "Notify daemon about a git push",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			skipSteps, err := parseSkipPushOptions(pushOptions)
			if err != nil {
				return err
			}
			intent, err := parseIntentPushOptions(pushOptions)
			if err != nil {
				return err
			}
			gatePath, err := normalizeNotifyGatePath(gate)
			if err != nil {
				return err
			}

			p, err := paths.New()
			if err != nil {
				return err
			}

			client, err := ipc.Dial(p.Socket())
			if err != nil {
				return fmt.Errorf("connect to daemon: %w", err)
			}
			defer client.Close()

			var result ipc.PushReceivedResult
			return client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
				Gate:      gatePath,
				Ref:       ref,
				Old:       oldSHA,
				New:       newSHA,
				SkipSteps: skipSteps,
				Intent:    intent,
			}, &result)
		},
	}

	cmd.Flags().StringVar(&gate, "gate", "", "bare repo path that received the push")
	cmd.Flags().StringVar(&ref, "ref", "", "git ref name")
	cmd.Flags().StringVar(&oldSHA, "old", "", "previous commit SHA")
	cmd.Flags().StringVar(&newSHA, "new", "", "new commit SHA")
	cmd.Flags().StringArrayVar(&pushOptions, "push-option", nil, "git push option")
	_ = cmd.MarkFlagRequired("gate")
	_ = cmd.MarkFlagRequired("ref")
	_ = cmd.MarkFlagRequired("old")
	_ = cmd.MarkFlagRequired("new")

	return cmd
}

func normalizeNotifyGatePath(gate string) (string, error) {
	if strings.TrimSpace(gate) == "" {
		return "", fmt.Errorf("gate path is required")
	}
	abs, err := filepath.Abs(gate)
	if err != nil {
		return "", fmt.Errorf("resolve gate path: %w", err)
	}
	return filepath.Clean(abs), nil
}

func parseSkipPushOptions(options []string) ([]types.StepName, error) {
	var steps []types.StepName
	for _, option := range options {
		value, ok := strings.CutPrefix(option, "no-mistakes.skip=")
		if !ok {
			continue
		}
		parsed, err := parseSkipSteps(value)
		if err != nil {
			return nil, err
		}
		steps = append(steps, parsed...)
	}
	return dedupeSteps(steps), nil
}

func parseSkipSteps(value string) ([]types.StepName, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var steps []types.StepName
	for _, part := range strings.Split(value, ",") {
		step := types.StepName(strings.TrimSpace(part))
		if !validStep(step) {
			return nil, fmt.Errorf("unknown step %q", step)
		}
		steps = append(steps, step)
	}
	return dedupeSteps(steps), nil
}

// intentPushOptionPrefix carries an agent-supplied intent through a git push.
// The value is base64-encoded so multi-line or special-character intents
// survive the push-option transport (which is line-oriented).
const intentPushOptionPrefix = "no-mistakes.intent="

// formatIntentPushOption encodes intent as a single push option, or returns ""
// when there is no intent to carry.
func formatIntentPushOption(intent string) string {
	if strings.TrimSpace(intent) == "" {
		return ""
	}
	return intentPushOptionPrefix + base64.StdEncoding.EncodeToString([]byte(intent))
}

// parseIntentPushOptions extracts and decodes the intent push option, if any.
// The last occurrence wins.
func parseIntentPushOptions(options []string) (string, error) {
	intent := ""
	for _, option := range options {
		encoded, ok := strings.CutPrefix(option, intentPushOptionPrefix)
		if !ok {
			continue
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "", fmt.Errorf("decode intent push option: %w", err)
		}
		intent = string(decoded)
	}
	return intent, nil
}

func formatSkipPushOptions(steps []types.StepName) []string {
	if len(steps) == 0 {
		return nil
	}
	parts := make([]string, 0, len(steps))
	for _, step := range dedupeSteps(steps) {
		parts = append(parts, string(step))
	}
	return []string{"no-mistakes.skip=" + strings.Join(parts, ",")}
}

func validStep(step types.StepName) bool {
	for _, known := range types.AllSteps() {
		if step == known {
			return true
		}
	}
	return false
}

func dedupeSteps(steps []types.StepName) []types.StepName {
	seen := make(map[types.StepName]bool, len(steps))
	out := make([]types.StepName, 0, len(steps))
	for _, step := range steps {
		if seen[step] {
			continue
		}
		seen[step] = true
		out = append(out, step)
	}
	return out
}

func newDaemonStartCmd() *cobra.Command {
	var abandonExecuting bool
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Install or refresh the managed daemon service and start it",
		RunE: func(cmd *cobra.Command, args []string) error {
			logLifecycleInvocation("daemon.start", false, abandonExecuting)
			return trackCommand("daemon.start", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				if err := p.EnsureDirs(); err != nil {
					return err
				}
				if err := guardDaemonStartAgainstExecutingRuns(p, "daemon start", abandonExecuting); err != nil {
					return err
				}
				if err := daemonStartFn(p); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon started\n", sGreen.Render("✓"))
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&abandonExecuting, "abandon-executing-runs", false, "refresh a stale service definition even while a run is executing a step, failing that run")
	return cmd
}

func newDaemonStopCmd() *cobra.Command {
	var force bool
	var abandonExecuting bool
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			logLifecycleInvocation("daemon.stop", force, abandonExecuting)
			return trackCommand("daemon.stop", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				if err := guardDestructiveDaemonLifecycle(p, cmd.ErrOrStderr(), "daemon stop", force, abandonExecuting); err != nil {
					return err
				}
				if err := daemonStopFn(p); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon stopped\n", sGreen.Render("✓"))
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "stop the daemon even when parked or idle pipeline runs exist")
	cmd.Flags().BoolVar(&abandonExecuting, "abandon-executing-runs", false, "also stop the daemon while a run is executing a step, failing that run (implies --force)")
	return cmd
}

func newDaemonRestartCmd() *cobra.Command {
	var force bool
	var abandonExecuting bool
	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Restart the daemon (stop if running, then start)",
		RunE: func(cmd *cobra.Command, args []string) error {
			logLifecycleInvocation("daemon.restart", force, abandonExecuting)
			return trackCommand("daemon.restart", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				if err := p.EnsureDirs(); err != nil {
					return err
				}
				if err := guardDestructiveDaemonLifecycle(p, cmd.ErrOrStderr(), "daemon restart", force, abandonExecuting); err != nil {
					return err
				}
				if err := daemonStopFn(p); err != nil {
					return fmt.Errorf("stop daemon: %w", err)
				}
				if err := daemonStartFn(p); err != nil {
					return fmt.Errorf("start daemon: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon restarted\n", sGreen.Render("✓"))
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "restart the daemon even when parked or idle pipeline runs exist")
	cmd.Flags().BoolVar(&abandonExecuting, "abandon-executing-runs", false, "also restart the daemon while a run is executing a step, failing that run (implies --force)")
	return cmd
}

// guardDestructiveDaemonLifecycle refuses to stop or restart the daemon while
// pipeline runs are active, in two tiers.
//
// --force covers runs that survive a daemon stop: a run parked at a gate is
// resumed by startup recovery, and an idle row was never in flight. It does
// not cover a run executing a step, because stopping the daemon cancels that
// step, fails the run, and strands its pipeline commits in the local gate.
// That case needs --abandon-executing-runs, which says what it costs.
func guardDestructiveDaemonLifecycle(p *paths.Paths, stderr io.Writer, action string, force, abandonExecuting bool) error {
	// A health probe that errors has not proven the daemon is down. Assume it
	// is up so runs are classified on their own evidence rather than written
	// off as idle: this guard's job is to fail closed.
	//
	// Positive proof of death is the one thing that outranks that assumption.
	// An unclean death leaves a socket file with nothing listening, which is
	// precisely the shape that makes the probe error, so without this the
	// leftover rows of the crashed run read as "executing a step" and the
	// operator is told to pass --abandon-executing-runs to a daemon whose
	// recorded process no longer exists.
	runs, err := lifecycle.ClassifyActiveRuns(p, func() bool {
		alive, err := daemonIsRunningFn(p)
		if err == nil {
			return alive
		}
		return !daemonProvablyStoppedFn(p)
	})
	if err != nil {
		return fmt.Errorf("check active pipeline runs: %w", err)
	}
	if len(runs) == 0 {
		return nil
	}
	if err := refuseExecutingRuns(action, "--force does not cover executing runs, because ", runs, abandonExecuting); err != nil {
		return err
	}
	if force || abandonExecuting {
		fmt.Fprintf(stderr, "FORCE: %s will stop/restart the daemon while %d active pipeline runs are in progress\n", action, len(runs))
		fmt.Fprint(stderr, lifecycle.ActiveRunList(runs))
		return nil
	}
	return fmt.Errorf("refusing %s because %d active pipeline runs are in progress; pass --force to stop/restart the daemon anyway\n%s", action, len(runs), lifecycle.ActiveRunList(runs))
}

// guardDaemonStartAgainstExecutingRuns closes the same hole for `daemon start`,
// which is destructive in one shape: with a daemon already running, Start
// refreshes a drifted managed service definition by stopping the current
// daemon and restarting the service. That kills whatever step is executing and
// strands its pipeline commits, exactly like an unguarded `daemon stop`.
//
// Only the executing tier applies. Starting the daemon is how an operator
// recovers from a dead one, so parked and idle rows must never refuse it. The
// probe is read strictly: only a daemon that answers healthy can reach the
// service-refresh path inside Start, so unlike stop and restart a probe error
// means the destructive branch is unreachable rather than unproven.
func guardDaemonStartAgainstExecutingRuns(p *paths.Paths, action string, abandonExecuting bool) error {
	runs, err := lifecycle.ClassifyActiveRuns(p, func() bool {
		alive, err := daemonIsRunningFn(p)
		return err == nil && alive
	})
	if err != nil {
		return fmt.Errorf("check active pipeline runs: %w", err)
	}
	return refuseExecutingRuns(action, "refreshing a stale managed service definition restarts the daemon, and ", runs, abandonExecuting)
}

func refuseExecutingRuns(action, scopeNote string, runs []lifecycle.ActiveRun, abandonExecuting bool) error {
	if abandonExecuting {
		return nil
	}
	executing := lifecycle.ExecutingRuns(runs)
	if len(executing) == 0 {
		return nil
	}
	return fmt.Errorf("refusing %s because %d active pipeline %s executing a step right now; %sstopping the daemon cancels the step and strands the run's pipeline commits in the local gate. Wait for the step to finish or park at a gate, end the run with `no-mistakes axi abort --run <id>`, or pass --abandon-executing-runs to fail it deliberately\n%s",
		action, len(executing), runNoun(len(executing)), scopeNote, lifecycle.ExecutingRunList(executing))
}

func runNoun(count int) string {
	if count == 1 {
		return "run is"
	}
	return "runs are"
}

func newDaemonStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Check if the daemon is running",
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommand("daemon.status", func() error {
				p, err := paths.New()
				if err != nil {
					return err
				}
				alive, err := daemonIsRunningFn(p)
				if err != nil {
					return err
				}
				if alive {
					pid, _ := daemon.ReadPID(p)
					if pid > 0 {
						fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon running %s\n", sGreen.Render("●"), sDim.Render(fmt.Sprintf("(pid %d)", pid)))
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon running\n", sGreen.Render("●"))
					}
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s daemon not running\n", sDim.Render("○"))
				}
				return nil
			})
		},
	}
}

func newDaemonRunCmd() *cobra.Command {
	var root string

	cmd := &cobra.Command{
		Use:    "run",
		Short:  "Run the daemon in the foreground",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if root != "" {
				if err := os.Setenv("NM_HOME", root); err != nil {
					return fmt.Errorf("set NM_HOME: %w", err)
				}
			}
			return daemonRun()
		},
	}

	cmd.Flags().StringVar(&root, "root", "", "override no-mistakes data directory")
	return cmd
}
