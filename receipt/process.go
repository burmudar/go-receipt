package receipt

import (
	"context"
	"errors"
	"fmt"
)

type Scanner interface {
	Scan(context.Context, File) (Analysis, error)
}

type Progress struct {
	Extracted int `json:"extracted"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
}

// Process extracts saved receipts without contacting their original source.
// Normal runs process pending records. Explicit retries also include failures.
// An interrupted pending record remains eligible for the next run. Run only one
// processor per database until worker coordination is introduced.
func (s *Store) Process(ctx context.Context, scanner Scanner, retryFailed bool) (Progress, error) {
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
		file, err := s.Original(ctx, r.SHA256)
		if err != nil {
			return progress, fmt.Errorf("load receipt %s: %w", r.SHA256, err)
		}
		if err := s.startAttempt(ctx, r.SHA256); err != nil {
			return progress, err
		}
		analysis, scanErr := scanner.Scan(ctx, file)
		if scanErr == nil {
			scanErr = analysis.Validate()
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
