package servecli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml"
	"github.com/spf13/cobra"
)

// serviceFile is intentionally separate from runtime settings: it accepts only
// file paths for secrets, and durations use the same syntax as CLI flags.
type serviceFile struct {
	Paperless struct {
		URL       *string `toml:"url"`
		TokenFile *string `toml:"token_file"`
		Tags      *string `toml:"tags"`
		Timeout   *string `toml:"timeout"`
	} `toml:"paperless"`
	Server struct {
		Database       *string `toml:"database"`
		Listen         *string `toml:"listen"`
		PublicURL      *string `toml:"public_url"`
		PasswordFile   *string `toml:"password_file"`
		RequestTimeout *string `toml:"request_timeout"`
		XeroTimeout    *string `toml:"xero_timeout"`
		CheckInterval  *string `toml:"check_interval"`
	} `toml:"server"`
	Receipts struct {
		Directory *string `toml:"directory"`
		Interval  *string `toml:"interval"`
	} `toml:"receipts"`
	OpenAI struct {
		APIKeyFile *string `toml:"api_key_file"`
		Model      *string `toml:"model"`
		Timeout    *string `toml:"timeout"`
	} `toml:"openai"`
	WebDAV struct {
		URL          *string `toml:"url"`
		Username     *string `toml:"username"`
		PasswordFile *string `toml:"password_file"`
		Inbox        *string `toml:"inbox"`
	} `toml:"webdav"`
	Xero struct {
		TransactionInterval *string `toml:"transaction_interval"`
	} `toml:"xero"`
}

// applyConfig sets flag values only when the user has not explicitly supplied
// that flag. Paths in TOML are relative to the TOML file, not the process cwd.
func applyConfig(cmd *cobra.Command, filename string, database *string) error {
	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("open service config: %w", err)
	}
	defer file.Close()
	var config serviceFile
	if err := toml.NewDecoder(file).Strict(true).Decode(&config); err != nil {
		return fmt.Errorf("parse service config: %w", err)
	}
	abs, err := filepath.Abs(filename)
	if err != nil {
		return fmt.Errorf("resolve service config path: %w", err)
	}
	base := filepath.Dir(abs)
	resolve := func(value string) string {
		if value == "" || filepath.IsAbs(value) {
			return value
		}
		return filepath.Join(base, value)
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
