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
}

// Cmd imports and extracts receipts. database points to the root --db flag value.
func Cmd(database *string) *cobra.Command {
	cfg := config{database: database}

	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Import receipts and persist structured extraction results",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
	persistentFlags.StringVar(
		&cfg.passwordFile, "webdav-password-file", ".storage-password",
		"WebDAV password file; WEBDAV_PASSWORD takes precedence",
	)

	flags := cmd.Flags()
	flags.StringVar(&cfg.folder, "dir", "", "Local receipt folder (top-level regular files only)")
	flags.StringVar(&cfg.inbox, "webdav-inbox", "inbox", "WebDAV inbox path")
	flags.BoolVar(&cfg.importOnly, "import-only", false, "Record source locations and hashes without contacting OpenAI")
	cmd.MarkFlagsMutuallyExclusive("dir", "webdav-url")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List persisted receipt status, analysis, and errors as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
		token, err := secret("OPENAI_API_KEY", c.apiKeyFile)
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
	password, err := secret("WEBDAV_PASSWORD", r.config.passwordFile)
	if err != nil {
		return nil, fmt.Errorf("%w: WebDAV credentials", receipt.ErrSourceUnconfigured)
	}
	input, err := intake.NewWebDAVInput(r.config.endpoint, r.config.user, password, r.config.inbox)
	if err != nil {
		return nil, err
	}
	return input.Read(ctx, location)
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
