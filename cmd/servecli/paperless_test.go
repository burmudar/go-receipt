package servecli

import (
	"path/filepath"
	"testing"
)

func TestPaperlessConfig(t *testing.T) {
	filename := writeConfig(t, `[paperless]
url = "https://paperless.example"
token_file = "secrets/token"
tags = "7,12"
timeout = "45s"
`)
	cmd, database := configCommand(t, "--paperless-timeout", "10s")
	if err := applyConfig(cmd, filename, database); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"paperless-url": "https://paperless.example", "paperless-token-file": filepath.Join(filepath.Dir(filename), "secrets/token"), "paperless-tags": "[7,12]", "paperless-timeout": "10s"} {
		if got := cmd.Flags().Lookup(name).Value.String(); got != want {
			t.Errorf("%s=%s want %s", name, got, want)
		}
	}
}
