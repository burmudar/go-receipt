package server

import (
	"github.com/burmudar/go-receipt/receipt"
	"html/template"
	"strings"
	"testing"
)

func TestArchiveProgressPages(t *testing.T) {
	pages, err := template.ParseFS(templates, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"pending", "consuming", "failed", "confirmed"} {
		item := makeReceiptItem(receipt.Record{SHA256: "hash", Status: receipt.Extracted}, receipt.Review{State: "approved"})
		item.Attachment = receipt.Attachment{State: "confirmed", Locked: true}
		item.Archive = receipt.Archive{State: state, TaskID: "task-reference", Error: "archive problem"}
		if state == "confirmed" {
			item.Archive.DocumentID = 42
			item.Archive.Error = ""
		}
		var out strings.Builder
		if err = pages.ExecuteTemplate(&out, "receipt.html", receiptDetailPage{Item: item}); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"Paperless archive</dt><dd>" + state, "task-reference", "Intake cleanup</dt><dd>Pending", "not complete until archive and cleanup finish"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%s missing %s", state, want)
			}
		}
		if item.State == "completed" {
			t.Fatal("premature completion")
		}
		if state == "confirmed" && !strings.Contains(out.String(), "document 42") {
			t.Fatal("document reference missing")
		}
		out.Reset()
		if err = pages.ExecuteTemplate(&out, "receipts.html", struct {
			Items  []receiptItem
			Filter string
		}{[]receiptItem{item}, ""}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "archive "+state) {
			t.Fatal("list state missing")
		}
		if state == "failed" && !strings.Contains(out.String(), "archive problem") {
			t.Fatal("list error missing")
		}
	}
}
