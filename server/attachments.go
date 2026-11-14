package server

import (
	"context"
	"github.com/burmudar/go-receipt/receipt"
)

func (s *Server) attachPeriodically(ctx context.Context) {
	s.jobPeriodically(ctx, "xero-attachments", s.cfg.ReceiptInterval, func(ctx context.Context) error {
		return s.xero.ProcessAttachments(ctx, s.cfg.Source)
	})
}

func (s *Server) attachmentProgress(ctx context.Context) (map[string]receipt.Attachment, error) {
	return receipt.NewStore(s.cfg.DB).Attachments(ctx)
}
