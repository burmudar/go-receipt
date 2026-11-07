package receipt_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/intake"
	"github.com/burmudar/go-receipt/receipt"
	"github.com/pressly/goose/v3"
)

type manualScanner func(context.Context, receipt.File) (receipt.Analysis, error)

func (f manualScanner) Scan(ctx context.Context, file receipt.File) (receipt.Analysis, error) {
	return f(ctx, file)
}

func TestManualEntryDuringRetry(t *testing.T) {
	for _, scanFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "extraction succeeds", true: "extraction fails"}[scanFails], func(t *testing.T) {
			ctx := context.Background()
			conn, err := db.Open(filepath.Join(t.TempDir(), "app.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			p, err := goose.NewProvider(goose.DialectSQLite3, conn, os.DirFS(".."))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = p.Up(ctx); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "receipt.pdf")
			data := []byte("receipt")
			if err = os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			store := receipt.NewStore(conn)
			hash, _, err := store.Import(ctx, receipt.File{Name: "receipt.pdf", Source: path, Data: data})
			if err != nil {
				t.Fatal(err)
			}
			manual := receipt.Analysis{
				Merchant:   "Manual Shop",
				Total:      "12.34",
				Currency:   "USD",
				Date:       time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC),
				Confidence: 1,
			}
			if err := store.EnterManually(ctx, hash, manual); !errors.Is(err, receipt.ErrReviewConflict) {
				t.Fatalf("entry must not override pending receipt: %v", err)
			}
			if _, err = conn.Exec(`UPDATE receipt SET status='failed',last_error='merchant is required' WHERE sha256=?`, hash); err != nil {
				t.Fatal(err)
			}
			invalid := manual
			invalid.Total = "bad"
			if err := store.EnterManually(ctx, hash, invalid); err == nil {
				t.Fatal("invalid manual data accepted")
			}
			scanner := manualScanner(func(ctx context.Context, _ receipt.File) (receipt.Analysis, error) {
				if err := store.EnterManually(ctx, hash, manual); err != nil {
					t.Fatal(err)
				}
				if scanFails {
					return receipt.Analysis{}, errors.New("extract failed")
				}
				return receipt.Analysis{Merchant: "Machine Shop", Total: "12.34", Currency: "USD", Date: manual.Date}, nil
			})
			progress, err := store.Process(ctx, intake.LocalSource{}, scanner, true)
			if err != nil {
				t.Fatal(err)
			}
			if want := (receipt.Progress{Skipped: 1}); progress != want {
				t.Fatalf("progress: got %+v, want %+v", progress, want)
			}
			records, err := store.List(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 {
				t.Fatalf("got %d records, want 1", len(records))
			}
			record := records[0]
			if record.Status != receipt.Extracted {
				t.Errorf("status: got %q, want %q", record.Status, receipt.Extracted)
			}
			if record.AnalysisSource != "manual" {
				t.Errorf("analysis source: got %q, want manual", record.AnalysisSource)
			}
			if record.Analysis == nil {
				t.Fatal("manual analysis missing")
			}
			if record.Analysis.Merchant != "Manual Shop" {
				t.Errorf("merchant: got %q, want Manual Shop", record.Analysis.Merchant)
			}
			if record.Analysis.Confidence != 0 {
				t.Errorf("confidence: got %v, want 0", record.Analysis.Confidence)
			}
			if record.LastError != "" {
				t.Errorf("last error: got %q, want empty", record.LastError)
			}
			if err := store.EnterManually(ctx, hash, manual); !errors.Is(err, receipt.ErrReviewConflict) {
				t.Fatalf("entry must not overwrite extracted data: %v", err)
			}
		})
	}
}
