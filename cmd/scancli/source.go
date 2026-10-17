package scancli

import (
	"context"
	"fmt"
	"iter"
	"net/url"
	"os"
	"path"
	"path/filepath"

	"github.com/burmudar/go-receipt/receipt"
	"github.com/burmudar/go-receipt/remotefs"
)

// Both input adapters yield the same receipt type. Processing and retries do not
// need to know which adapter supplied the original bytes.
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
			if !yield(receipt.File{Name: entry.Name(), Source: filename, Data: data}, err) {
				return
			}
		}
	}, nil
}

func webDAVFiles(ctx context.Context, endpoint, user, password, inbox string) (iter.Seq2[receipt.File, error], error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("WebDAV URL must be an HTTP(S) URL without credentials, query, or fragment")
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
			source := u.JoinPath(remotePath).String()
			file, err := dir.Download(remotePath)
			r := receipt.File{Name: info.Name(), Source: source}
			if err == nil {
				r.Data = file.Data
			}
			if !yield(r, err) {
				return
			}
		}
	}, nil
}
