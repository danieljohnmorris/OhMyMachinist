package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/controlplane"
	"github.com/spf13/cobra"
)

// newPlaneCommand is the built-in Plane intake: the same behaviour the retired
// plane-intake.py offered, driven from Machinist configuration and the
// control-plane database.
func newPlaneCommand(options *commandOptions) *cobra.Command {
	var home string
	plane := &cobra.Command{
		Use:   "plane",
		Short: "Run Plane intake: serve, poll, sync or replay a payload",
	}
	plane.PersistentFlags().StringVar(&home, "plane-home", "", "Machinist state directory (default ~/.machinist)")

	var dryRun bool
	poll := &cobra.Command{
		Use:   "poll",
		Short: "One intake pass over every configured Plane project",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return withPlaneIntake(options, home, func(intake *controlplane.PlaneIntake) error {
				queued, err := intake.Poll(command.Context(), dryRun)
				if err != nil {
					return err
				}
				fmt.Fprintf(options.stdout, "%d new job(s)\n", queued)
				return nil
			})
		},
	}
	poll.Flags().BoolVar(&dryRun, "dry-run", false, "evaluate without creating jobs or touching Plane")

	sync := &cobra.Command{
		Use:   "sync",
		Short: "Push run outcomes to the linked Plane tickets once",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return withPlaneIntake(options, home, func(intake *controlplane.PlaneIntake) error {
				return intake.Sync(command.Context())
			})
		},
	}
	sync.Flags().BoolVar(&dryRun, "dry-run", false, "evaluate without touching Plane")

	file := &cobra.Command{
		Use:   "file PAYLOAD",
		Short: "Replay one saved webhook payload",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			body, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			return withPlaneIntake(options, home, func(intake *controlplane.PlaneIntake) error {
				return intake.ProcessPayload(command.Context(), body, "file", dryRun)
			})
		},
	}
	file.Flags().BoolVar(&dryRun, "dry-run", false, "evaluate without creating jobs or touching Plane")

	serve := &cobra.Command{
		Use:   "serve",
		Short: "Serve the Plane webhook (same control plane as `machinist start`)",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return startControlPlane(command.Context(), options)
		},
	}

	plane.AddCommand(serve, poll, sync, file)
	return plane
}

// withPlaneIntake loads configuration, opens the store and runs the callback
// with an intake driver.
func withPlaneIntake(options *commandOptions, home string, run func(*controlplane.PlaneIntake) error) error {
	machinistConfig, err := config.LoadConfig(options.configPath)
	if err != nil {
		return err
	}
	if home == "" {
		if override := os.Getenv("MACHINIST_HOME"); override != "" {
			home = override
		} else {
			userHome, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			home = filepath.Join(userHome, ".machinist")
		}
	}
	store, err := controlplane.OpenStore(machinistConfig.Server.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	intake, err := controlplane.NewPlaneIntake(store, machinistConfig.Path(), home)
	if err != nil {
		return err
	}
	intake.SetCommandResolver(func(name string) (config.ResolvedCommand, error) {
		return config.LoadCommand(machinistConfig.Path(), name)
	})
	if err := intake.ImportState(context.Background()); err != nil {
		return err
	}
	return run(intake)
}

// startControlPlane starts the server; shared by `machinist start` and
// `machinist plane serve`.
func startControlPlane(ctx context.Context, options *commandOptions) error {
	machinistConfig, err := config.LoadConfig(options.configPath)
	if err != nil {
		return err
	}
	serverConfig := machinistConfig.Server
	if options.listen != "" {
		serverConfig.Listen = options.listen
	}
	if err := config.ValidateListenAuth(serverConfig.Auth, serverConfig.Listen); err != nil {
		return err
	}
	token, err := serverConfig.WorkerToken()
	if err != nil {
		return err
	}
	store, err := controlplane.OpenStore(serverConfig.Database)
	if err != nil {
		return err
	}
	defer store.Close()
	server, err := controlplane.NewServer(store, machinistConfig.Path(), token, serverConfig.ConcurrentJobLimit(), serverConfig.Auth)
	if err != nil {
		return err
	}
	return server.Serve(ctx, serverConfig.Listen, func(address net.Addr) {
		fmt.Fprintf(options.stderr, "machinist: control plane listening on http://%s\n", address)
	})
}
