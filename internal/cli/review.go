package cli

import (
	"github.com/spf13/cobra"
)

func newReviewCommand(options *commandOptions) *cobra.Command {
	review := &cobra.Command{Use: "review", Short: "Manage review profiles and skills"}
	review.AddCommand(newInstallSkillCommand(options))
	return review
}

// newInstallSkillCommand keeps `machinist review install-skill` working as an
// alias for `machinist skills install`.
func newInstallSkillCommand(options *commandOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "install-skill <github SKILL.md url>",
		Short: "Install a reviewer skill (alias for machinist skills install)",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			return installSkillSource(command.Context(), options, args[0])
		},
	}
}
