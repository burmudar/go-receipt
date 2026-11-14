package servecli

import (
	"fmt"
	"github.com/burmudar/go-receipt/cmd/serviceconfig"
	"github.com/spf13/cobra"
)

// applyConfig sets flag values only when the user has not explicitly supplied
// that flag. Paths in TOML are relative to the TOML file, not the process cwd.
func applyConfig(cmd *cobra.Command, filename string, database *string) error {
	config, resolve, err := serviceconfig.Load(filename)
	if err != nil {
		return err
	}
	if flag := cmd.Flag("db"); config.Server.Database != nil && (flag == nil || !flag.Changed) {
		*database = resolve(*config.Server.Database)
	}
	// In TOML mode secrets come only from explicitly configured file paths.
	// Clear legacy default paths unless the user set a flag explicitly.
	for _, name := range []string{"password-file", "api-key-file", "webdav-password-file"} {
		flag := cmd.Flags().Lookup(name)
		if !flag.Changed {
			if err := flag.Value.Set(""); err != nil {
				return fmt.Errorf("reset --%s: %w", name, err)
			}
		}
	}
	settings := []struct {
		name  string
		value *string
		path  bool
	}{
		{"paperless-url", config.Paperless.URL, false},
		{"paperless-token-file", config.Paperless.TokenFile, true},
		{"paperless-tags", config.Paperless.Tags, false},
		{"paperless-timeout", config.Paperless.Timeout, false},
		{"listen", config.Server.Listen, false},
		{"public-url", config.Server.PublicURL, false},
		{"password-file", config.Server.PasswordFile, true},
		{"request-timeout", config.Server.RequestTimeout, false},
		{"xero-timeout", config.Server.XeroTimeout, false},
		{"check-interval", config.Server.CheckInterval, false},
		{"dir", config.Receipts.Directory, true},
		{"receipt-interval", config.Receipts.Interval, false},
		{"api-key-file", config.OpenAI.APIKeyFile, true},
		{"model", config.OpenAI.Model, false},
		{"extraction-timeout", config.OpenAI.Timeout, false},
		{"webdav-url", config.WebDAV.URL, false},
		{"webdav-user", config.WebDAV.Username, false},
		{"webdav-password-file", config.WebDAV.PasswordFile, true},
		{"webdav-inbox", config.WebDAV.Inbox, false},
		{"transaction-interval", config.Xero.TransactionInterval, false},
	}
	for _, setting := range settings {
		if setting.value == nil || cmd.Flags().Changed(setting.name) {
			continue
		}
		value := *setting.value
		if setting.path {
			value = resolve(value)
		}
		if err := cmd.Flags().Set(setting.name, value); err != nil {
			return fmt.Errorf("invalid config for --%s: %w", setting.name, err)
		}
	}
	return nil
}
