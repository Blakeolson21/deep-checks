package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/lanehealth"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
	"github.com/spf13/cobra"
)

type doctorAgentCheck struct {
	name     string
	binaries []string
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check system health and dependencies",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return trackCommandStatus("doctor", func() (string, error) {
				w := cmd.OutOrStdout()
				allOK := true

				ok := func(label, detail string) {
					fmt.Fprintf(w, "  %s %s  %s\n", sGreen.Render("✓"), sDim.Render(label), detail)
				}
				warn := func(label, detail string) {
					fmt.Fprintf(w, "  %s %s  %s\n", sYellow.Render("–"), sDim.Render(label), detail)
				}
				fail := func(label, detail string) {
					fmt.Fprintf(w, "  %s %s  %s\n", sRed.Render("✗"), sDim.Render(label), detail)
				}

				fmt.Fprintf(w, "  %s\n", sCyan.Render("System"))

				if _, err := exec.LookPath("git"); err != nil {
					fail("git           ", "not found")
					allOK = false
				} else {
					gitCtx, cancelGit := git.BoundContext(cmd.Context(), "--version")
					defer cancelGit()
					gitCmd := exec.CommandContext(gitCtx, "git", "--version")
					gitCmd.WaitDelay = git.CommandWaitDelay()
					winproc.Harden(gitCmd)
					out, err := gitCmd.Output()
					if err != nil {
						fail("git           ", fmt.Sprintf("error (%v)", err))
						allOK = false
					} else {
						ok("git           ", strings.TrimSpace(string(out)))
					}
				}

				if _, err := exec.LookPath("gh"); err != nil {
					warn("gh            ", "not found "+sDim.Render("(optional, needed for PR/CI)"))
				} else {
					ok("gh            ", "ok")
				}

				if _, err := exec.LookPath("az"); err != nil {
					warn("az            ", "not found "+sDim.Render("(optional, needed for Azure DevOps PR/CI)"))
				} else {
					ok("az            ", "ok")
				}

				p, err := paths.New()
				if err != nil {
					fail("data directory", fmt.Sprintf("error resolving paths (%v)", err))
					allOK = false
				} else if _, err := os.Stat(p.Root()); os.IsNotExist(err) {
					fail("data directory", fmt.Sprintf("not found (%s)", p.Root()))
					allOK = false
				} else {
					ok("data directory", p.Root())
				}

				if p != nil {
					if _, err := os.Stat(p.DB()); os.IsNotExist(err) {
						warn("database      ", "not found "+sDim.Render("(will be created on first use)"))
					} else {
						d, err := db.Open(p.DB())
						if err != nil {
							fail("database      ", fmt.Sprintf("error (%v)", err))
							allOK = false
						} else {
							d.Close()
							ok("database      ", "ok")
						}
					}
				}

				if p != nil {
					alive, _ := daemon.IsRunning(p)
					if alive {
						ok("daemon        ", "running")
					} else {
						warn("daemon        ", "stopped")
					}
				}

				// A lane suppressed by a quota cooldown is installed and healthy-looking
				// but will not be used until it resets, so doctor is where that
				// otherwise-invisible state has to surface.
				laneOutages := map[string]lanehealth.Outage{}
				if p != nil {
					for _, outage := range lanehealth.NewStore(p.LaneHealthFile(), nil).Snapshot() {
						laneOutages[outage.Lane] = outage
					}
				}

				agents := doctorAgentChecks()
				fmt.Fprintln(w)
				fmt.Fprintf(w, "  %s\n", sCyan.Render("Agents"))
				for _, a := range agents {
					label := fmt.Sprintf("%-14s", a.name)
					var found, missing []string
					for _, bin := range a.binaries {
						if path, err := exec.LookPath(bin); err != nil {
							missing = append(missing, bin)
						} else {
							found = append(found, path)
						}
					}
					outage, exhausted := laneOutages[a.name]
					switch {
					case len(missing) == 0 && exhausted:
						warn(label, fmt.Sprintf("quota-exhausted until %s %s",
							outage.Until.Local().Format("2006-01-02 15:04 MST"),
							sDim.Render("(skipped by the pipeline until then)")))
					case len(missing) == 0:
						ok(label, strings.Join(found, ", "))
					case len(a.binaries) > 1:
						warn(label, "not found ("+strings.Join(missing, ", ")+")")
					default:
						warn(label, "not found")
					}
				}

				if p == nil {
					fail("gate validation", "unavailable: data directory could not be resolved")
					allOK = false
				} else {
					globalCfg, err := config.LoadGlobal(p.ConfigFile())
					if err != nil {
						fail("gate validation", fmt.Sprintf("unavailable: load config (%v)", err))
						allOK = false
					} else {
						cfg := config.Merge(globalCfg, &config.RepoConfig{})
						if err := cfg.ResolveAgent(cmd.Context(), exec.LookPath); err != nil {
							fail("gate validation", err.Error())
							allOK = false
						} else {
							ok("gate validation", fmt.Sprintf("%s is runnable", cfg.Agent))
						}
					}
				}

				if !allOK {
					fmt.Fprintln(w)
					fmt.Fprintf(w, "  %s\n", sRed.Render("some checks failed"))
					return "error", nil
				}

				return "success", nil
			})
		},
	}
}

func doctorAgentChecks() []doctorAgentCheck {
	agents := []doctorAgentCheck{
		{"claude", []string{"claude"}},
		{"codex", []string{"codex"}},
		{"rovodev", []string{"acli"}},
		{"opencode", []string{"opencode"}},
		{"pi", []string{"pi"}},
		{"copilot", []string{"copilot"}},
		{"acpx", []string{"acpx"}},
	}
	for _, alias := range types.ACPAliases() {
		agents = append(agents, doctorAgentCheck{
			name: string(alias.Name),
			binaries: []string{
				alias.DefaultCommandBinary(),
				"acpx",
			},
		})
	}
	return agents
}
