package server

import (
	"context"
	"errors"
	"strings"

	"github.com/burmudar/go-receipt/paperless"
	"github.com/burmudar/go-receipt/receipt"
)

// ArchiveStatus distinguishes workflow prerequisites from disabled configuration.
// A missing archive row is normal before the archive worker's next cycle.
func (item receiptItem) ArchiveStatus() string {
	if item.Archive.State != "" {
		return item.Archive.State
	}
	if !item.ArchiveConfigured {
		return "Paperless is not configured"
	}
	if item.State == "ignored" {
		return "Not scheduled — receipt is ignored"
	}
	if item.Status != receipt.Extracted {
		return "Awaiting successful extraction"
	}
	if item.State != "approved" {
		return "Awaiting manual approval"
	}
	if item.Attachment.State != "confirmed" {
		return "Awaiting confirmed Xero attachment"
	}
	return "Awaiting next archive processing cycle"
}

func (s *Server) archivePeriodically(ctx context.Context) {
	periodic(ctx, s.cfg.ReceiptInterval, func(ctx context.Context) {
		if err := s.archive.Process(ctx, s.cfg.Source); err != nil && ctx.Err() == nil {
			s.logArchiveError(err)
		}
	})
}

func (s *Server) logArchiveError(err error) {
	var batch *paperless.BatchError
	if errors.As(err, &batch) {
		for _, failure := range batch.Failures {
			s.cfg.Logger.Warn("receipt archive failed", "receipt_sha256", failure.SHA256, "error", strings.ReplaceAll(failure.Err.Error(), "\n", "; "))
		}
		return
	}
	s.cfg.Logger.Warn("archive worker failed", "error", strings.ReplaceAll(err.Error(), "\n", "; "))
}
