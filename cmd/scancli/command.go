package scancli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/burmudar/go-receipt/cmd/serviceconfig"
	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/extract"
	"github.com/burmudar/go-receipt/intake"
	"github.com/burmudar/go-receipt/receipt"
	"github.com/spf13/cobra"
)

type config struct {
	database     *string
	folder       string
	endpoint     string
	user         string
	passwordFile string
	inbox        string
	apiKeyFile   string
	model        string
	timeout      time.Duration
	importOnly   bool
	configFile   string
	configured   bool
}

// Cmd imports and extracts receipts. database points to the root --db flag value.
func Cmd(database *string) *cobra.Command {
	cfg := config{database: database}

	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Import receipts and persist structured extraction results",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return cfg.run(cmd, false)
		},
	}

	persistentFlags := cmd.PersistentFlags()
	persistentFlags.StringVar(
		&cfg.apiKeyFile, "api-key-file", ".openai-password",
		"OpenAI key file; OPENAI_API_KEY takes precedence",
	)
	persistentFlags.StringVar(&cfg.model, "model", "gpt-5.5", "OpenAI extraction model")
	persistentFlags.DurationVar(&cfg.timeout, "timeout", 2*time.Minute, "Timeout per receipt extraction")

	persistentFlags.StringVar(&cfg.endpoint, "webdav-url", "", "WebDAV server URL")
	persistentFlags.StringVar(&cfg.user, "webdav-user", "", "WebDAV username")
	persistentFlags.StringVar(&cfg.inbox, "webdav-inbox", "inbox", "WebDAV inbox path")
	persistentFlags.StringVar(
		&cfg.passwordFile, "webdav-password-file", ".storage-password",
		"WebDAV password file; WEBDAV_PASSWORD takes precedence",
	)

	flags := cmd.Flags()
	flags.StringVar(&cfg.folder, "dir", "", "Local receipt folder (top-level regular files only)")
	flags.BoolVar(&cfg.importOnly, "import-only", false, "Record source locations and hashes without contacting OpenAI")
	cmd.MarkFlagsMutuallyExclusive("dir", "webdav-url")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List persisted receipt status, analysis, and errors as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			conn, err := db.Open(*cfg.database)
			if err != nil {
				return err
			}
			defer conn.Close()

			records, err := receipt.NewStore(conn).List(cmd.Context())
			if err != nil {
				return fmt.Errorf("list receipts (apply database migrations first): %w", err)
			}

			return json.NewEncoder(cmd.OutOrStdout()).Encode(records)
		},
	}

	retryCmd := &cobra.Command{
		Use:   "retry",
		Short: "Process pending and failed receipts from their retained intake files",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			if cmd.Flags().Changed("config") {
				if cfg.configFile == "" {
					return fmt.Errorf("--config must not be empty")
				}
				if err := cfg.applyConfig(cmd); err != nil {
					return err
				}
			}
			return cfg.run(cmd, true)
		},
	}
	retryCmd.Flags().StringVar(&cfg.configFile, "config", "", "TOML service configuration file")

	cmd.AddCommand(listCmd, retryCmd)
	return cmd
}

// applyConfig uses the service's receipt settings while preserving explicit CLI flags.
func (c *config) applyConfig(cmd *cobra.Command) error {
	cfg, resolve, err := serviceconfig.Load(c.configFile)
	if err != nil {
		return err
	}
	c.configured = true
	if flag := cmd.Flag("db"); cfg.Server.Database != nil && (flag == nil || !flag.Changed) {
		*c.database = resolve(*cfg.Server.Database)
	}
	for _, name := range []string{"api-key-file", "webdav-password-file"} {
		if flag := cmd.Flag(name); !flag.Changed {
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
		{"api-key-file", cfg.OpenAI.APIKeyFile, true},
		{"model", cfg.OpenAI.Model, false},
		{"timeout", cfg.OpenAI.Timeout, false},
		{"webdav-url", cfg.WebDAV.URL, false},
		{"webdav-user", cfg.WebDAV.Username, false},
		{"webdav-password-file", cfg.WebDAV.PasswordFile, true},
		{"webdav-inbox", cfg.WebDAV.Inbox, false},
	}
	for _, setting := range settings {
		flag := cmd.Flag(setting.name)
		if setting.value == nil || flag.Changed {
			continue
		}
		value := *setting.value
		if setting.path {
			value = resolve(value)
		}
		if err := flag.Value.Set(value); err != nil {
			return fmt.Errorf("invalid config for --%s: %w", setting.name, err)
		}
	}
	return nil
}

func secret(env, filename string) (string, error) {
	if value := strings.TrimSpace(os.Getenv(env)); value != "" {
		return value, nil
	}

	data, err := os.ReadFile(filename)
	if err != nil {
		return "", fmt.Errorf("set %s or provide a readable secret file: %w", env, err)
	}

	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", fmt.Errorf("set %s or provide a non-empty secret file", env)
	}

	return value, nil
}

