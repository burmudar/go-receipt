package servecli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"iter"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/burmudar/go-receipt/cmd/scancli"
	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/receipt"
	"github.com/burmudar/go-receipt/server"
	"github.com/spf13/cobra"
)

// Cmd runs the receipt service. database points to the root --db flag value.
func Cmd(database *string) *cobra.Command {
	var listen, publicURL, passwordFile string
	var requestTimeout, xeroTimeout, checkInterval time.Duration
	var folder, apiKeyFile, model string
	var receiptInterval, transactionInterval, extractionTimeout time.Duration

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the receipt service and its browser UI",
		Long: `Run the receipt service and its browser UI.

The service listens on localhost by default. To use it from other devices on the
home network, put it behind an HTTPS reverse proxy such as Caddy. Set --public-url
to the proxy address and set a password with RCPT_PASSWORD or --password-file.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The flags are valid. Do not print usage for runtime errors.
			cmd.SilenceUsage = true

			if publicURL == "" {
				// Xero accepts http redirect URIs only for localhost.
				_, port, err := net.SplitHostPort(listen)
				if err != nil {
					return fmt.Errorf("parse --listen: %w", err)
				}
				publicURL = "http://localhost:" + port
			}
			public, err := url.Parse(publicURL)
			if err != nil {
				return fmt.Errorf("parse --public-url: %w", err)
			}

			required := server.AuthRequired(listen, public)
			password, err := readPassword(passwordFile, required)
			if err != nil {
				return err
			}

			conn, err := db.Open(*database)
			if err != nil {
				return err
			}
			defer conn.Close()

			if receiptInterval <= 0 || transactionInterval <= 0 {
				return fmt.Errorf("worker intervals must be positive")
			}
			var discover func(context.Context) (iter.Seq2[receipt.File, error], error)
			var scanner receipt.Scanner
			if folder != "" {
				key, err := readSecret(apiKeyFile)
				if err != nil {
					return err
				}
				if extractionTimeout <= 0 || strings.TrimSpace(model) == "" {
					return fmt.Errorf("extraction timeout must be positive and model must not be empty")
				}
				openai, err := scancli.NewReceiptScanner(key, model)
				if err != nil {
					return err
				}
				scanner = timedScanner{openai, extractionTimeout}
				discover = func(ctx context.Context) (iter.Seq2[receipt.File, error], error) {
					return scancli.LocalFiles(ctx, folder)
				}
			}
			srv, err := server.New(server.Config{
				DB:                  conn,
				Listen:              listen,
				PublicURL:           public,
				Password:            password,
				RequestTimeout:      requestTimeout,
				XeroTimeout:         xeroTimeout,
				CheckInterval:       checkInterval,
				Discover:            discover,
				Source:              scancli.LocalSource{},
				Scanner:             scanner,
				ReceiptInterval:     receiptInterval,
				TransactionInterval: transactionInterval,
			})
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return srv.Run(ctx)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&folder, "dir", "", "Local receipt folder to import in the background")
	flags.StringVar(&apiKeyFile, "api-key-file", ".openai-password", "OpenAI key file; OPENAI_API_KEY takes precedence")
	flags.StringVar(&model, "model", "gpt-5.5", "OpenAI extraction model")
	flags.DurationVar(&extractionTimeout, "extraction-timeout", 2*time.Minute, "Timeout per receipt extraction")
	flags.DurationVar(&receiptInterval, "receipt-interval", time.Minute, "Time between receipt import runs")
	flags.DurationVar(&transactionInterval, "transaction-interval", 15*time.Minute, "Time between Xero transaction refreshes")
	flags.StringVar(&listen, "listen", "127.0.0.1:11211", "TCP address for the HTTP server")
	flags.StringVar(&publicURL, "public-url", "", "URL that browsers use, for example the Caddy HTTPS address (default http://localhost:<listen port>)")
	flags.StringVar(&passwordFile, "password-file", ".rcpt-password", "UI password file; RCPT_PASSWORD takes precedence")
	flags.DurationVar(&requestTimeout, "request-timeout", time.Minute, "Read and write timeout for each incoming request")
	flags.DurationVar(&xeroTimeout, "xero-timeout", 20*time.Second, "Timeout for each request to Xero")
	flags.DurationVar(&checkInterval, "check-interval", 24*time.Hour, "Time between background Xero connection checks; must be less than 60 days")
	return cmd
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

func readSecret(filename string) (string, error) {
	value := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if value != "" {
		return value, nil
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return "", fmt.Errorf("set OPENAI_API_KEY or provide a readable secret file: %w", err)
	}
	value = strings.TrimSpace(string(data))
	if value == "" {
		return "", fmt.Errorf("OpenAI key file is empty")
	}
	return value, nil
}

// readPassword returns an empty password when authentication is not required and no password is set.
func readPassword(filename string, required bool) (string, error) {
	if value := strings.TrimSpace(os.Getenv("RCPT_PASSWORD")); value != "" {
		return value, nil
	}
	data, err := os.ReadFile(filename)
	if errors.Is(err, fs.ErrNotExist) && !required {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("set RCPT_PASSWORD or provide a readable password file, because the service is reachable from other devices: %w", err)
	}
	value := strings.TrimSpace(string(data))
	if value == "" && required {
		return "", fmt.Errorf("the password file %s is empty", filename)
	}
	return value, nil
}
