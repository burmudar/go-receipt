// Package receipt persists receipt metadata and the extraction lifecycle.
package receipt

import (
	"crypto/sha256"
	"encoding/hex"
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

// SHA256 identifies the file contents independently of their source location.
func (f File) SHA256() string {
	sum := sha256.Sum256(f.Data)
	return hex.EncodeToString(sum[:])
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
	case math.IsNaN(a.Confidence) || math.IsInf(a.Confidence, 0) ||
		a.Confidence < 0 || a.Confidence > 1:
		return fmt.Errorf("confidence must be between zero and one")
	}

	return nil
}

const (
	Pending   = "pending"
	Extracted = "extracted"
	Failed    = "failed"
)

// Record stores metadata, not file bytes. Originals remain at Source until Xero
// attachment and Paperless archiving are confirmed.
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
