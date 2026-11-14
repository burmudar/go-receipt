package scancli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/receipt"
	"github.com/pressly/goose/v3"
	"github.com/spf13/cobra"
)

func TestRetryConfig(t *testing.T) {
	dir := t.TempDir()
	filename := filepath.Join(dir, "config.toml")
	data := `[server]
database = "app.db"
[openai]
api_key_file = "key"
model = "configured-model"
timeout = "3m"
[webdav]
url = "https://example.com/dav"
username = "user"
password_file = "password"
inbox = "receipts"
[paperless]
token_file = "paperless-token"
`
	if err := os.WriteFile(filename, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key"), []byte("file-key"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "env-key")

	for _, override := range []bool{false, true} {
		database := "default.db"
		root := &cobra.Command{Use: "test"}
		root.PersistentFlags().StringVar(&database, "db", database, "")
		scan := Cmd(&database)
		root.AddCommand(scan)
		retry, _, err := root.Find([]string{"scan", "retry"})
		if err != nil {
			t.Fatal(err)
		}
		args := []string{"--config", filename}
		if override {
			args = append(args, "--db", "override.db", "--model", "override-model", "--timeout", "4m")
		}
		if err := retry.ParseFlags(args); err != nil {
			t.Fatal(err)
		}
		c := config{database: &database, configFile: filename}
		if err := c.applyConfig(retry); err != nil {
			t.Fatal(err)
		}
		wantDB, wantModel, wantTimeout := filepath.Join(dir, "app.db"), "configured-model", 3*time.Minute
		if override {
			wantDB, wantModel, wantTimeout = "override.db", "override-model", 4*time.Minute
		}
		if database != wantDB || retry.Flag("model").Value.String() != wantModel || retry.Flag("timeout").Value.String() != wantTimeout.String() {
			t.Fatalf("wrong config: db=%q model=%s timeout=%s", database, retry.Flag("model").Value, retry.Flag("timeout").Value)
		}
		if got := retry.Flag("webdav-password-file").Value.String(); got != filepath.Join(dir, "password") {
			t.Fatalf("password path: %q", got)
		}
		if got := retry.Flag("webdav-inbox").Value.String(); got != "receipts" {
			t.Fatalf("inbox: %q", got)
		}
		key, err := c.readSecret("OPENAI_API_KEY", retry.Flag("api-key-file").Value.String())
		if err != nil || key != "file-key" {
			t.Fatalf("file secret: %q %v", key, err)
		}
	}
}

func TestRetryCommandWithConfig(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "configured.db")
	conn, err := db.Open(dbPath)
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
	if err := os.WriteFile(filepath.Join(dir, "key"), []byte("test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(filename, []byte("[server]\ndatabase = \"configured.db\"\n[openai]\napi_key_file = \"key\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(dir, "wrong.db")
	root := &cobra.Command{Use: "test"}
	root.PersistentFlags().StringVar(&database, "db", database, "")
	root.AddCommand(Cmd(&database))
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetArgs([]string{"scan", "retry", "--config", filename})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if database != dbPath || !strings.Contains(output.String(), `"extracted":0`) {
		t.Fatalf("db=%q output=%q", database, output.String())
	}
}

func TestRetryRuntimeFailureDoesNotPrintUsage(t *testing.T) {
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
	_, _, err = receipt.NewStore(conn).Import(context.Background(), receipt.File{
		Name: "missing.pdf", Source: filepath.Join(dir, "missing.pdf"), Data: []byte("receipt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "test-key")
	root := &cobra.Command{Use: "test"}
	root.PersistentFlags().StringVar(&database, "db", database, "")
	root.AddCommand(Cmd(&database))
	var out, stderr bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&stderr)
	root.SetArgs([]string{"scan", "retry"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected receipt processing error")
	}
	if !strings.Contains(out.String(), `"failed":1`) || !strings.Contains(stderr.String(), "missing.pdf") {
		t.Fatalf("missing summary or error: stdout=%q stderr=%q", out.String(), stderr.String())
	}
	if strings.Contains(out.String()+stderr.String(), "Usage:") {
		t.Fatalf("runtime error printed usage: stdout=%q stderr=%q", out.String(), stderr.String())
	}
}

func TestRetryInvalidFlagPrintsUsage(t *testing.T) {
	database := "unused.db"
	root := &cobra.Command{Use: "test"}
	root.AddCommand(Cmd(&database))
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"scan", "retry", "--not-a-flag"})
	if err := root.Execute(); err == nil || !strings.Contains(output.String(), "Usage:") {
		t.Fatalf("expected usage for invalid flag: err=%v output=%q", err, output.String())
	}
}

func TestRetryConfigErrors(t *testing.T) {
	for _, tc := range []struct{ content, want string }{
		{"[openai]\ntimeout = \"invalid\"\n", "invalid config for --timeout"},
		{"[openai]\nunexpected = \"value\"\n", "parse service config"},
	} {
		dir := t.TempDir()
		filename := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(filename, []byte(tc.content), 0600); err != nil {
			t.Fatal(err)
		}
		database := "default.db"
		root := &cobra.Command{Use: "test"}
		root.PersistentFlags().StringVar(&database, "db", database, "")
		root.AddCommand(Cmd(&database))
		retry, _, _ := root.Find([]string{"scan", "retry"})
		if err := retry.ParseFlags(nil); err != nil {
			t.Fatal(err)
		}
		c := config{database: &database, configFile: filename}
		if err := c.applyConfig(retry); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("got %v, want %s", err, tc.want)
		}
	}
}
