package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/owainlewis/machinist/internal/skills"
	review "github.com/owainlewis/machinist/profiles/review"
	"github.com/spf13/cobra"
)

func newSkillsCommand(options *commandOptions) *cobra.Command {
	command := &cobra.Command{Use: "skills", Short: "Manage the versioned skills library"}
	command.AddCommand(newSkillsListCommand(options))
	command.AddCommand(newSkillsInstallCommand(options))
	command.AddCommand(newSkillsUpdateCommand(options))
	command.AddCommand(newSkillsPinCommand(options))
	command.AddCommand(newSkillsUninstallCommand(options))
	return command
}

func openSkillLibrary(options *commandOptions) (*skills.Library, error) {
	library, err := skills.Open(skills.DefaultRoot())
	if err != nil {
		return nil, err
	}
	library.Warn = func(message string) { fmt.Fprintf(options.stderr, "machinist: warning: %s\n", message) }
	return library, nil
}

// installSkillSource is shared by `machinist skills install` and the
// `machinist review install-skill` alias.
func installSkillSource(ctx context.Context, options *commandOptions, source string) error {
	library, err := openSkillLibrary(options)
	if err != nil {
		return err
	}
	parsed, err := skills.ParseSource(source)
	if err != nil {
		return err
	}
	result, err := library.Install(ctx, parsed)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(options.stdout, "installed skill %s version %s (active) into %s\n", result.Name, result.Version, library.Root)
	return err
}

func newSkillsInstallCommand(options *commandOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "install <github url | directory | zip>",
		Short: "Install a skill into the versioned library",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			return installSkillSource(command.Context(), options, args[0])
		},
	}
}

func newSkillsListCommand(options *commandOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List installed skills and their active version",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			library, err := openSkillLibrary(options)
			if err != nil {
				return err
			}
			installed, err := library.List()
			if err != nil {
				return err
			}
			for _, skill := range installed {
				if _, err := fmt.Fprintf(options.stdout, "%-40s active=%s versions=%d\n", skill.Name, skill.Active, len(skill.Versions)); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newSkillsUpdateCommand(options *commandOptions) *cobra.Command {
	var activate bool
	update := &cobra.Command{
		Use:   "update <name>",
		Short: "Re-resolve a skill's source and add a new version",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			library, err := openSkillLibrary(options)
			if err != nil {
				return err
			}
			result, err := library.Update(command.Context(), args[0], activate)
			if err != nil {
				return err
			}
			if !result.Added {
				_, err = fmt.Fprintf(options.stdout, "skill %s is already at version %s\n", result.Name, result.Version)
				return err
			}
			_, err = fmt.Fprintf(options.stdout, "updated skill %s: added version %s (active %s)\n", result.Name, result.Version, result.Active)
			return err
		},
	}
	update.Flags().BoolVar(&activate, "activate", false, "make the new version active")
	return update
}

func newSkillsPinCommand(options *commandOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "pin <name> <version>",
		Short: "Set the active version of an installed skill",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			library, err := openSkillLibrary(options)
			if err != nil {
				return err
			}
			if err := library.Pin(args[0], args[1]); err != nil {
				return err
			}
			_, err = fmt.Fprintf(options.stdout, "pinned skill %s to version %s\n", args[0], args[1])
			return err
		},
	}
}

func newSkillsUninstallCommand(options *commandOptions) *cobra.Command {
	var force bool
	uninstall := &cobra.Command{
		Use:   "uninstall <name>",
		Short: "Remove a skill and every installed version",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			library, err := openSkillLibrary(options)
			if err != nil {
				return err
			}
			if err := library.Uninstall(args[0], skills.UninstallOptions{Referenced: skillReferenced(args[0]), Force: force}); err != nil {
				return err
			}
			_, err = fmt.Fprintf(options.stdout, "uninstalled skill %s\n", args[0])
			return err
		},
	}
	uninstall.Flags().BoolVar(&force, "force", false, "remove the skill even when a review profile references it")
	return uninstall
}

// skillReferenced reports whether any shipped review profile still references
// the named skill.
func skillReferenced(name string) bool {
	key, err := skills.Key(name)
	if err != nil {
		return false
	}
	entries, err := review.FS.ReadDir(".")
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		body, err := review.FS.ReadFile(entry.Name())
		if err != nil {
			continue
		}
		for _, reference := range skills.ProfileReferences(string(body)) {
			if referenceKey, err := skills.Key(reference); err == nil && referenceKey == key {
				return true
			}
		}
	}
	return false
}
