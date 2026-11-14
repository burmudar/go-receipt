package server

import (
	"html/template"
	"strings"
	"testing"

	"github.com/burmudar/go-receipt/receipt"
)

func TestArchiveWaitingReason(t *testing.T) {
	pages, err := template.ParseFS(templates, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, extraction, review, attachment, archive string
		configured                                    bool
		want                                          string
	}{
		{"new receipt", receipt.Extracted, "needs_review", "", "", true, "Awaiting manual approval"},
		{"extraction pending", receipt.Pending, "needs_review", "", "", true, "Awaiting successful extraction"},
		{"extraction failed", receipt.Failed, "needs_review", "", "", true, "Awaiting successful extraction"},
		{"ignored", receipt.Extracted, "ignored", "", "", true, "Not scheduled — receipt is ignored"},
		{"attachment pending", receipt.Extracted, "approved", "pending", "", true, "Awaiting confirmed Xero attachment"},
		{"attachment failed", receipt.Extracted, "approved", "failed", "", true, "Awaiting confirmed Xero attachment"},
		{"between worker cycles", receipt.Extracted, "approved", "confirmed", "", true, "Awaiting next archive processing cycle"},
		{"disabled", receipt.Extracted, "approved", "confirmed", "", false, "Paperless is not configured"},
		{"confirmed despite disabled worker", receipt.Extracted, "approved", "confirmed", "confirmed", false, "confirmed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := makeReceiptItem(receipt.Record{SHA256: "hash", Status: tc.extraction}, receipt.Review{State: tc.review})
			item.Attachment.State = tc.attachment
			item.Archive.State = tc.archive
			item.ArchiveConfigured = tc.configured
			var out strings.Builder
			if err := pages.ExecuteTemplate(&out, "receipt.html", receiptDetailPage{Item: item}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "<dt>Paperless archive</dt><dd>"+tc.want) {
				t.Fatalf("expected specific waiting reason %q", tc.want)
			}
			if strings.Contains(out.String(), "awaiting confirmed attachment and Paperless configuration") {
				t.Fatal("ambiguous fallback still shown")
			}
		})
	}
}
