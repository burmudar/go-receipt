package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/burmudar/go-receipt/receipt"
	"github.com/burmudar/go-receipt/xero"
)

func periodic(ctx context.Context, interval time.Duration, work func(context.Context)) {
	work(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			work(ctx)
		}
	}
}

func (s *Server) processPeriodically(ctx context.Context) {
	periodic(ctx, s.cfg.ReceiptInterval, func(ctx context.Context) {
		store := receipt.NewStore(s.cfg.DB)
		hadError := false
		defer func() {
			if ctx.Err() != nil {
				return
			}
			s.workerMu.Lock()
			defer s.workerMu.Unlock()
			if hadError {
				s.receiptWorkerError = "Receipt import or processing encountered errors. Check the service logs and receipt list."
			} else {
				s.receiptWorkerError = ""
			}
		}()
		files, err := s.cfg.Discover(ctx)
		if err != nil {
			hadError = true
			if ctx.Err() == nil {
				s.cfg.Logger.Warn("discover receipts", "error", err)
			}
		} else {
			for file, err := range files {
				if ctx.Err() != nil {
					return
				}
				if err == nil {
					_, _, err = store.Import(ctx, file)
				}
				if err != nil {
					hadError = true
					s.cfg.Logger.Warn("import receipt", "source", file.Source, "error", err)
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		if _, err := store.Process(ctx, s.cfg.Source, s.cfg.Scanner, false); err != nil && ctx.Err() == nil {
			hadError = true
			s.cfg.Logger.Warn("process receipts", "error", err)
		}
	})
}

func (s *Server) refreshPeriodically(ctx context.Context) {
	periodic(ctx, s.cfg.TransactionInterval, func(ctx context.Context) {
		connection, err := s.xero.Connection(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.cfg.Logger.Warn("read xero connection", "error", err)
			}
			return
		}
		if connection.Status != xero.StatusConnected {
			return
		}
		if err := s.xero.RefreshBankTransactions(ctx); err != nil && err != xero.ErrRefreshRunning && ctx.Err() == nil {
			s.cfg.Logger.Warn("background xero transaction refresh failed", "error", err)
		}
	})
}

func (s *Server) receipts(w http.ResponseWriter, r *http.Request) {
	records, err := receipt.NewStore(s.cfg.DB).List(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	reviews, err := receipt.NewStore(s.cfg.DB).Reviews(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	attachments, err := s.attachmentProgress(r.Context())
	if err != nil {
		s.fail(w, 500, err)
		return
	}
	var items []receiptItem
	for _, record := range records {
		item := makeReceiptItem(record, reviews[record.SHA256])
		item.Attachment = attachments[record.SHA256]
		if r.URL.Query().Get("state") == "" || r.URL.Query().Get("state") == item.State {
			items = append(items, item)
		}
	}
	s.render(w, http.StatusOK, "receipts.html", struct {
		Items  []receiptItem
		Filter string
	}{items, r.URL.Query().Get("state")})
}

func (s *Server) findReceipt(r *http.Request) (receipt.Record, error) {
	records, err := receipt.NewStore(s.cfg.DB).List(r.Context())
	if err != nil {
		return receipt.Record{}, err
	}
	for _, record := range records {
		if record.SHA256 == r.PathValue("hash") {
			return record, nil
		}
	}
	return receipt.Record{}, nil
}

func (s *Server) receiptDetail(w http.ResponseWriter, r *http.Request) {
	record, err := s.findReceipt(r)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if record.SHA256 == "" {
		http.NotFound(w, r)
		return
	}
	store := receipt.NewStore(s.cfg.DB)
	reviews, err := store.Reviews(r.Context())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	history, err := store.ReviewHistory(r.Context(), record.SHA256)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	var transactions []xero.BankTransaction
	if s.xero != nil {
		transactions, err = s.xero.SearchBankTransactions(r.Context(), r.URL.Query().Get("q"))
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
	}
	attachments, err := s.attachmentProgress(r.Context())
	if err != nil {
		s.fail(w, 500, err)
		return
	}
	item := makeReceiptItem(record, reviews[record.SHA256])
	item.Attachment = attachments[record.SHA256]
	s.render(w, http.StatusOK, "receipt.html", receiptDetailPage{Item: item, History: history, Transactions: transactions, Query: r.URL.Query().Get("q")})
}

func (s *Server) receiptDocument(w http.ResponseWriter, r *http.Request) {
	record, err := s.findReceipt(r)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if record.SHA256 == "" {
		http.NotFound(w, r)
		return
	}
	if s.cfg.Source == nil {
		http.Error(w, "Receipt source is not configured", http.StatusServiceUnavailable)
		return
	}
	data, err := s.cfg.Source.Read(r.Context(), record.Source)
	if err != nil {
		if errors.Is(err, receipt.ErrSourceUnconfigured) {
			http.Error(w, "Receipt source is not configured", http.StatusServiceUnavailable)
		} else {
			http.Error(w, "Receipt source unavailable", http.StatusNotFound)
		}
		return
	}
	if (receipt.File{Data: data}).SHA256() != record.SHA256 {
		http.Error(w, "Receipt source has changed", http.StatusConflict)
		return
	}
	kind := http.DetectContentType(data)
	switch kind {
	case "image/jpeg", "image/png", "image/webp", "image/gif", "application/pdf":
	default:
		http.Error(w, fmt.Sprintf("Unsupported document type %q", kind), http.StatusUnsupportedMediaType)
		return
	}
	w.Header().Set("Content-Type", kind)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}
