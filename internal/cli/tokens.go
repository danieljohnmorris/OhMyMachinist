package cli

import (
	"fmt"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/controlplane"
	"github.com/spf13/cobra"
)

// newTokensCommand manages scoped API tokens against the server database.
// The plaintext secret is printed exactly once at creation; only its SHA-256
// hash is stored.
func newTokensCommand(options *commandOptions) *cobra.Command {
	tokens := &cobra.Command{
		Use:   "tokens",
		Short: "Create, list and revoke scoped API tokens",
	}
	tokens.AddCommand(newTokenCreateCommand(options))
	tokens.AddCommand(newTokenListCommand(options))
	tokens.AddCommand(newTokenRevokeCommand(options))
	return tokens
}

func openTokenStore(options *commandOptions) (*controlplane.Store, func(), int, error) {
	machinistConfig, err := config.LoadConfig(options.configPath)
	if err != nil {
		return nil, nil, 0, err
	}
	store, err := controlplane.OpenStore(machinistConfig.Server.Database)
	if err != nil {
		return nil, nil, 0, err
	}
	return store, func() { store.Close() }, machinistConfig.Server.ConcurrentJobLimit(), nil
}

func newTokenCreateCommand(options *commandOptions) *cobra.Command {
	var name, scope string
	create := &cobra.Command{
		Use:   "create",
		Short: "Create an API token (print the secret once)",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			store, closeStore, _, err := openTokenStore(options)
			if err != nil {
				return err
			}
			defer closeStore()
			token, secret, err := store.CreateToken(command.Context(), name, scope)
			if err != nil {
				return err
			}
			fmt.Fprintf(options.stderr, "machinist: created token %s (%s, scope %s); store the secret now, it is not shown again\n",
				token.ID, token.Name, token.Scope)
			fmt.Fprintln(options.stdout, secret)
			return nil
		},
	}
	create.Flags().StringVar(&name, "name", "", "token name (for the audit log)")
	create.Flags().StringVar(&scope, "scope", "read", "token scope: read or write")
	_ = create.MarkFlagRequired("name")
	return create
}

func newTokenListCommand(options *commandOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List API tokens (secrets are never shown)",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			store, closeStore, _, err := openTokenStore(options)
			if err != nil {
				return err
			}
			defer closeStore()
			tokens, err := store.ListTokens(command.Context())
			if err != nil {
				return err
			}
			for _, token := range tokens {
				revoked := ""
				if token.RevokedAt != nil {
					revoked = " revoked"
				}
				fmt.Fprintf(options.stdout, "%s\t%s\t%s%s\n", token.ID, token.Scope, token.Name, revoked)
			}
			return nil
		},
	}
}

func newTokenRevokeCommand(options *commandOptions) *cobra.Command {
	return &cobra.Command{
		Use:     "revoke <token-id>",
		Aliases: []string{"delete"},
		Short:   "Revoke an API token",
		Args:    cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			store, closeStore, _, err := openTokenStore(options)
			if err != nil {
				return err
			}
			defer closeStore()
			if err := store.RevokeToken(command.Context(), args[0]); err != nil {
				return fmt.Errorf("revoke token %s: %w", args[0], err)
			}
			fmt.Fprintf(options.stderr, "machinist: revoked token %s\n", args[0])
			return nil
		},
	}
}
