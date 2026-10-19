package servecli

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/server"
	"github.com/spf13/cobra"
)

// Cmd runs the receipt service. database points to the root --db flag value.
func Cmd(database *string) *cobra.Command {
	var listen, publicURL, passwordFile string
	var requestTimeout, xeroTimeout, checkInterval time.Duration

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

			srv, err := server.New(server.Config{
				DB:             conn,
				Listen:         listen,
				PublicURL:      public,
				Password:       password,
				RequestTimeout: requestTimeout,
				XeroTimeout:    xeroTimeout,
				CheckInterval:  checkInterval,
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
	flags.StringVar(&listen, "listen", "127.0.0.1:8080", "TCP address for the HTTP server")
	flags.StringVar(&publicURL, "public-url", "", "URL that browsers use, for example the Caddy HTTPS address (default http://localhost:<listen port>)")
	flags.StringVar(&passwordFile, "password-file", ".rcpt-password", "UI password file; RCPT_PASSWORD takes precedence")
	flags.DurationVar(&requestTimeout, "request-timeout", time.Minute, "Read and write timeout for each incoming request")
	flags.DurationVar(&xeroTimeout, "xero-timeout", 20*time.Second, "Timeout for each request to Xero")
	flags.DurationVar(&checkInterval, "check-interval", 24*time.Hour, "Time between background Xero connection checks; must be less than 60 days")
	return cmd
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
