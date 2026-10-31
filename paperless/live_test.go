package paperless

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Explicit opt-in only. Creates and removes its own synthetic document and tag.
// Never uses real receipts, the application database, or the Xero API.
func TestLivePaperless(t *testing.T) {
	endpoint := os.Getenv("RCPT_TEST_PAPERLESS_URL")
	if endpoint == "" {
		t.Skip("live Paperless test not enabled")
	}
	password, err := os.ReadFile(os.Getenv("RCPT_TEST_PAPERLESS_PASSWORD_FILE"))
	if err != nil {
		t.Fatal("read live-test password file")
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	auth, _ := json.Marshal(map[string]string{"username": "admin", "password": strings.TrimSpace(string(password))})
	req, err := http.NewRequest("POST", endpoint+"/api/token/", bytes.NewReader(auth))
	if err != nil {
		t.Fatal("invalid test URL")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal("live Paperless token request failed")
	}
	var token struct{ Token string }
	err = json.NewDecoder(resp.Body).Decode(&token)
	resp.Body.Close()
	if resp.StatusCode != 200 || err != nil || token.Token == "" {
		t.Fatalf("live authentication failed: HTTP %d", resp.StatusCode)
	}
	conn, _, hash, f, _ := setup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	m := manager(t, conn, Config{URL: endpoint, Token: token.Token, Tags: []int{1}, Timeout: 30 * time.Second})
	tagBody, _ := json.Marshal(map[string]string{"name": fmt.Sprintf("go-receipt-live-test-%d", time.Now().UnixNano())})
	data, err := m.request(ctx, "POST", "tags/", "application/json", tagBody)
	if err != nil {
		t.Fatal(err)
	}
	var tag struct{ ID int }
	if json.Unmarshal(data, &tag) != nil || tag.ID <= 0 {
		t.Fatal("invalid test tag response")
	}
	m.cfg.Tags = []int{tag.ID}
	defer func() {
		if _, err := m.request(context.Background(), "DELETE", fmt.Sprintf("tags/%d/", tag.ID), "", nil); err != nil {
			t.Errorf("remove test tag %d: %v", tag.ID, err)
		}
	}()
	// Cleanup only the exact verified document created by this test, even if assertions fail.
	defer func() {
		var checksum string
		conn.QueryRow(`SELECT checksum FROM receipt_archive WHERE receipt_sha256=?`, hash).Scan(&checksum)
		if checksum == "" {
			return
		}
		id, err := m.lookup(context.Background(), checksum)
		if err != nil {
			t.Error("test-document cleanup lookup failed", err)
			return
		}
		if id > 0 {
			if _, err = m.request(context.Background(), "DELETE", fmt.Sprintf("documents/%d/", id), "", nil); err != nil {
				t.Errorf("remove test document %d: %v", id, err)
			} else {
				t.Logf("removed synthetic document %d (Paperless trash retention may apply)", id)
			}
		}
	}()
	if err = m.Process(ctx, fileSource{}); err != nil {
		t.Fatal(err)
	}
	a := state(t, conn, hash)
	if a.State != "consuming" || a.TaskID == "" || a.DocumentID != 0 {
		t.Fatalf("upload acceptance state: %+v", a)
	}
	t.Log("live upload accepted; task persisted without premature confirmation")
	// Reconstruct the manager to exercise durable recovery, not in-memory task state.
	m = manager(t, conn, m.cfg)
	for state(t, conn, hash).State != "confirmed" {
		select {
		case <-ctx.Done():
			t.Fatal("live consumption timed out")
		case <-time.After(2 * time.Second):
		}
		if err = m.Process(ctx, fileSource{}); err != nil {
			t.Fatal(err)
		}
	}
	a = state(t, conn, hash)
	t.Logf("consumption confirmed with original SHA-256 verification: document %d", a.DocumentID)
	data, err = m.request(ctx, "GET", fmt.Sprintf("documents/%d/", a.DocumentID), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Title, Created string
		Tags           []int
	}
	if json.Unmarshal(data, &doc) != nil {
		t.Fatal("invalid live metadata")
	}
	hasTag := false
	for _, id := range doc.Tags {
		if id == tag.ID {
			hasTag = true
		}
	}
	if doc.Title != "Corrected shop — 12.30 ZAR" || !strings.HasPrefix(doc.Created, "2026-10-01") || !hasTag {
		t.Fatalf("unexpected synthetic metadata: %+v", doc)
	}
	if err = m.Process(ctx, nil); err != nil {
		t.Fatal(err)
	}
	// Simulate losing the upload/task confirmation in local durable state. Search must recover it.
	mustExec(t, conn, `UPDATE receipt_archive SET task_id='',document_id=0,state='failed' WHERE receipt_sha256=?`, hash)
	if err = m.Process(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if state(t, conn, hash).DocumentID != a.DocumentID {
		t.Fatal("uncertain recovery changed document")
	}
	t.Log("final metadata and uncertain-upload checksum recovery verified")
	// Deliberately send a second copy to measure this instance's duplicate behavior.
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, _ := w.CreateFormFile("document", "duplicate-test.pdf")
	part.Write(f.data)
	w.WriteField("tags", strconv.Itoa(tag.ID))
	w.Close()
	data, err = m.request(ctx, "POST", "documents/post_document/", w.FormDataContentType(), body.Bytes())
	if err != nil {
		t.Fatal("duplicate submission failed before consumption", err)
	}
	var task string
	if json.Unmarshal(data, &task) != nil || task == "" {
		t.Fatal("invalid duplicate task")
	}
	for {
		select {
		case <-ctx.Done():
			t.Fatal("duplicate consumption timed out")
		case <-time.After(2 * time.Second):
		}
		data, err = m.request(ctx, "GET", "tasks/?task_id="+task, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		var tasks []struct{ Status, Result string }
		if json.Unmarshal(data, &tasks) != nil || len(tasks) != 1 {
			t.Fatal("invalid duplicate task response")
		}
		if tasks[0].Status == "FAILURE" {
			if !strings.Contains(strings.ToLower(tasks[0].Result), "duplicate") {
				t.Fatal("duplicate task failed for a different reason")
			}
			t.Log("live duplicate consumption rejected as duplicate")
			break
		}
		if tasks[0].Status == "SUCCESS" {
			t.Fatal("instance unexpectedly consumed duplicate")
		}
	}
	var checksum string
	conn.QueryRow(`SELECT checksum FROM receipt_archive WHERE receipt_sha256=?`, hash).Scan(&checksum)
	id, err := m.lookup(ctx, checksum)
	if err != nil || id != a.DocumentID {
		t.Fatal("duplicate test changed checksum match", err)
	}
	var source string
	conn.QueryRow(`SELECT source FROM receipt WHERE sha256=?`, hash).Scan(&source)
	if _, err = os.Stat(source); err != nil {
		t.Fatal("intake original missing")
	}
	t.Log("one document remains; intake original retained; no Xero requests were made")
}

// A valid, uniquely identified PDF keeps live tests independent of real receipts.
func syntheticPDF() []byte {
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	text := fmt.Sprintf("BT /F1 12 Tf 50 750 Td (Synthetic go-receipt test %d) Tj ET", time.Now().UnixNano())
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "<< /Type /Pages /Kids [3 0 R] /Count 1 >>", "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>", fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(text), text)}
	offsets := []int{0}
	for i, obj := range objects {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, off := range offsets[1:] {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return b.Bytes()
}
