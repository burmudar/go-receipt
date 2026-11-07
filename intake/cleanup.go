package intake

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func verifyCleanup(data []byte, hash string) error {
	if fmt.Sprintf("%x", sha256.Sum256(data)) != hash {
		return fmt.Errorf("intake content changed")
	}
	return nil
}

// Cleanup rejects symlinks and verifies the opened regular file and pathname.
// The final SameFile check narrows, but cannot close, the unlink replacement race.
func (LocalSource) Cleanup(ctx context.Context, location, hash string, beforeDelete func() error) error {
	if !filepath.IsAbs(location) || filepath.Clean(location) != location {
		return fmt.Errorf("invalid local source")
	}
	for p := location; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink source refused")
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	f, err := os.Open(location)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	if err = verifyCleanup(data, hash); err != nil {
		return err
	}
	current, err := os.Lstat(location)
	if err != nil {
		return err
	}
	if !current.Mode().IsRegular() || !os.SameFile(info, current) {
		return fmt.Errorf("source replaced")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = beforeDelete(); err != nil {
		return err
	}
	return os.Remove(location)
}

// Cleanup intentionally does not use advisory LOCK or conditional headers:
// the pinned Copyparty does not enforce them as replacement protection.
func (d *WebDAVInput) Cleanup(ctx context.Context, location, hash string, beforeDelete func() error) error {
	relative, err := webDAVRelativePath(d.endpoint, location)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(relative, d.inbox+"/") || strings.Contains(strings.TrimPrefix(relative, d.inbox+"/"), "/") {
		return fmt.Errorf("source outside intake")
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	request := func(method string) (*http.Response, error) {
		u, e := url.Parse(location)
		if e != nil {
			return nil, e
		}
		req, e := http.NewRequestWithContext(ctx, method, u.String(), nil)
		if e != nil {
			return nil, e
		}
		req.SetBasicAuth(d.user, d.password)
		return client.Do(req)
	}
	resp, err := request(http.MethodGet)
	if err != nil {
		return fmt.Errorf("verify WebDAV source failed")
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 10_000_001))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || readErr != nil || len(data) > 10_000_000 {
		return fmt.Errorf("verify WebDAV source failed")
	}
	if err = verifyCleanup(data, hash); err != nil {
		return err
	}
	if err = beforeDelete(); err != nil {
		return err
	}
	resp, err = request(http.MethodDelete)
	if err != nil {
		return fmt.Errorf("WebDAV deletion outcome uncertain")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("WebDAV deletion not confirmed")
	}
	return nil
}

func (s MixedSource) Cleanup(ctx context.Context, location, hash string, beforeDelete func() error) error {
	if filepath.IsAbs(location) {
		return (LocalSource{}).Cleanup(ctx, location, hash, beforeDelete)
	}
	if s.WebDAV == nil {
		return fmt.Errorf("WebDAV not configured")
	}
	return s.WebDAV.Cleanup(ctx, location, hash, beforeDelete)
}
