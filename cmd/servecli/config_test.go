package servecli

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/server"
	"github.com/pressly/goose/v3"
	"github.com/spf13/cobra"
)

func configCommand(t *testing.T, args ...string) (*cobra.Command, *string) {
	t.Helper()
	database := "app.db"
	root := &cobra.Command{Use: "rcpt"}
	root.PersistentFlags().StringVar(&database, "db", "app.db", "database")
	cmd := Cmd(&database)
	root.AddCommand(cmd)
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	return cmd, &database
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "service.toml")
	if err := os.WriteFile(filename, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestApplyConfigAndFlagPrecedence(t *testing.T) {
	filename := writeConfig(t, `[server]
database = "state/app.db"
listen = "127.0.0.1:1234"
public_url = "http://localhost:1234"
password_file = "secrets/ui"
request_timeout = "31s"
xero_timeout = "11s"
check_interval = "12h"
[receipts]
directory = "inbox"
interval = "2m"
[openai]
api_key_file = "secrets/openai"
model = "model-config"
timeout = "3m"
[webdav]
url = "https://example.test/dav/"
username = "user"
password_file = "secrets/webdav"
inbox = "incoming"
[xero]
transaction_interval = "30m"
`)
	cmd, database := configCommand(t, "--listen", "127.0.0.1:9999", "--api-key-file", "override-key", "--db", "override.db")
	if err := applyConfig(cmd, filename, database); err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(filename)
	want := map[string]string{
		"listen": "127.0.0.1:9999", "public-url": "http://localhost:1234", "password-file": filepath.Join(base, "secrets/ui"),
		"request-timeout": "31s", "xero-timeout": "11s", "check-interval": "12h0m0s",
		"dir": filepath.Join(base, "inbox"), "receipt-interval": "2m0s", "api-key-file": "override-key",
		"model": "model-config", "extraction-timeout": "3m0s", "webdav-url": "https://example.test/dav/",
		"webdav-user": "user", "webdav-password-file": filepath.Join(base, "secrets/webdav"),
		"webdav-inbox": "incoming", "transaction-interval": "30m0s",
	}
	for name, expected := range want {
		if got := cmd.Flags().Lookup(name).Value.String(); got != expected {
			t.Errorf("--%s = %q, want %q", name, got, expected)
		}
	}
	if *database != "override.db" {
		t.Fatalf("--db not respected: %q", *database)
	}
	cmd, database = configCommand(t)
	if err := applyConfig(cmd, filename, database); err != nil {
		t.Fatal(err)
	}
	if *database != filepath.Join(base, "state/app.db") {
		t.Fatalf("config database path: %q", *database)
	}
}

func TestConfigRejectsInvalidAndInlineSecrets(t *testing.T) {
	for _, content := range []string{
		"[webdav]\npassword = 'not-allowed'\n",
		"[openai]\napi_key = 'not-allowed'\n",
		"[server]\nlisten = 123\n",
		"[server]\nrequest_timeout = 'never'\n",
		"[unknown]\nvalue = 'x'\n",
	} {
		t.Run(strings.ReplaceAll(content, "\n", "_"), func(t *testing.T) {
			cmd, database := configCommand(t)
			if err := applyConfig(cmd, writeConfig(t, content), database); err == nil {
				t.Fatal("expected invalid config error")
			}
		})
	}
}

func TestConfigModeRequiresSecretFiles(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "environment-key-must-not-be-used")
	t.Setenv("WEBDAV_PASSWORD", "environment-password-must-not-be-used")
	t.Setenv("RCPT_PASSWORD", "environment-ui-must-not-be-used")
	for _, tc := range []struct{ name, config, want string }{
		{"openai", "[receipts]\ndirectory = 'inbox'\n", "OpenAI API key file path is required"},
		{"webdav", "[webdav]\nurl = 'https://example.test/dav'\nusername = 'user'\n", "WebDAV password file path is required"},
		{"ui", "[server]\npublic_url = 'https://example.test'\n", "server.password_file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filename := writeConfig(t, tc.config)
			cmd, _ := configCommand(t, "--config", filename, "--db", filepath.Join(t.TempDir(), "app.db"))
			err := cmd.RunE(cmd, nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v; want %q", err, tc.want)
			}
		})
	}
}

func TestConfiguredServiceStartsAndShutsDown(t *testing.T) {
	dir := t.TempDir()
	database := filepath.Join(dir, "app.db")
	conn, err := db.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, conn, os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ui-password"), []byte("from-file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(dir, "config.toml")
	contents := "[server]\ndatabase = 'app.db'\nlisten = '" + address + "'\npassword_file = 'ui-password'\n"
	if err := os.WriteFile(filename, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RCPT_PASSWORD", "different-environment-password")
	cmd, dbPath := configCommand(t, "--config", filename)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd.SetContext(ctx)
	done := make(chan error, 1)
	go func() { done <- cmd.RunE(cmd, nil) }()
	client := &http.Client{Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	baseURL := "http://" + address
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(baseURL + "/login")
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("service did not start: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := client.PostForm(baseURL+"/login", url.Values{"password": {"from-file"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || len(resp.Cookies()) == 0 {
		t.Fatalf("file password login: %d", resp.StatusCode)
	}
	resp, err = client.PostForm(baseURL+"/login", url.Values{"password": {"different-environment-password"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("environment password unexpectedly accepted: %d", resp.StatusCode)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if *dbPath != database {
		t.Fatalf("service used database %q; want %q", *dbPath, database)
	}
}

func TestPublicURLRejectsCredentials(t *testing.T) {
	public, err := url.Parse("https://user:secret@example.test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = server.New(server.Config{Listen: "127.0.0.1:11211", PublicURL: public})
	if err == nil || !strings.Contains(err.Error(), "must not contain credentials") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("public URL validation: %v", err)
	}
}

func TestSecretFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(file, []byte("value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readFileSecret(file, "test secret")
	if err != nil || got != "value" {
		t.Fatalf("secret = %q, error = %v", got, err)
	}
	if _, err := readFileSecret("", "test secret"); err == nil {
		t.Fatal("empty path accepted")
	}
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readFileSecret(file, "test secret"); err == nil {
		t.Fatal("empty secret accepted")
	}
}
