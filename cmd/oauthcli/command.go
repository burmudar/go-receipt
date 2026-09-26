package oauthcli

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/db/oauth"
	"github.com/spf13/cobra"
)

func Cmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "oauth",
		Short: "A brief description of your command",
		Long: `A longer description that spans multiple lines and likely contains examples
and usage of using your command. For example:

Cobra is a CLI library for Go that empowers applications.
This application is a tool to generate the needed files
to quickly create a Cobra application.`,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("oauth called")
		},
	}

	cmd.AddCommand(setSubCmd(), getSubCmd())
	return cmd
}

func setSubCmd() *cobra.Command {
	args := OAuthArgs{}
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Set OAuth client credentials",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := args.Validate(); err != nil {
				return err
			}

			conn, err := db.Open("app.db")
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
	cmd.Flags().StringVar(&args.ClientSecret, "client-secret", "", "oauth client secret")
	cmd.Flags().StringVar(&args.RedirectURI, "redirect-uri", "http://localhost:11211", "oauth redirect-uri")
	return cmd
}

func getSubCmd() *cobra.Command {
	var name, clientID string
	cmd := &cobra.Command{
		Use:   "get",
		Short: "List OAuth clients, optionally filtered by name and client ID",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			conn, err := db.Open("app.db")
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
