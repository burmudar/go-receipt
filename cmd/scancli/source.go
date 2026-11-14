package scancli

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

// Both input adapters yield the same receipt type. SQLite stores their source
// locations and hashes; the intake retains the original files.
func localFiles(ctx context.Context, dir string) (iter.Seq2[receipt.File, error], error) {
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

// LocalFiles discovers top-level regular files in a local intake folder.
func LocalFiles(ctx context.Context, dir string) (iter.Seq2[receipt.File, error], error) {
	return localFiles(ctx, dir)
}

// LocalSource reads persisted local intake locations.
type LocalSource struct{}

func (LocalSource) Read(ctx context.Context, location string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(location) {
		return nil, fmt.Errorf("not a local receipt source")
	}
	return os.ReadFile(location)
}

// sourceReader reloads persisted source locations. WebDAV credentials are only
// sent to the configured endpoint, never to an arbitrary URL from the database.
type sourceReader struct {
	config config
	remote *remotefs.Dir
}

func (r *sourceReader) Read(ctx context.Context, location string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if filepath.IsAbs(location) {
		return os.ReadFile(location)
	}

	source, err := parseWebDAVURL(location)
	if err != nil {
		return nil, err
	}

	if r.config.endpoint == "" || r.config.user == "" {
		return nil, fmt.Errorf("set --webdav-url and --webdav-user to read this source")
	}

	endpoint, err := parseWebDAVURL(r.config.endpoint)
	if err != nil {
		return nil, err
	}

	basePath := strings.TrimRight(endpoint.Path, "/") + "/"
	remotePath, matchesPath := strings.CutPrefix(source.Path, basePath)
	if source.Scheme != endpoint.Scheme || source.Host != endpoint.Host || !matchesPath {
		return nil, fmt.Errorf("receipt source is outside the configured WebDAV endpoint")
	}

	if r.remote == nil {
		password, err := secret("WEBDAV_PASSWORD", r.config.passwordFile)
		if err != nil {
			return nil, err
		}

		r.remote, err = remotefs.NewDir(r.config.user, password, r.config.endpoint)
		if err != nil {
			return nil, fmt.Errorf("connect to WebDAV: %w", err)
		}
	}

	file, err := r.remote.Download(remotePath)
	if err != nil {
		return nil, err
	}

	return file.Data, nil
}

func parseWebDAVURL(endpoint string) (*url.URL, error) {
	u, err := url.Parse(endpoint)
	if err != nil ||
		u.Host == "" ||
		(u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil ||
		u.RawQuery != "" ||
		u.Fragment != "" {
		return nil, fmt.Errorf("WebDAV URL must be an HTTP(S) URL without credentials, query, or fragment")
	}

	return u, nil
}

func webDAVFiles(ctx context.Context, endpoint, user, password, inbox string) (iter.Seq2[receipt.File, error], error) {
	u, err := parseWebDAVURL(endpoint)
	if err != nil {
		return nil, err
	}

	dir, err := remotefs.NewDir(user, password, endpoint)
	if err != nil {
		return nil, fmt.Errorf("connect to WebDAV: %w", err)
	}

	return func(yield func(receipt.File, error) bool) {
		for info, err := range dir.List(inbox) {
			if err != nil {
				yield(receipt.File{}, fmt.Errorf("list WebDAV inbox: %w", err))
				return
			}
			if ctx.Err() != nil {
				yield(receipt.File{}, ctx.Err())
				return
			}

			if !info.Mode().IsRegular() {
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
			}

			if !yield(r, err) {
				return
			}
		}
	}, nil
}
