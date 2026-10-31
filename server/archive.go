package server

import "context"

func (s *Server) archivePeriodically(ctx context.Context) {
	periodic(ctx, s.cfg.ReceiptInterval, func(ctx context.Context) {
		if err := s.archive.Process(ctx, s.cfg.Source); err != nil && ctx.Err() == nil {
			s.cfg.Logger.Warn("archive receipts", "error", err)
		}
	})
}