func (c config) files(ctx context.Context) (iter.Seq2[receipt.File, error], error) {
	if c.folder != "" {
		return intake.LocalFiles(ctx, c.folder)
	}

	if c.endpoint == "" || c.user == "" {
		return nil, fmt.Errorf("set --dir for local receipts, or --webdav-url and --webdav-user")
	}

	password, err := secret("WEBDAV_PASSWORD", c.passwordFile)
	if err != nil {
		return nil, err
	}

	input, err := intake.NewWebDAVInput(c.endpoint, c.user, password, c.inbox)
	if err != nil {
		return nil, err
	}
	return input.Files(ctx)
}

type summary struct {
	Imported     int `json:"imported"`
	Duplicates   int `json:"duplicates"`
	ImportFailed int `json:"import_failed"`

	receipt.Progress
}

func (c config) run(cmd *cobra.Command, retry bool) error {
	if c.timeout <= 0 || strings.TrimSpace(c.model) == "" {
		return fmt.Errorf("--timeout must be positive and --model must not be empty")
	}

	conn, err := db.Open(*c.database)
	if err != nil {
		return err
	}
	defer conn.Close()

	store := receipt.NewStore(conn)
	ctx := cmd.Context()
	if _, err := store.List(ctx); err != nil {
		return fmt.Errorf("open receipt store (apply database migrations first): %w", err)
	}

	var result summary
	var failures []error

	if !retry {
		files, err := c.files(ctx)
		if err != nil {
			return err
		}

		for file, readErr := range files {
			if readErr != nil {
				result.ImportFailed++
				failures = append(failures, fmt.Errorf("read %s: %w", file.Source, readErr))
				continue
			}

			_, added, err := store.Import(ctx, file)
			if err != nil {
				result.ImportFailed++
				failures = append(failures, fmt.Errorf("import %s: %w", file.Source, err))
			} else if added {
				result.Imported++
			} else {
				result.Duplicates++
			}
		}
	}

	if !c.importOnly {
		token, err := c.readSecret("OPENAI_API_KEY", c.apiKeyFile)
		if err != nil {
			failures = append(failures, err)
		} else {
			scanner, err := extract.NewScanner(token, c.model)
			if err == nil {
				source := &sourceReader{config: c}
				extractor := timedScanner{
					Scanner: scanner,
					timeout: c.timeout,
				}
				result.Progress, err = store.Process(ctx, source, extractor, retry)
			}
			if err != nil {
				failures = append(failures, err)
			}
		}
	}

	if err := json.NewEncoder(cmd.OutOrStdout()).Encode(result); err != nil {
		failures = append(failures, err)
	}

	return errors.Join(failures...)
}

// sourceReader loads CLI credentials lazily, so local retries do not require WebDAV access.
type sourceReader struct {
	config config
}

func (r *sourceReader) Read(ctx context.Context, location string) ([]byte, error) {
	if filepath.IsAbs(location) {
		return (intake.LocalSource{}).Read(ctx, location)
	}
	if r.config.endpoint == "" || r.config.user == "" {
		return nil, fmt.Errorf("%w: set --webdav-url and --webdav-user", receipt.ErrSourceUnconfigured)
	}
	password, err := r.config.readSecret("WEBDAV_PASSWORD", r.config.passwordFile)
	if err != nil {
		return nil, fmt.Errorf("%w: WebDAV credentials", receipt.ErrSourceUnconfigured)
	}
	input, err := intake.NewWebDAVInput(r.config.endpoint, r.config.user, password, r.config.inbox)
	if err != nil {
		return nil, err
	}
	return input.Read(ctx, location)
}

// TOML mode, like serve, takes secrets only from configured files.
func (c config) readSecret(env, filename string) (string, error) {
	if !c.configured {
		return secret(env, filename)
	}
	if filename == "" {
		return "", fmt.Errorf("secret file path is required in the configuration or flags")
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return "", fmt.Errorf("read secret file: %w", err)
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", fmt.Errorf("secret file must not be empty")
	}
	return value, nil
}

type timedScanner struct {
	receipt.Scanner
	timeout time.Duration
}

func (s timedScanner) Scan(ctx context.Context, file receipt.File) (receipt.Analysis, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	return s.Scanner.Scan(ctx, file)
}
