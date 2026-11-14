package server

import (
	"context"
	"github.com/burmudar/go-receipt/receipt"
)

func (s *Server) attachPeriodically(ctx context.Context) {
	periodic(ctx, s.cfg.ReceiptInterval, func(ctx context.Context) {
		if err := s.xero.ProcessAttachments(ctx, s.cfg.Source); err != nil && ctx.Err() == nil {
			s.cfg.Logger.Warn("attach receipts", "error", err)
		}
	})
}

func (s *Server) attachmentProgress(ctx context.Context) (map[string]receipt.Attachment, error) {
	return receipt.NewStore(s.cfg.DB).Attachments(ctx)
}
