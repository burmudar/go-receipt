package intake

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestWebDAVCleanup(t *testing.T) {
	for _, scenario := range []string{"success", "mismatch", "missing", "missing-inbox", "read-forbidden", "redirect", "forbidden", "lost-response", "dispatch-failed", "outside", "replacement-race"} {
		t.Run(scenario, func(t *testing.T) {
			var mu sync.Mutex
			data := "original"
			deletes, dispatches := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				u, p, ok := r.BasicAuth()
				if !ok || u != "user" || p != "password" {
					t.Error("missing auth")
				}
				switch r.Method {
				case "GET":
					switch scenario {
					case "missing", "missing-inbox":
						w.WriteHeader(404)
						return
					case "read-forbidden":
						w.WriteHeader(http.StatusForbidden)
						return
					case "redirect":
						w.Header().Set("Location", "/elsewhere")
						w.WriteHeader(302)
						return
					case "mismatch":
						data = "replacement"
					}
					fmt.Fprint(w, data)
				case "HEAD":
					if scenario == "missing-inbox" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					w.WriteHeader(http.StatusOK)
				case "DELETE":
					deletes++
					if scenario == "forbidden" {
						w.WriteHeader(403)
						return
					}
					if scenario == "lost-response" {
						c, _, _ := w.(http.Hijacker).Hijack()
						c.Close()
						return
					}
					data = ""
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected %s", r.Method)
				}
			}))
			defer srv.Close()
			source, err := NewWebDAVInput(srv.URL, "user", "password", "inbox")
			if err != nil {
				t.Fatal(err)
			}
			location := srv.URL + "/inbox/receipt.pdf"
			if scenario == "outside" {
				location = srv.URL + "/other/receipt.pdf"
			}
			err = source.Cleanup(context.Background(), location, fmt.Sprintf("%x", sha256.Sum256([]byte("original"))), func() error {
				mu.Lock()
				defer mu.Unlock()
				dispatches++
				if scenario == "dispatch-failed" {
					return errors.New("database unavailable")
				}
				if scenario == "replacement-race" {
					data = "replacement"
				}
				return nil
			})
			mu.Lock()
			defer mu.Unlock()
			success := scenario == "success" || scenario == "replacement-race" || scenario == "missing"
			if (err == nil) != success {
				t.Fatalf("error %v", err)
			}
			wantDeletes := 0
			if scenario == "success" || scenario == "replacement-race" || scenario == "forbidden" || scenario == "lost-response" {
				wantDeletes = 1
			}
			if deletes != wantDeletes {
				t.Fatalf("deletes %d", deletes)
			}
			if scenario == "missing" && dispatches != 1 {
				t.Fatal("missing file requires authorization recheck")
			}
			if scenario == "replacement-race" && data != "" {
				t.Fatal("test must expose accepted race, not claim atomic safety")
			}
		})
	}
}
func TestLocalMissingParentDoesNotConfirmCleanup(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "missing-mount")
	location := filepath.Join(parent, "receipt.pdf")
	dispatched := false
	err := (LocalSource{}).Cleanup(context.Background(), location, "unused", func() error {
		dispatched = true
		return nil
	})
	if err == nil || dispatched {
		t.Fatalf("missing parent must not confirm absence: err=%v dispatched=%v", err, dispatched)
	}
}

func TestLocalCleanupSafeguards(t *testing.T) {
	for _, scenario := range []string{"symlink", "directory", "dispatch-failed", "replacement-race"} {
		t.Run(scenario, func(t *testing.T) {
			dir, resolveErr := filepath.EvalSymlinks(t.TempDir())
			if resolveErr != nil {
				t.Fatal(resolveErr)
			}
			path := filepath.Join(dir, "receipt.pdf")
			os.WriteFile(path, []byte("original"), 0600)
			if scenario == "symlink" {
				link := filepath.Join(dir, "link")
				os.Symlink(path, link)
				path = link
			}
			if scenario == "directory" {
				path = dir
			}
			err := (LocalSource{}).Cleanup(context.Background(), path, fmt.Sprintf("%x", sha256.Sum256([]byte("original"))), func() error {
				if scenario == "dispatch-failed" {
					return errors.New("no durable dispatch")
				}
				if scenario == "replacement-race" {
					replacement := filepath.Join(dir, "new")
					os.WriteFile(replacement, []byte("replacement"), 0600)
					return os.Rename(replacement, path)
				}
				t.Fatal("invalid source dispatched")
				return nil
			})
			if scenario == "replacement-race" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err = os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("expected documented race")
				}
			} else {
				if err == nil {
					t.Fatal("unsafe source accepted")
				}
				if _, err = os.Stat(path); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
