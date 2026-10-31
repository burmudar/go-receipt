// Package intake discovers receipt files and reads their retained originals.
package intake

import (
	"context"
	"fmt"
	"iter"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/burmudar/go-receipt/receipt"
	"github.com/burmudar/go-receipt/remotefs"
)

// LocalFiles discovers and reads top-level regular files in a local intake folder.
func LocalFiles(ctx context.Context, dir string) (iter.Seq2[receipt.File, error], error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(absolute)
	if err != nil {
		return nil, fmt.Errorf("read receipt folder: %w", err)
	}

	return func(yield func(receipt.File, error) bool) {
		for _, entry := range entries {
			if ctx.Err() != nil {
				yield(receipt.File{}, ctx.Err())
				return
			}

			// Do not follow symlinks or read directories and special files.
			if !entry.Type().IsRegular() {
				continue
			}

			filename := filepath.Join(absolute, entry.Name())
			data, err := os.ReadFile(filename)
			file := receipt.File{
				Name:   entry.Name(),
				Source: filename,
				Data:   data,
			}
			if !yield(file, err) {
				return
			}
		}
	}, nil
}

// LocalSource reads persisted local intake locations.
type LocalSource struct{}

func (LocalSource) Read(ctx context.Context, location string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(location) {
		return nil, fmt.Errorf("%w: WebDAV source", receipt.ErrSourceUnconfigured)
	}
	return os.ReadFile(location)
}

func parseWebDAVURL(endpoint string) (*url.URL, error) {
	u, err := url.Parse(endpoint)
	if err != nil ||
		u.Host == "" ||
		(u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil ||
		u.RawQuery != "" ||
		u.Fragment != "" ||
		strings.Contains(u.Path, "\\") ||
		(u.Path != "" && u.Path != "/" && path.Clean(u.Path) != strings.TrimSuffix(u.Path, "/")) {
		return nil, fmt.Errorf("WebDAV URL must be an HTTP(S) URL without credentials, query, or fragment")
	}

	return u, nil
}

// WebDAVInput discovers and reads originals using a fixed endpoint and credentials.
// It is immutable and safe for concurrent background and preview requests.
type WebDAVInput struct {
	endpoint string
	user     string
	password string
	inbox    string
}

// NewWebDAVInput validates the endpoint, credentials, and relative inbox path.
func NewWebDAVInput(endpoint, user, password, inbox string) (*WebDAVInput, error) {
	if _, err := parseWebDAVURL(endpoint); err != nil {
		return nil, err
	}
	if strings.TrimSpace(user) == "" || password == "" {
		return nil, fmt.Errorf("WebDAV username and password are required")
	}
	if err := validateInbox(inbox); err != nil {
		return nil, err
	}
	return &WebDAVInput{endpoint: endpoint, user: user, password: password, inbox: inbox}, nil
}

// Files discovers the WebDAV inbox and reads the original file bytes.
func (d *WebDAVInput) Files(ctx context.Context) (iter.Seq2[receipt.File, error], error) {
	return webDAVFiles(ctx, d.endpoint, d.user, d.password, d.inbox)
}

// Read retrieves a persisted source reference under the configured endpoint.
func (d *WebDAVInput) Read(ctx context.Context, location string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	remotePath, err := webDAVRelativePath(d.endpoint, location)
	if err != nil {
		return nil, err
	}
	return readWebDAV(ctx, d.endpoint, d.user, d.password, remotePath)
}

// DiscoverSources discovers each configured intake independently; a failure in one
// source does not prevent the other from being scanned.
func DiscoverSources(ctx context.Context, folder string, webDAV *WebDAVInput) (iter.Seq2[receipt.File, error], error) {
	return func(yield func(receipt.File, error) bool) {
		if folder != "" {
			files, err := LocalFiles(ctx, folder)
			if err != nil {
				if !yield(receipt.File{}, err) {
					return
				}
			} else {
				for file, err := range files {
					if !yield(file, err) {
						return
					}
				}
			}
		}
		if webDAV != nil && ctx.Err() == nil {
			files, err := webDAV.Files(ctx)
			if err != nil {
				yield(receipt.File{}, err)
				return
			}
			for file, err := range files {
				if !yield(file, err) {
					return
				}
			}
		}
	}, nil
}

// MixedSource routes local and WebDAV references without following arbitrary URLs.
type MixedSource struct {
	WebDAV *WebDAVInput
}

// Read selects the local or WebDAV source for a persisted location.
func (s MixedSource) Read(ctx context.Context, location string) ([]byte, error) {
	if filepath.IsAbs(location) {
		return (LocalSource{}).Read(ctx, location)
	}
	if s.WebDAV == nil {
		return nil, fmt.Errorf("%w: WebDAV", receipt.ErrSourceUnconfigured)
	}
	return s.WebDAV.Read(ctx, location)
}

func validateInbox(inbox string) error {
	if inbox == "" || inbox == "." || strings.HasPrefix(inbox, "/") ||
		strings.Contains(inbox, "\\") || path.Clean(inbox) != inbox ||
		inbox == ".." || strings.HasPrefix(inbox, "../") {
		return fmt.Errorf("WebDAV inbox must be a relative path within the endpoint")
	}
	return nil
}

func webDAVRelativePath(endpoint, location string) (string, error) {
	source, err := parseWebDAVURL(location)
	if err != nil {
		return "", err
	}
	base, err := parseWebDAVURL(endpoint)
	if err != nil {
		return "", err
	}
	prefix := strings.TrimRight(base.Path, "/") + "/"
	relative, ok := strings.CutPrefix(source.Path, prefix)
	if source.Scheme != base.Scheme || source.Host != base.Host || !ok ||
		relative == "" || path.Clean(relative) != relative || strings.Contains(relative, "\\") {
		return "", fmt.Errorf("%w: receipt source is outside the configured WebDAV endpoint", receipt.ErrSourceUnconfigured)
	}
	return relative, nil
}

func readWebDAV(ctx context.Context, endpoint, user, password, relative string) ([]byte, error) {
	dir, err := remotefs.NewDirContext(ctx, user, password, endpoint)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: could not connect to WebDAV", receipt.ErrSourceUnavailable)
	}
	file, err := dir.Download(relative)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: could not read WebDAV receipt", receipt.ErrSourceUnavailable)
	}
	return file.Data, nil
}

