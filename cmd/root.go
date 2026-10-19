package cmd

import (
	"os"

	"github.com/burmudar/go-receipt/cmd/oauthcli"
	"github.com/burmudar/go-receipt/cmd/scancli"
	"github.com/burmudar/go-receipt/cmd/servecli"
	"github.com/spf13/cobra"
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "rcpt",
	Short: "receipt management",
	Long:  `manage receipt server`,
}

// database is the SQLite database path for every command.
var database string

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVar(&database, "db", "app.db", "SQLite database (apply migrations first)")
	rootCmd.AddCommand(oauthcli.Cmd(&database), scancli.Cmd(&database), servecli.Cmd(&database))
}
