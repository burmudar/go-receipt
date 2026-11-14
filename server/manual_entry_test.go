package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/burmudar/go-receipt/receipt"
)

func TestFailedReceiptManualEntry(t *testing.T) {
	s := workspaceFixture(t)
	ctx := context.Background()
	store := receipt.NewStore(s.cfg.DB)
	records, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hash := records[0].SHA256
	if _, err := s.cfg.DB.Exec(`UPDATE receipt SET status='failed',analysis=NULL,extracted_at=NULL,last_error='merchant is required' WHERE sha256=?`, hash); err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/login", strings.NewReader("password=fixture-password"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.ServeHTTP(login, req)
	cookie := login.Result().Cookies()[0]
	request := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		var body *strings.Reader
		if form == nil {
			body = strings.NewReader("")
		} else {
			body = strings.NewReader(form.Encode())
		}
		req := httptest.NewRequest(method, path, body)
		req.AddCookie(cookie)
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w
	}
	base := "/receipts/" + hash
	page := request("GET", base, nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "Enter receipt details manually") || !strings.Contains(page.Body.String(), "merchant is required") {
		t.Fatalf("failed page: %d %s", page.Code, page.Body.String())
	}
	bad := url.Values{"mode": {"manual"}, "merchant": {""}, "total": {"12.34"}, "currency": {"USD"}, "date": {"2025-01-02"}}
	if w := request("POST", base+"/correction", bad); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid entry: %d %s", w.Code, w.Body.String())
	}
	good := url.Values{"mode": {"manual"}, "merchant": {"Shop"}, "total": {"12.34"}, "currency": {"USD"}, "date": {"2025-01-02"}}
	if w := request("POST", base+"/correction", good); w.Code != http.StatusSeeOther {
		t.Fatalf("manual entry: %d %s", w.Code, w.Body.String())
	}
	if w := request("POST", base+"/correction", good); w.Code != http.StatusConflict {
		t.Fatalf("stale manual entry: %d %s", w.Code, w.Body.String())
	}
	page = request("GET", base, nil)
	for _, text := range []string{"Manually entered", "Manual entry after extraction failed: merchant is required", "Edit receipt details", "Approve suggested match"} {
		if !strings.Contains(page.Body.String(), text) {
			t.Errorf("missing %q", text)
		}
	}
	if strings.Contains(page.Body.String(), "Original extraction</summary>") || strings.Contains(page.Body.String(), "<dt>Extraction confidence</dt>") {
		t.Fatal("manual data presented as machine extraction")
	}
	history, err := store.ReviewHistory(ctx, hash)
	if err != nil || len(history) != 1 || history[0].Analysis.Merchant != "Shop" || !strings.Contains(history[0].Detail, "merchant is required") {
		t.Fatalf("history: %+v %v", history, err)
	}
	if w := request("POST", base+"/decision", url.Values{"action": {"approved"}, "transaction": {"fixture-transaction"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("approval: %d %s", w.Code, w.Body.String())
	}
	good.Del("mode")
	if w := request("POST", base+"/correction", good); w.Code != http.StatusConflict {
		t.Fatalf("approved edit: %d", w.Code)
	}
}
