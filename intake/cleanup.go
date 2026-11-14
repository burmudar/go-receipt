package intake

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
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
	missing := false
	for p := location; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) && p == location {
			missing = true
			continue
		}
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
	if missing {
		// A missing mount or an inaccessible parent is not proof of absence.
		if _, err := os.ReadDir(filepath.Dir(location)); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return beforeDelete()
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
	request := func(method, target string) (*http.Response, error) {
		u, e := url.Parse(target)
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
	resp, err := request(http.MethodGet, location)
	if err != nil {
		return fmt.Errorf("verify WebDAV source failed")
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		// Confirm the configured inbox is reachable before treating 404 as absence.
		parent, err := url.Parse(location)
		if err != nil {
			return fmt.Errorf("invalid WebDAV source")
		}
		parent.Path = path.Dir(parent.Path) + "/"
		parent.RawPath = ""
		check, err := request(http.MethodHead, parent.String())
		if err != nil {
			return fmt.Errorf("verify WebDAV inbox failed")
		}
		check.Body.Close()
		if check.StatusCode < 200 || check.StatusCode >= 300 {
			return fmt.Errorf("verify WebDAV inbox returned HTTP %d", check.StatusCode)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return beforeDelete()
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 10_000_001))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || readErr != nil || len(data) > 10_000_000 {
		return fmt.Errorf("verify WebDAV source failed (HTTP %d)", resp.StatusCode)
	}
	if err = verifyCleanup(data, hash); err != nil {
		return err
	}
	if err = beforeDelete(); err != nil {
		return err
	}
	resp, err = request(http.MethodDelete, location)
	if err != nil {
		return fmt.Errorf("WebDAV deletion outcome uncertain")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("WebDAV deletion returned HTTP %d", resp.StatusCode)
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
