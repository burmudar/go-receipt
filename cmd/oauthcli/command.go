package oauthcli

import (
	"encoding/json"
	"slices"

	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/db/oauth"
	"github.com/spf13/cobra"
)

// Cmd manages OAuth client credentials. database points to the root --db flag value.
func Cmd(database *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "oauth",
		Short: "Manage OAuth client credentials",
		Long: `Manage OAuth client credentials.

The Xero client credentials are a setup step. Connect Xero in the browser with "rcpt serve".`,
	}

	cmd.AddCommand(setSubCmd(database), getSubCmd(database))
	return cmd
}

func setSubCmd(database *string) *cobra.Command {
	args := OAuthArgs{}
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Set the Xero OAuth client credentials",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			}
			return oauth.NewStore(conn).Create(&m)
		},
	}
	cmd.Flags().StringVar(&args.ClientID, "client-id", "", "oauth client id")
	cmd.Flags().StringVar(&args.ClientSecret, "client-secret", "", "oauth client secret")
	return cmd
}

func getSubCmd(database *string) *cobra.Command {
	var name, clientID string
	cmd := &cobra.Command{
		Use:   "get",
		Short: "List OAuth clients without secrets, optionally filtered by name and client ID",
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
