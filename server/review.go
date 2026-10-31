package server

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/burmudar/go-receipt/receipt"
	"github.com/burmudar/go-receipt/xero"
)

type receiptItem struct {
	receipt.Record
	Review     receipt.Review
	State      string
	Effective  *receipt.Analysis
	Attachment receipt.Attachment
}

var titleDatePattern = regexp.MustCompile(`[0-9]{4}-[0-9]{2}-[0-9]{2}`)

// TitleDate is a display hint only; it never replaces extracted or corrected data.
// Do not guess between distinct dates or accept a date embedded in a longer number.
func (item receiptItem) TitleDate() string {
	name := item.Name
	result := ""
	for _, match := range titleDatePattern.FindAllStringIndex(name, -1) {
		start, end := match[0], match[1]
		if start > 0 && name[start-1] >= '0' && name[start-1] <= '9' ||
			end < len(name) && name[end] >= '0' && name[end] <= '9' {
			continue
		}
		candidate := name[start:end]
		date, err := time.Parse(time.DateOnly, candidate)
		if err != nil || date.Year() < 1 {
			continue
		}
		if result != "" && result != candidate {
			return ""
		}
		result = candidate
	}
	return result
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
	return receiptItem{Record: r, Review: review, State: state, Effective: effective}
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