func webDAVFiles(ctx context.Context, endpoint, user, password, inbox string) (iter.Seq2[receipt.File, error], error) {
	u, err := parseWebDAVURL(endpoint)
	if err != nil {
		return nil, err
	}

	if err := validateInbox(inbox); err != nil {
		return nil, err
	}
	dir, err := remotefs.NewDirContext(ctx, user, password, endpoint)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: could not connect to WebDAV", receipt.ErrSourceUnavailable)
	}

	return func(yield func(receipt.File, error) bool) {
		for info, err := range dir.List(inbox) {
			if err != nil {
				if ctx.Err() != nil {
					yield(receipt.File{}, ctx.Err())
					return
				}
				yield(receipt.File{}, fmt.Errorf("%w: could not list WebDAV inbox", receipt.ErrSourceUnavailable))
				return
			}
			if ctx.Err() != nil {
				yield(receipt.File{}, ctx.Err())
				return
			}

			if !info.Mode().IsRegular() {
				continue
			}

			if info.Name() == "." || info.Name() == ".." || strings.ContainsAny(info.Name(), "/\\") {
				if !yield(receipt.File{}, fmt.Errorf("invalid WebDAV file name")) {
					return
				}
				continue
			}
			remotePath := path.Join(inbox, info.Name())
			// JoinPath expects escaped path elements. Receipt names are decoded
			// by the WebDAV client, so escape literal percent signs first.
			relativeURL := &url.URL{Path: remotePath}
			source := u.JoinPath(relativeURL.EscapedPath()).String()
			file, err := dir.Download(remotePath)
			r := receipt.File{
				Name:   info.Name(),
				Source: source,
			}
			if err == nil {
				r.Data = file.Data
			} else if ctx.Err() != nil {
				err = ctx.Err()
			} else {
				err = fmt.Errorf("%w: could not download WebDAV receipt", receipt.ErrSourceUnavailable)
			}

			if !yield(r, err) {
				return
			}
		}
	}, nil
}
