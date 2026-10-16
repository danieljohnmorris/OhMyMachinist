package cli

import (
	"context"
	"fmt"
	"strconv"

	"github.com/owainlewis/machinist/internal/controlplane"
	"github.com/spf13/cobra"
)

// factoryIdentity names control-plane changes made from the CLI without a
// token, so the audit log still says who changed what.
var factoryIdentity = controlplane.TokenIdentity{Name: "cli"}

// newPauseCommand stops dispatching new jobs. It replaces stopping systemd
// units for spark-mode builds: workers stay connected and the running job is
// drained (or cancelled with --cancel).
func newPauseCommand(options *commandOptions) *cobra.Command {
	var reason string
	var cancel bool
	pause := &cobra.Command{
		Use:   "pause",
		Short: "Stop dispatching new jobs (drain the running job, or cancel it)",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			store, closeStore, err := openTokenStore(options)
			if err != nil {
				return err
			}
			defer closeStore()
			mode := "drain"
			if cancel {
				mode = "cancel"
			}
			if err := store.PauseFactory(command.Context(), factoryIdentity.Name, reason, mode); err != nil {
				return err
			}
			action := "factory.drain"
			if cancel {
				action = "factory.pause"
			}
			recordFactoryCLIAudit(command.Context(), options, store, action, fmt.Sprintf("mode=%s reason=%s", mode, reason))
			return printFactoryState(command.Context(), options, store)
		},
	}
	pause.Flags().StringVar(&reason, "reason", "", "why the factory is being stopped (recorded in the audit log)")
	pause.Flags().BoolVar(&cancel, "cancel", false, "cancel the running job instead of letting it finish")
	return pause
}

func newResumeCommand(options *commandOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "Resume dispatching new jobs",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			store, closeStore, err := openTokenStore(options)
			if err != nil {
				return err
			}
			defer closeStore()
			if err := store.ResumeFactory(command.Context()); err != nil {
				return err
			}
			recordFactoryCLIAudit(command.Context(), options, store, "factory.start", "resumed dispatch")
			return printFactoryState(command.Context(), options, store)
		},
	}
}

func newJobCapCommand(options *commandOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "job-cap <n>",
		Short: "Set how many jobs run at once (clamped to the enabled lanes)",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			value, err := strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("job cap must be an integer, got %q", args[0])
			}
			store, closeStore, err := openTokenStore(options)
			if err != nil {
				return err
			}
			defer closeStore()
			before, err := store.FactoryStatus(command.Context(), 0)
			if err != nil {
				return err
			}
			clamped, err := store.SetFactoryJobCap(command.Context(), value)
			if err != nil {
				return err
			}
			recordFactoryCLIAudit(command.Context(), options, store, "factory.job_cap", fmt.Sprintf("old=%d new=%d requested=%d", before.MaxConcurrentJobs, clamped, value))
			return printFactoryState(command.Context(), options, store)
		},
	}
}

func newLaneCommand(options *commandOptions) *cobra.Command {
	lane := &cobra.Command{
		Use:   "lane",
		Short: "List, enable and disable lanes",
	}
	lane.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List lanes with their state and repository checkouts",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			store, closeStore, err := openTokenStore(options)
			if err != nil {
				return err
			}
			defer closeStore()
			return printFactoryState(command.Context(), options, store)
		},
	})
	lane.AddCommand(newLaneToggleCommand(options, true))
	lane.AddCommand(newLaneToggleCommand(options, false))
	return lane
}

func newLaneToggleCommand(options *commandOptions, enabled bool) *cobra.Command {
	use, short := "enable <name>", "Enable a lane so it takes new jobs"
	if !enabled {
		use, short = "disable <name>", "Disable a lane (stays connected, gets no new jobs)"
	}
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			store, closeStore, err := openTokenStore(options)
			if err != nil {
				return err
			}
			defer closeStore()
			if err := store.SetLaneEnabled(command.Context(), args[0], enabled); err != nil {
				return err
			}
			action, value := "factory.lane_disable", "disabled"
			if enabled {
				action, value = "factory.lane_enable", "enabled"
			}
			recordFactoryCLIAudit(command.Context(), options, store, action, fmt.Sprintf("lane=%s new=%s", args[0], value))
			return printFactoryState(command.Context(), options, store)
		},
	}
}

func recordFactoryCLIAudit(ctx context.Context, options *commandOptions, store *controlplane.Store, action, detail string) {
	if err := store.RecordAudit(ctx, factoryIdentity, action, detail); err != nil {
		fmt.Fprintf(options.stderr, "machinist: audit %s: %v\n", action, err)
	}
}

// printFactoryState renders the current dispatch state, cap and lanes.
func printFactoryState(ctx context.Context, options *commandOptions, store *controlplane.Store) error {
	state, err := store.FactoryStatus(ctx, 0)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(options.stdout, "state: %s", state.State); err != nil {
		return err
	}
	if state.Reason != "" {
		fmt.Fprintf(options.stdout, " (%s by %s)", state.Reason, state.Actor)
	}
	fmt.Fprintf(options.stdout, "  jobs-at-once: %d  ready-lanes: %d\n", state.MaxConcurrentJobs, state.ReadyLanes)
	for _, lane := range state.Lanes {
		enabled := "enabled"
		if !lane.Enabled {
			enabled = "disabled"
		}
		fmt.Fprintf(options.stdout, "  %-24s %-9s %-8s %s\n", lane.Name, lane.State, enabled, joinRepositories(lane.Repositories))
	}
	return nil
}

func joinRepositories(repositories []string) string {
	if len(repositories) == 0 {
		return "-"
	}
	joined := repositories[0]
	for _, repository := range repositories[1:] {
		joined += "," + repository
	}
	return joined
}
