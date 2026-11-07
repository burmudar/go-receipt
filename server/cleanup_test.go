package server

import (
	"github.com/burmudar/go-receipt/receipt"
	"html/template"
	"strings"
	"testing"
)

func TestCleanupProgressPages(t *testing.T) {
	pages, err := template.ParseFS(templates, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"pending", "failed", "uncertain", "confirmed"} {
		item := makeReceiptItem(receipt.Record{SHA256: "hash", Status: receipt.Extracted}, receipt.Review{State: "approved", TransactionID: "target"})
		item.Attachment = receipt.Attachment{State: "confirmed", Locked: true}
		item.Archive = receipt.Archive{State: "confirmed", DocumentID: 42}
		item.Cleanup = receipt.Cleanup{State: state, Error: "cleanup intervention"}
		if item.Complete() != (state == "confirmed") {
			t.Fatal("incorrect completion")
		}
		if item.Complete() {
			item.State = "completed"
		}
		var out strings.Builder
		if err = pages.ExecuteTemplate(&out, "receipt.html", receiptDetailPage{Item: item}); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"Intake cleanup</dt><dd>" + state, "cleanup intervention", "Approved for target"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("missing %s", want)
			}
		}
		if strings.Contains(out.String(), "Complete — attached and archived") != (state == "confirmed") {
			t.Fatal("wrong completion label")
		}
		out.Reset()
		if err = pages.ExecuteTemplate(&out, "receipts.html", struct {
			Items  []receiptItem
			Filter string
		}{[]receiptItem{item}, ""}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "cleanup "+state) || !strings.Contains(out.String(), "cleanup intervention") {
			t.Fatal("missing list progress")
		}
		item.Archive.State = "consuming"
		if item.Complete() {
			t.Fatal("archive acceptance treated as completion")
		}
		item.Archive.State = "confirmed"
		item.Attachment.State = "pending"
		if item.Complete() {
			t.Fatal("unattached completion")
		}
	}
}
