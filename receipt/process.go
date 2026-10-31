package receipt

import (
	"context"
	"errors"
	"fmt"
)

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
// An interrupted pending record remains eligible for the next run. Run only one
// processor per database until worker coordination is introduced.
func (s *Store) Process(ctx context.Context, source Source, scanner Scanner, retryFailed bool) (Progress, error) {
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

		if err := s.startAttempt(ctx, r.SHA256); err != nil {
			return progress, err
		}

		analysis, scanErr := extract(ctx, source, scanner, r)
		// Cancellation is not an extraction failure; leave the record pending for restart.
		if ctx.Err() != nil {
			return progress, ctx.Err()
		}

		if err := s.finish(ctx, r.SHA256, analysis, scanErr); err != nil {
			return progress, fmt.Errorf("save receipt %s: %w", r.SHA256, err)
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
		return Analysis{}, fmt.Errorf("read source %s: %w", record.Source, err)
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
