package server

import (
	"context"

	"github.com/burmudar/go-receipt/receipt"
)

func (s *Server) cleanupPeriodically(ctx context.Context) {
	source, ok := s.cfg.Source.(receipt.CleanupSource)
	if !ok {
		return
	}
	periodic(ctx, s.cfg.ReceiptInterval, func(ctx context.Context) {
		if err := receipt.NewStore(s.cfg.DB).ProcessCleanup(ctx, source); err != nil && ctx.Err() == nil {
			s.cfg.Logger.Warn("cleanup worker failed", "error", err)
		}
	})
}

func (item receiptItem) Complete() bool {
	return item.Attachment.State == "confirmed" && item.Archive.State == "confirmed" && item.Cleanup.State == "confirmed"
}
func (item receiptItem) CleanupStatus() string {
	if item.Cleanup.State != "" {
		return item.Cleanup.State
	}
	return "Pending — requires approved, attached and archived receipt"
}
