package cli

import (
	"github.com/kunchenguid/no-mistakes/internal/update"
	"github.com/spf13/cobra"
)

func newUpdateCmd() *cobra.Command {
	var beta bool
	var yes bool
	var force bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Self-update (disabled in this build; rebuild from source instead)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			logLifecycleInvocation("update", force, false)
			return trackCommand("update", func() error {
				return update.Run(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), update.RunOptions{Beta: beta, Yes: yes, Force: force, Stdin: cmd.InOrStdin()})
			})
		},
	}
	cmd.Flags().BoolVar(&beta, "beta", false, "accepted for compatibility; self-update is disabled in this build and this flag does not re-enable it")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "accepted for compatibility; self-update is disabled in this build and this flag does not re-enable it")
	cmd.Flags().BoolVar(&force, "force", false, "accepted for compatibility; self-update is disabled in this build and this flag does not re-enable it")
	return cmd
}
