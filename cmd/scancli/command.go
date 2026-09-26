package scancli

import (
	"github.com/spf13/cobra"
)

func Cmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "scan receipts",
		Long:  "load receipts remotely over WebDAV and use an LLM to classify them into transactions",
		RunE: func(cmd *cobra.Command, args []string) error {
			return scan()
		},
	}

	return cmd
}
