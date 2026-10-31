package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/burmudar/go-receipt/receipt"
	"github.com/burmudar/go-receipt/xero"
)

type receiptItem struct {
	receipt.Record
	Review    receipt.Review
	State     string
	Effective *receipt.Analysis
}
type receiptDetailPage struct {
	Item         receiptItem
	History      []receipt.ReviewEvent
	Transactions []xero.BankTransaction
	Query        string
}

func makeReceiptItem(r receipt.Record, review receipt.Review) receiptItem {
	state := review.State
	if state == "" {
		state = "needs_review"
	}
	if r.Status == receipt.Pending {
		state = "pending"
	}
	if r.Status == receipt.Failed {
		state = "failed"
	}
	effective := r.Analysis
	if review.Corrected != nil {
		effective = review.Corrected
	}
	return receiptItem{r, review, state, effective}
}

func (s *Server) correctReceipt(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	record, err := s.findReceipt(r)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if record.SHA256 == "" {
		http.NotFound(w, r)
		return
	}
	if record.Analysis == nil {
		http.Error(w, "receipt has no extraction", http.StatusConflict)
		return
	}
	date, err := time.Parse(time.DateOnly, strings.TrimSpace(r.PostFormValue("date")))
	if err != nil {
		http.Error(w, "date must be YYYY-MM-DD", http.StatusBadRequest)
		return
	}
	analysis := receipt.Analysis{Merchant: strings.TrimSpace(r.PostFormValue("merchant")), Total: strings.TrimSpace(r.PostFormValue("total")), Currency: strings.TrimSpace(r.PostFormValue("currency")), Date: date, Confidence: record.Analysis.Confidence}
	if err := analysis.Validate(); err != nil {
		http.Error(w, fmt.Sprintf("invalid correction: %v", err), http.StatusBadRequest)
		return
	}
	if err := receipt.NewStore(s.cfg.DB).Correct(r.Context(), record.SHA256, analysis); err != nil {
		s.reviewError(w, err)
		return
	}
	http.Redirect(w, r, "/receipts/"+record.SHA256, http.StatusSeeOther)
}

func (s *Server) decideReceipt(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	record, err := s.findReceipt(r)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if record.SHA256 == "" {
		http.NotFound(w, r)
		return
	}
	if err := receipt.NewStore(s.cfg.DB).Decide(r.Context(), record.SHA256, r.PostFormValue("action"), r.PostFormValue("transaction")); err != nil {
		s.reviewError(w, err)
		return
	}
	http.Redirect(w, r, "/receipts/"+record.SHA256, http.StatusSeeOther)
}
func (s *Server) reviewError(w http.ResponseWriter, err error) {
	if errors.Is(err, receipt.ErrReviewConflict) {
		s.fail(w, http.StatusConflict, err)
		return
	}
	s.fail(w, http.StatusInternalServerError, err)
}
