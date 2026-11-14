package server

import (
	"context"
	"errors"
	"fmt"
	"iter"
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
	s.jobPeriodically(ctx, "receipt-import", s.cfg.ReceiptInterval, func(ctx context.Context) error {
		return s.intake.WithSnapshot(ctx, s.cfg.Discover, func(files iter.Seq2[receipt.File, error], err error) error {
			store := receipt.NewStore(s.cfg.DB)
			var failures []error
			if err != nil {
				failures = append(failures, err)
			} else {
				for file, err := range files {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if err == nil {
						_, _, err = store.Import(ctx, file)
					}
					if err != nil {
						failures = append(failures, err)
					}
				}
			}
			_, err = store.ProcessAutomatic(ctx, s.cfg.Source, s.cfg.Scanner)
			if err != nil {
				failures = append(failures, err)
			}
			return errors.Join(failures...)
		})
	})
}

func (s *Server) refreshPeriodically(ctx context.Context) {
	s.jobPeriodically(ctx, "xero-transactions", s.cfg.TransactionInterval, func(ctx context.Context) error {
		connection, err := s.xero.Connection(ctx)
		if err != nil {
			return err
		}
		if connection.Status != xero.StatusConnected {
			return fmt.Errorf("Xero is not connected")
		}
		err = s.xero.RefreshBankTransactions(ctx)
		if errors.Is(err, xero.ErrRefreshRunning) {
			return nil
		}
		return err
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
	archives, err := receipt.NewStore(s.cfg.DB).Archives(r.Context())
	if err != nil {
		s.fail(w, 500, err)
		return
	}
	cleanups, err := receipt.NewStore(s.cfg.DB).Cleanups(r.Context())
	if err != nil {
		s.fail(w, 500, err)
		return
	}
	var items []receiptItem
	for _, record := range records {
		item := makeReceiptItem(record, reviews[record.SHA256])
		item.Attachment = attachments[record.SHA256]
		item.Archive = archives[record.SHA256]
		item.Cleanup = cleanups[record.SHA256]
		if item.Complete() {
			item.State = "completed"
		}
		item.ArchiveConfigured = s.archive != nil
		if filter := r.URL.Query().Get("archive"); filter != "" {
			state := item.Archive.State
			if state == "" || state == "uploading" || state == "consuming" {
				state = "pending"
			}
			if state != filter {
				continue
			}
		}
		if r.URL.Query().Get("state") == "" || r.URL.Query().Get("state") == item.State {
			items = append(items, item)
		}
	}
	s.render(w, http.StatusOK, "receipts.html", struct {
		Items  []receiptItem
		Filter string
	}{items, r.URL.Query().Get("state") + " " + r.URL.Query().Get("archive")})
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
	item := makeReceiptItem(record, reviews[record.SHA256])
	var transactions []xero.BankTransaction
	var suggestions []xero.Suggestion
	if s.xero != nil {
		transactions, err = s.xero.SearchBankTransactions(r.Context(), r.URL.Query().Get("q"))
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		if record.Status == receipt.Extracted && item.Review.State != "approved" {
			suggestions, err = s.xero.SuggestedBankTransactions(r.Context(), item.Effective)
			if err != nil {
				s.fail(w, http.StatusInternalServerError, err)
				return
			}
		}
		links, err := s.xero.ReceiptLinks(r.Context())
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		for i := range transactions {
			transactions[i].ReceiptLinks = links[transactions[i].ID]
		}
		for i := range suggestions {
			suggestions[i].Transaction.ReceiptLinks = links[suggestions[i].Transaction.ID]
		}
	}
	attachments, err := s.attachmentProgress(r.Context())
	if err != nil {
		s.fail(w, 500, err)
		return
	}
	archives, err := store.Archives(r.Context())
	if err != nil {
		s.fail(w, 500, err)
		return
	}
	cleanups, err := store.Cleanups(r.Context())
	if err != nil {
		s.fail(w, 500, err)
		return
	}
	item.Cleanup = cleanups[record.SHA256]
	item.Archive = archives[record.SHA256]
	item.ArchiveConfigured = s.archive != nil
	item.Attachment = attachments[record.SHA256]
	if item.Complete() {
		item.State = "completed"
	}
	s.render(w, http.StatusOK, "receipt.html", receiptDetailPage{Item: item, History: history, Transactions: transactions, Suggestions: suggestions, Query: r.URL.Query().Get("q")})
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
