package receipt

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrSourceUnconfigured indicates that the source for a receipt is not configured.
var ErrSourceUnconfigured = errors.New("receipt source is not configured")

// ErrSourceUnavailable indicates a temporary source failure that can be retried.
var ErrSourceUnavailable = errors.New("receipt source is temporarily unavailable")

// Source reads the original bytes from a persisted intake location.
type Source interface {
	Read(context.Context, string) ([]byte, error)
}

type Scanner interface {
	Scan(context.Context, File) (Analysis, error)
}

type Progress struct {
	Extracted int `json:"extracted"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

// Process extracts receipts by reading and verifying their retained source files.
// Normal runs process pending records. Explicit retries also include failures.
// An interrupted pending record remains eligible for the next run. Records with
// unconfigured or temporarily unavailable sources retain their existing state.
// Run only one processor per database until worker coordination is introduced.
func (s *Store) Process(ctx context.Context, source Source, scanner Scanner, retryFailed bool) (Progress, error) {
	return s.process(ctx, source, scanner, retryFailed, false)
}

// ProcessAutomatic retries failed extraction at most five times total, with
// persisted attempt counts and exponential delays. Explicit CLI retries remain available.
func (s *Store) ProcessAutomatic(ctx context.Context, source Source, scanner Scanner) (Progress, error) {
	return s.process(ctx, source, scanner, true, true)
}

func (s *Store) process(ctx context.Context, source Source, scanner Scanner, retryFailed, automatic bool) (Progress, error) {
	var progress Progress
	records, err := s.List(ctx)
	if err != nil {
		return progress, err
	}

	var failures []error
	for _, r := range records {
		if err := ctx.Err(); err != nil {
			return progress, errors.Join(append(failures, err)...)
		}

		if r.Status == Extracted || (r.Status == Failed && !retryFailed) {
			progress.Skipped++
			continue
		}

		if automatic && r.Attempts > 0 {
			delay := time.Minute * time.Duration(1<<min(r.Attempts-1, 6))
			if (r.Status == Failed && r.Attempts >= 5) || time.Since(r.UpdatedAt) < delay {
				progress.Skipped++
				continue
			}
		}

		started, err := s.startAttempt(ctx, r.SHA256, automatic)
		if err != nil {
			return progress, err
		}
		if !started {
			progress.Skipped++
			continue
		}

		analysis, scanErr := extract(ctx, source, scanner, r)
		// Cancellation is not an extraction failure; leave the record pending for restart.
		if ctx.Err() != nil {
			return progress, ctx.Err()
		}

		if errors.Is(scanErr, ErrSourceUnconfigured) || errors.Is(scanErr, ErrSourceUnavailable) {
			if automatic {
				if r.Attempts+1 >= 5 {
					if _, err := s.finish(ctx, r.SHA256, analysis, scanErr); err != nil {
						return progress, err
					}
				} else if _, err := s.db.ExecContext(ctx, `UPDATE receipt SET last_error=? WHERE sha256=? AND status != 'extracted'`, scanErr.Error(), r.SHA256); err != nil {
					return progress, err
				}
			}
			progress.Skipped++
			failures = append(failures, fmt.Errorf("receipt %s (%s): %w", r.SHA256, r.Name, scanErr))
			continue
		}

		finished, err := s.finish(ctx, r.SHA256, analysis, scanErr)
		if err != nil {
			return progress, fmt.Errorf("save receipt %s: %w", r.SHA256, err)
		}
		if !finished {
			progress.Skipped++
			continue
		}

		if scanErr != nil {
			progress.Failed++
			failures = append(failures, fmt.Errorf("receipt %s (%s): %w", r.SHA256, r.Name, scanErr))
		} else {
			progress.Extracted++
		}
	}

	return progress, errors.Join(failures...)
}

func extract(ctx context.Context, source Source, scanner Scanner, record Record) (Analysis, error) {
	data, err := source.Read(ctx, record.Source)
	if err != nil {
		return Analysis{}, fmt.Errorf("read source: %w", err)
	}

	file := File{
		Name:   record.Name,
		Source: record.Source,
		Data:   data,
	}
	if file.SHA256() != record.SHA256 {
		return Analysis{}, fmt.Errorf("source content changed: restore the original receipt before retrying")
	}

	analysis, err := scanner.Scan(ctx, file)
	if err != nil {
		return Analysis{}, err
	}

	return analysis, analysis.Validate()
}
