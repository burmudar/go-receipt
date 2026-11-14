package server

import (
	"github.com/burmudar/go-receipt/receipt"
	"html/template"
	"strings"
	"testing"
)

func TestAttachmentProgressPage(t *testing.T) {
	pages, err := template.ParseFS(templates, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"pending", "failed", "confirmed"} {
		item := makeReceiptItem(receipt.Record{SHA256: "hash", Status: receipt.Extracted}, receipt.Review{State: "approved", TransactionID: "target"})
		item.Attachment = receipt.Attachment{State: state, Locked: state == "confirmed", Error: "test upload error"}
		var out strings.Builder
		if err = pages.ExecuteTemplate(&out, "receipt.html", receiptDetailPage{Item: item}); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"<dt>Xero attachment</dt><dd>" + state, "test upload error", "Paperless archive", "Intake cleanup", "Pending — not implemented"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%s missing %s", state, want)
			}
		}
		if state == "confirmed" && strings.Contains(out.String(), "Return approval to review") {
			t.Error("confirmed upload offered target change")
		}
	}
}
