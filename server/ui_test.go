package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/burmudar/go-receipt/db"
	"github.com/burmudar/go-receipt/db/oauth"
	"github.com/burmudar/go-receipt/intake"
	"github.com/burmudar/go-receipt/receipt"
	"github.com/pressly/goose/v3"
)

func TestReceiptQueueFiltersAndPagination(t *testing.T) {
	var items []receiptItem
	for i := 0; i < 56; i++ {
		item := makeReceiptItem(receipt.Record{Name: fmt.Sprintf("receipt-%02d.pdf", i), Status: receipt.Extracted, Analysis: &receipt.Analysis{Merchant: "Corner shop", Currency: "USD", Total: "12.34"}}, receipt.Review{})
		item.Archive.State = "failed"
		items = append(items, item)
	}
	query := url.Values{"q": {"CORNER"}, "state": {"needs_review"}, "archive": {"failed"}, "attention": {"1"}, "page": {"2"}}
	page := receiptList(items, query)
	if page.Matching != 56 || len(page.Items) != 25 || page.Pages != 3 || page.Items[0].Name != "receipt-25.pdf" {
		t.Fatalf("pagination: %+v", page)
	}
	next, err := url.Parse(page.Next)
	if err != nil {
		t.Fatal(err)
	}
	if next.Query().Get("q") != "CORNER" || next.Query().Get("archive") != "failed" || next.Query().Get("attention") != "1" || next.Query().Get("page") != "3" {
		t.Fatalf("lost filters: %s", page.Next)
	}
	for _, value := range []string{"-1", "invalid", "999999999999999999999999999"} {
		query.Set("page", value)
		if receiptList(items, query).Page != 1 {
			t.Fatal("invalid page not clamped")
		}
	}
	query.Set("page", "1000")
	if receiptList(items, query).Page != 3 {
		t.Fatal("large page not clamped")
	}
	query.Set("q", "no matches")
	page = receiptList(items, query)
	if len(page.Items) != 0 || page.Pages != 1 || page.Next != "" {
		t.Fatalf("empty: %+v", page)
	}
	items[0].State = "approved"
	items[0].Archive.Error = "archive unavailable"
	page = receiptList(items, url.Values{"attention": {"1"}})
	// Failed states still need attention even when no provider error text exists.
	if page.Failures != 56 || page.NeedsReview != 56 {
		t.Fatalf("delivery errors hidden: %+v", page)
	}
}

// Isolated UI fixture: no workers or external provider calls. Also used by the
// opt-in browser smoke test so every visual and form check uses synthetic data.
func workspaceFixture(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Open(filepath.Join(t.TempDir(), "ui.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	p, err := goose.NewProvider(goose.DialectSQLite3, conn, os.DirFS(".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if err = oauth.NewStore(conn).Create(&oauth.OAuthClientModel{Name: "Xero", ClientID: "fixture", ClientSecret: "fixture"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err = conn.Exec(`INSERT INTO xero_connection(id,tenant_id,tenant_name,access_token,refresh_token,expires_at,status,refreshed_at) VALUES (1,'fixture','Demo organisation','fixture','fixture',?,'connected',?)`, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(`INSERT INTO xero_bank_transaction(tenant_id,bank_transaction_id,type,status,is_reconciled,date,contact_name,currency_code,total,has_attachments,updated_date_utc,fetched_at) VALUES ('fixture','fixture-transaction','SPEND','AUTHORISED',1,'2025-01-02','Shop','USD','12.34',0,?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	store := receipt.NewStore(conn)
	dir := t.TempDir()
	for i := 0; i < 31; i++ {
		name := fmt.Sprintf("receipt-%02d.pdf", i)
		path := filepath.Join(dir, name)
		data := demoPDF(i)
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err = store.Import(ctx, receipt.File{Name: name, Source: path, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = store.Process(ctx, intake.LocalSource{}, &testScanner{}, false); err != nil {
		t.Fatal(err)
	}
	public, _ := url.Parse("http://localhost")
	s, err := New(Config{DB: conn, Listen: "127.0.0.1:0", PublicURL: public, Password: "fixture-password", RequestTimeout: time.Second, XeroTimeout: time.Second, CheckInterval: time.Hour, Source: intake.LocalSource{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func demoPDF(n int) []byte {
	stream := fmt.Sprintf("BT /F1 22 Tf 60 700 Td (SHOP) Tj /F1 12 Tf 0 -40 Td (Receipt %02d - 2025-01-02) Tj 0 -40 Td (Groceries                 USD 12.34) Tj 0 -50 Td (TOTAL                     USD 12.34) Tj 0 -60 Td (Thank you for shopping with us.) Tj ET", n)
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 420 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>", "<< /Type /Font /Subtype /Type1 /BaseFont /Courier >>", fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream)}
	var b strings.Builder
	b.WriteString("%PDF-1.4\n")
	offsets := []int{0}
	for i, obj := range objects {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	start := b.Len()
	fmt.Fprintf(&b, "xref\n0 6\n0000000000 65535 f \n")
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&b, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", start)
	return []byte(b.String())
}

func TestWorkspaceAssetsAndEscaping(t *testing.T) {
	s := workspaceFixture(t)
	for _, path := range []string{"/static/pico.min.css", "/static/app.css"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/css") || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("asset %s: %d %v", path, w.Code, w.Header())
		}
	}
	for _, path := range []string{"/static/", "/static/PICO-LICENSE.md", "/static/nope.css"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 404 {
			t.Fatalf("unexpected asset exposure: %s %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/receipts", nil))
	if w.Code != http.StatusSeeOther {
		t.Fatal("queue authentication lost")
	}
	login := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/login", strings.NewReader("password=fixture-password"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.ServeHTTP(login, request)
	w = httptest.NewRecorder()
	request = httptest.NewRequest("GET", "/receipts?q="+url.QueryEscape(`<script>alert(1)</script>`), nil)
	request.AddCookie(login.Result().Cookies()[0])
	s.ServeHTTP(w, request)
	if w.Code != 200 || strings.Contains(w.Body.String(), "<script>") || !strings.Contains(w.Body.String(), "&lt;script&gt;") || !strings.Contains(w.Body.String(), "No receipts in this view.") {
		t.Fatal("search escaping or empty state failed")
	}
}

func TestWorkspaceBrowser(t *testing.T) {
	if os.Getenv("RECEIPT_UI_BROWSER") != "1" {
		t.Skip("opt-in: see docs/ui.md for browser test setup")
	}
	srv := httptest.NewServer(workspaceFixture(t))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", "testdata/ui-smoke.cjs", srv.URL)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("browser smoke: %v\n%s", err, output)
	}
	t.Log(string(output))
}
