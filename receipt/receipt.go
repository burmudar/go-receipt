// Package receipt persists source documents and their extraction lifecycle.
package receipt

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// File is independent of the transport used to retrieve a receipt.
type File struct {
	Name   string
	Source string
	Data   []byte
}

// Analysis uses a decimal string for Total so no monetary value passes through
// floating-point conversion. Keep this representation in JSON and SQLite.
type Analysis struct {
	Merchant   string    `json:"merchant" jsonschema_description:"The merchant name"`
	Total      string    `json:"total" jsonschema_description:"Exact decimal amount without currency or grouping separators, for example 123.45"`
	Currency   string    `json:"currency" jsonschema_description:"Three-letter uppercase currency code, for example ZAR"`
	Date       time.Time `json:"date" jsonschema_description:"Receipt date as an RFC3339 timestamp"`
	Confidence float64   `json:"confidence" jsonschema_description:"Extraction confidence between zero and one"`
}

var (
	decimalAmount = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`)
	currencyCode  = regexp.MustCompile(`^[A-Z]{3}$`)
)

func (a Analysis) Validate() error {
	switch {
	case strings.TrimSpace(a.Merchant) == "":
		return fmt.Errorf("merchant is required")
	case !decimalAmount.MatchString(a.Total):
		return fmt.Errorf("total must be an exact decimal string")
	case !currencyCode.MatchString(a.Currency):
		return fmt.Errorf("currency must be a three-letter uppercase code")
	case a.Date.IsZero():
		return fmt.Errorf("receipt date is required")
	case math.IsNaN(a.Confidence) || math.IsInf(a.Confidence, 0) || a.Confidence < 0 || a.Confidence > 1:
		return fmt.Errorf("confidence must be between zero and one")
	}
	return nil
}

const (
	Pending   = "pending"
	Extracted = "extracted"
	Failed    = "failed"
)

// Record omits original file bytes. Original files remain in SQLite for retries
// and later preview or attachment upload.
type Record struct {
	SHA256      string     `json:"sha256"`
	Name        string     `json:"name"`
	Source      string     `json:"source"`
	Status      string     `json:"status"`
	Analysis    *Analysis  `json:"analysis,omitempty"`
	Attempts    int        `json:"attempts"`
	LastError   string     `json:"last_error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ExtractedAt *time.Time `json:"extracted_at,omitempty"`
}
