// Package serviceconfig reads the shared service TOML configuration.
package serviceconfig

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml"
)

type File struct {
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

// Load strictly parses the file and returns a resolver for paths relative to it.
func Load(filename string) (File, func(string) string, error) {
	var config File
	file, err := os.Open(filename)
	if err != nil {
		return config, nil, fmt.Errorf("open service config: %w", err)
	}
	defer file.Close()
	if err := toml.NewDecoder(file).Strict(true).Decode(&config); err != nil {
		return config, nil, fmt.Errorf("parse service config: %w", err)
	}
	abs, err := filepath.Abs(filename)
	if err != nil {
		return config, nil, fmt.Errorf("resolve service config path: %w", err)
	}
	base := filepath.Dir(abs)
	resolve := func(value string) string {
		if value == "" || filepath.IsAbs(value) {
			return value
		}
		return filepath.Join(base, value)
	}
	return config, resolve, nil
}
