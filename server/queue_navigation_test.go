package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/burmudar/go-receipt/receipt"
)

func TestQueueNavigationAcrossPagesAndChangedFilters(t *testing.T) {
	var items []receiptItem
	for i := 0; i < 30; i++ {
		items = append(items, makeReceiptItem(receipt.Record{SHA256: fmt.Sprintf("hash-%02d", i), Name: fmt.Sprintf("receipt-%02d", i), Status: receipt.Extracted}, receipt.Review{}))
	}
	raw := "state=needs_review&attention=1&q=receipt&page=1"
	nav := queueNavigation(items, "hash-24", raw)
	if nav.Position != 25 || nav.Total != 30 {
		t.Fatalf("position: %+v", nav)
	}
	next, err := url.Parse(nav.Next)
	if err != nil {
		t.Fatal(err)
	}
	query, _ := url.ParseQuery(next.Query().Get("queue"))
	if next.Path != "/receipts/hash-25" || query.Get("page") != "2" || query.Get("state") != "needs_review" || query.Get("q") != "receipt" || query.Get("attention") != "1" {
		t.Fatalf("next loses context: %s", nav.Next)
	}
	if queueNavigation(items, "hash-00", raw).Previous != "" || queueNavigation(items, "hash-29", raw).Next != "" {
		t.Fatal("queue boundary wraps")
	}
	// Approval or correction may remove the current receipt from the live filter.
	items[24].State = "approved"
	changed := queueNavigation(items, "hash-24", raw)
	if changed.Position != 0 || changed.Previous == "" || !strings.HasPrefix(changed.Next, "/receipts/hash-25?") {
		t.Fatalf("lost neighbours after decision: %+v", changed)
	}
	nav = queueNavigation(items, "hash-29", "next=https://evil.example&return=//evil.example")
	if !strings.HasPrefix(nav.QueueURL, "/receipts?") || strings.Contains(nav.QueueURL, "evil") {
		t.Fatal("untrusted redirect")
	}
}

func TestApproveAndNextResolvesBeforeMutation(t *testing.T) {
	s := workspaceFixture(t)
	s.auth = nil // Exercise actual protected handlers without a login session in this test.
	request := func(path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		method := "GET"
		var body strings.Reader
		if form != nil {
			method = "POST"
			body = *strings.NewReader(form.Encode())
		}
		r := httptest.NewRequest(method, path, &body)
		if form != nil {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	items, err := s.receiptItems(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	current := items[0].SHA256
	queue := "q=Shop&state=needs_review&attention=1"
	nav := queueNavigation(items, current, queue)
	form := url.Values{"queue": {nav.Context}, "action": {"approved_next"}, "transaction": {"missing"}}
	if w := request("/receipts/"+current+"/decision", form); w.Code != http.StatusConflict || w.Header().Get("Location") != "" {
		t.Fatalf("failed approval advanced: %d", w.Code)
	}
	form.Set("transaction", "fixture-transaction")
	w := request("/receipts/"+current+"/decision", form)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != nav.Next {
		t.Fatalf("approval did not advance: %d %s want %s", w.Code, w.Header().Get("Location"), nav.Next)
	}
	history, err := receipt.NewStore(s.cfg.DB).ReviewHistory(t.Context(), current)
	if err != nil || len(history) != 1 || history[0].Action != "approved" || history[0].ApprovalMethod != "manual" {
		t.Fatalf("approval audit: %+v %v", history, err)
	}
	// The next receipt is now first, not skipped due to removal of the approved one.
	nextURL, _ := url.Parse(nav.Next)
	w = request(nav.Next, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Receipt 1 of 30") {
		t.Fatalf("next position: %d", w.Code)
	}
	nextHash := strings.TrimPrefix(nextURL.Path, "/receipts/")
	records, err := s.receiptItems(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var filename string
	for _, item := range records {
		if item.SHA256 == nextHash {
			filename = item.Name
		}
	}
	lastQueue := url.Values{"q": {filename}, "state": {"needs_review"}}.Encode()
	form.Set("queue", lastQueue)
	w = request("/receipts/"+nextHash+"/decision", form)
	if w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "/receipts?") {
		t.Fatalf("last approval: %d %s", w.Code, w.Header().Get("Location"))
	}
	w = request(w.Header().Get("Location"), nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "No receipts in this view.") {
		t.Fatal("end of queue not shown")
	}
}
