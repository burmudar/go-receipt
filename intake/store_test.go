package intake

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/receipt"
	"github.com/pressly/goose/v3"
)

func fixture(t *testing.T) (string, string, string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "receipt.pdf")
	data := []byte("receipt")
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	return dir, name, fmt.Sprintf("%x", sha256.Sum256(data))
}

// Bound a real queued operation without sleeps. It must time out rather than
// reach its I/O callback while the snapshot owns protection.
func assertCleanupBlocked(t *testing.T, s *Store) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	entered := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() { done <- s.Cleanup(ctx, func() error { entered <- struct{}{}; return nil }) }()
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting cleanup: %v", err)
	}
	select {
	case <-entered:
		t.Fatal("cleanup entered active snapshot")
	default:
	}
}

func TestSnapshotProtectsListingAndReads(t *testing.T) {
	ctx := context.Background()
	dir, name, hash := fixture(t)
	var s Store
	err := s.WithSnapshot(ctx, func(ctx context.Context) (iter.Seq2[receipt.File, error], error) {
		// Protection starts before the actual listing, not after downloads.
		assertCleanupBlocked(t, &s)
		return DiscoverSources(ctx, dir, nil)
	}, func(files iter.Seq2[receipt.File, error], err error) error {
		if err != nil {
			return err
		}
		assertCleanupBlocked(t, &s)
		count := 0
		for f, err := range files {
			if err != nil {
				return err
			}
			if string(f.Data) != "receipt" {
				t.Errorf("unexpected bytes: %q", f.Data)
			}
			count++
		}
		if count != 1 {
			t.Errorf("got %d files", count)
		}
		// Protection also lasts after import while automatic processing reads sources.
		assertCleanupBlocked(t, &s)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Cleanup(ctx, func() error { return (LocalSource{}).Cleanup(ctx, name, hash, func() error { return nil }) }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("not removed: %v", err)
	}
}

func TestSnapshotWaitsForStartedCleanup(t *testing.T) {
	ctx := context.Background()
	dir, name, hash := fixture(t)
	var s Store
	entered, finish := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.Cleanup(ctx, func() error {
			return (LocalSource{}).Cleanup(ctx, name, hash, func() error { close(entered); <-finish; return nil })
		})
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("cleanup failed before dispatch: %v", err)
	}
	canceled, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := s.WithSnapshot(canceled, func(context.Context) (iter.Seq2[receipt.File, error], error) {
		t.Error("listing started during cleanup")
		return nil, nil
	}, func(iter.Seq2[receipt.File, error], error) error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	scanned := make(chan error, 1)
	go func() {
		scanned <- s.WithSnapshot(ctx, func(ctx context.Context) (iter.Seq2[receipt.File, error], error) {
			return DiscoverSources(ctx, dir, nil)
		}, func(files iter.Seq2[receipt.File, error], err error) error {
			if err != nil {
				return err
			}
			for _, err := range files {
				return fmt.Errorf("listed deleted entry: %v", err)
			}
			return nil
		})
	}()
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-scanned; err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotReleasesOnEveryExit(t *testing.T) {
	for _, mode := range []string{"discovery error", "processing error", "cancellation", "panic"} {
		t.Run(mode, func(t *testing.T) {
			var s Store
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			failure := errors.New("failed")
			func() {
				defer func() {
					if p := recover(); p != nil && mode != "panic" {
						panic(p)
					}
				}()
				err := s.WithSnapshot(ctx, func(context.Context) (iter.Seq2[receipt.File, error], error) { return nil, failure }, func(_ iter.Seq2[receipt.File, error], err error) error {
					switch mode {
					case "cancellation":
						cancel()
						return ctx.Err()
					case "panic":
						panic("failed")
					case "processing error":
						return failure
					default:
						return err
					}
				})
				if err == nil {
					t.Error("expected error")
				}
			}()
			// A fresh snapshot/cleanup must still work, not inherit canceled state.
			if err := s.Cleanup(context.Background(), func() error { return nil }); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSnapshotProtectsWebDAVLocalAlias(t *testing.T) {
	ctx := context.Background()
	_, name, hash := fixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodOptions:
			w.Header().Set("DAV", "1, 2")
		case "PROPFIND":
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusMultiStatus)
			fmt.Fprint(w, `<d:multistatus xmlns:d="DAV:"><d:response><d:href>/inbox/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response><d:response><d:href>/inbox/receipt.pdf</d:href><d:propstat><d:prop><d:resourcetype/><d:getcontentlength>7</d:getcontentlength></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`)
		case http.MethodGet:
			data, err := os.ReadFile(name)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Write(data)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	remote, err := NewWebDAVInput(server.URL, "user", "password", "inbox")
	if err != nil {
		t.Fatal(err)
	}
	var s Store
	cleanup := func(ctx context.Context) error {
		return s.Cleanup(ctx, func() error { return (LocalSource{}).Cleanup(ctx, name, hash, func() error { return nil }) })
	}
	err = s.WithSnapshot(ctx, remote.Files, func(files iter.Seq2[receipt.File, error], err error) error {
		if err != nil {
			return err
		}
		waiting, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		if err := cleanup(waiting); !errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("alias cleanup did not wait: %v", err)
		}
		count := 0
		for f, err := range files {
			if err != nil {
				return err
			}
			if string(f.Data) != "receipt" {
				return fmt.Errorf("unexpected content")
			}
			count++
		}
		if count != 1 {
			return fmt.Errorf("got %d files", count)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanup(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotAppendAndDurableRestart(t *testing.T) {
	ctx := context.Background()
	dir, _, _ := fixture(t)
	database := filepath.Join(t.TempDir(), "test.db")
	conn, err := db.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { conn.Close() }()
	migrations, err := goose.NewProvider(goose.DialectSQLite3, conn, os.DirFS(".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrations.Up(ctx); err != nil {
		t.Fatal(err)
	}
	var s Store
	scan := func(appendFile bool) error {
		return s.WithSnapshot(ctx, func(ctx context.Context) (iter.Seq2[receipt.File, error], error) {
			return DiscoverSources(ctx, dir, nil)
		}, func(files iter.Seq2[receipt.File, error], err error) error {
			if err != nil {
				return err
			}
			if appendFile {
				if err := os.WriteFile(filepath.Join(dir, "later.pdf"), []byte("later"), 0600); err != nil {
					return err
				}
			}
			for file, err := range files {
				if err != nil {
					return err
				}
				if _, _, err := receipt.NewStore(conn).Import(ctx, file); err != nil {
					return err
				}
			}
			return nil
		})
	}
	if err := scan(true); err != nil {
		t.Fatal(err)
	}
	records, err := receipt.NewStore(conn).List(ctx)
	if err != nil || len(records) != 1 {
		t.Fatalf("first snapshot: %v %v", records, err)
	}
	original := records[0]
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	conn, err = db.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	s = Store{} // restart: no in-memory progress survives
	if err := scan(false); err != nil {
		t.Fatal(err)
	}
	if err := scan(false); err != nil {
		t.Fatal(err)
	}
	records, err = receipt.NewStore(conn).List(ctx)
	if err != nil || len(records) != 2 {
		t.Fatalf("subsequent snapshots: %v %v", records, err)
	}
	for _, r := range records {
		if r.SHA256 == original.SHA256 && r.Source != original.Source {
			t.Fatal("original source changed")
		}
	}
}
