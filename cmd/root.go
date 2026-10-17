package cmd

import (
	"os"

	"github.com/burmudar/go-receipt/cmd/oauthcli"
	"github.com/burmudar/go-receipt/cmd/scancli"
	"github.com/burmudar/go-receipt/cmd/xerocli"
	"github.com/spf13/cobra"
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "rcpt",
	Short: "receipt management",
	Long:  `manage receipt server`,
	// Uncomment the following line if your bare application
	// has an action associated with it:
	// Run: func(cmd *cobra.Command, args []string) { },
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.AddCommand(oauthcli.Cmd(), scancli.Cmd(), xerocli.Cmd())
}
