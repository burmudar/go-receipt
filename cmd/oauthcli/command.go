package oauthcli

import (
	"encoding/json"
	"os"
	"slices"

	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/db/oauth"
	"github.com/spf13/cobra"
)

func Cmd() *cobra.Command {
	var database string
	cmd := &cobra.Command{Use: "oauth", Short: "Configure OAuth client credentials"}
	cmd.PersistentFlags().StringVar(&database, "db", "app.db", "migrated SQLite database")
	cmd.AddCommand(setSubCmd(&database), getSubCmd(&database))
	return cmd
}

func setSubCmd(database *string) *cobra.Command {
	args := OAuthArgs{}
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Set OAuth client credentials",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if secret := os.Getenv("XERO_CLIENT_SECRET"); secret != "" {
				args.ClientSecret = secret
			}
			if err := args.Validate(); err != nil {
				return err
			}

			conn, err := db.Open(*database)
			if err != nil {
				return err
			}
			defer conn.Close()

			m := oauth.OAuthClientModel{
				Name:         "Xero",
				ClientID:     args.ClientID,
				ClientSecret: args.ClientSecret,
				RedirectURI:  args.RedirectURI,
			}
			return oauth.NewStore(conn).Create(&m)
		},
	}
	cmd.Flags().StringVar(&args.ClientID, "client-id", "", "oauth client id")
	cmd.Flags().StringVar(&args.ClientSecret, "client-secret", "", "oauth client secret (prefer XERO_CLIENT_SECRET)")
	cmd.Flags().StringVar(&args.RedirectURI, "redirect-uri", "http://localhost:11211/callback", "registered loopback OAuth redirect URI")
	return cmd
}

func getSubCmd(database *string) *cobra.Command {
	var name, clientID string
	cmd := &cobra.Command{
		Use:   "get",
		Short: "List OAuth clients, optionally filtered by name and client ID",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			conn, err := db.Open(*database)
			if err != nil {
				return err
			}
			defer conn.Close()

			clients, err := oauth.NewStore(conn).List()
			if err != nil {
				return err
			}
			clients = slices.DeleteFunc(clients, func(client *oauth.OAuthClientModel) bool {
				return (cmd.Flags().Changed("name") && client.Name != name) ||
					(cmd.Flags().Changed("client-id") && client.ClientID != clientID)
			})
			return json.NewEncoder(cmd.OutOrStdout()).Encode(clients)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "filter by exact OAuth client name")
	cmd.Flags().StringVar(&clientID, "client-id", "", "filter by exact OAuth client ID")
	return cmd
}
