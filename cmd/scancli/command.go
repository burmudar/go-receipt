package scancli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"strings"
	"time"

	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/receipt"
	"github.com/spf13/cobra"
)

type config struct {
	database     string
	folder       string
	endpoint     string
	user         string
	passwordFile string
	inbox        string
	apiKeyFile   string
	model        string
	timeout      time.Duration
	importOnly   bool
}

func Cmd() *cobra.Command {
	cfg := config{}

	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Import receipts and persist structured extraction results",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cfg.run(cmd, false)
		},
	}

	persistentFlags := cmd.PersistentFlags()
	persistentFlags.StringVar(&cfg.database, "db", "app.db", "SQLite database (apply migrations first)")
	persistentFlags.StringVar(
		&cfg.apiKeyFile, "api-key-file", ".openai-password",
		"OpenAI key file; OPENAI_API_KEY takes precedence",
	)
	persistentFlags.StringVar(&cfg.model, "model", "gpt-5.5", "OpenAI extraction model")
	persistentFlags.DurationVar(&cfg.timeout, "timeout", 2*time.Minute, "Timeout per receipt extraction")

	flags := cmd.Flags()
	flags.StringVar(&cfg.folder, "dir", "", "Local receipt folder (top-level regular files only)")
	flags.StringVar(&cfg.endpoint, "webdav-url", "", "WebDAV server URL")
	flags.StringVar(&cfg.user, "webdav-user", "", "WebDAV username")
	flags.StringVar(
		&cfg.passwordFile, "webdav-password-file", ".storage-password",
		"WebDAV password file; WEBDAV_PASSWORD takes precedence",
	)
	flags.StringVar(&cfg.inbox, "webdav-inbox", "inbox", "WebDAV inbox path")
	flags.BoolVar(&cfg.importOnly, "import-only", false, "Save originals without contacting OpenAI")
	cmd.MarkFlagsMutuallyExclusive("dir", "webdav-url")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List persisted receipt status, analysis, and errors as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			conn, err := db.Open(cfg.database)
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
		Short: "Process pending and failed receipts from saved originals, without source access",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cfg.run(cmd, true)
		},
	}

	cmd.AddCommand(listCmd, retryCmd)
	return cmd
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
		return localFiles(ctx, c.folder)
	}

	if c.endpoint == "" || c.user == "" {
		return nil, fmt.Errorf("set --dir for local receipts, or --webdav-url and --webdav-user")
	}

	password, err := secret("WEBDAV_PASSWORD", c.passwordFile)
	if err != nil {
		return nil, err
	}

	return webDAVFiles(ctx, c.endpoint, c.user, password, c.inbox)
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

	conn, err := db.Open(c.database)
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
		token, err := secret("OPENAI_API_KEY", c.apiKeyFile)
		if err != nil {
			failures = append(failures, err)
		} else {
			scanner, err := NewReceiptScanner(token, c.model)
			if err == nil {
				result.Progress, err = store.Process(ctx, timedScanner{
					Scanner: scanner,
					timeout: c.timeout,
				}, retry)
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

type timedScanner struct {
	receipt.Scanner
	timeout time.Duration
}

func (s timedScanner) Scan(ctx context.Context, file receipt.File) (receipt.Analysis, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	return s.Scanner.Scan(ctx, file)
}
