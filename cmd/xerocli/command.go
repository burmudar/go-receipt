package xerocli

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/db/oauth"
	"github.com/burmudar/go-receipt/xero"
	"github.com/spf13/cobra"
)

func Cmd() *cobra.Command {
	var database string
	var timeout time.Duration
	cmd := &cobra.Command{Use: "xero", Short: "Connect Xero and read supported receipt attachment targets"}
	cmd.PersistentFlags().StringVar(&database, "db", "app.db", "migrated SQLite database")
	cmd.PersistentFlags().DurationVar(&timeout, "timeout", 5*time.Minute, "command timeout (login is limited to five minutes)")

	run := func(action func(context.Context, *cobra.Command, *xero.Store, *xero.Client) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, _ []string) error {
			if timeout <= 0 {
				return errors.New("timeout must be positive")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			conn, err := db.Open(database)
			if err != nil {
				return err
			}
			defer conn.Close()
			store := xero.NewStore(conn)
			// Local inspection does not require configured client credentials.
			if cmd.Name() == "status" || cmd.Name() == "list" {
				return action(ctx, cmd, store, nil)
			}
			clients, err := oauth.NewStore(conn).List()
			if err != nil {
				return err
			}
			for _, credentials := range clients {
				if credentials.Name == "Xero" {
					return action(ctx, cmd, store, xero.NewClient(store, *credentials))
				}
			}
			return errors.New("Xero client is not configured: run oauth set first")
		}
	}
	cmd.AddCommand(&cobra.Command{Use: "login", Short: "Authorize Xero through a local browser callback", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, cmd *cobra.Command, _ *xero.Store, client *xero.Client) error {
			return client.Login(ctx, cmd.OutOrStdout())
		})})
	cmd.AddCommand(&cobra.Command{Use: "connections", Short: "List connected Xero organisations", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, cmd *cobra.Command, _ *xero.Store, client *xero.Client) error {
			connections, err := client.Connections(ctx)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(connections)
		})})
	var tenant string
	selectCmd := &cobra.Command{Use: "select", Short: "Select a connected organisation by tenant ID", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, cmd *cobra.Command, store *xero.Store, client *xero.Client) error {
			if tenant == "" {
				return errors.New("tenant-id is required")
			}
			if err := client.Select(ctx, tenant); err != nil {
				return err
			}
			status, err := store.Status(ctx)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(status)
		})}
	selectCmd.Flags().StringVar(&tenant, "tenant-id", "", "organisation tenant ID from xero connections")
	cmd.AddCommand(selectCmd)
	cmd.AddCommand(&cobra.Command{Use: "sync", Short: "Refresh the selected organisation's attachment targets", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, cmd *cobra.Command, _ *xero.Store, client *xero.Client) error {
			count, err := client.Sync(ctx)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]int{"targets": count})
		})})
	cmd.AddCommand(&cobra.Command{Use: "list", Short: "List cached targets for the selected organisation", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, cmd *cobra.Command, store *xero.Store, _ *xero.Client) error {
			targets, err := store.Targets(ctx)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(targets)
		})})
	cmd.AddCommand(&cobra.Command{Use: "status", Short: "Show local connection and last successful sync state", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, cmd *cobra.Command, store *xero.Store, _ *xero.Client) error {
			status, err := store.Status(ctx)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(status)
		})})
	return cmd
}
