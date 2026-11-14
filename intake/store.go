package intake

import (
	"context"
	"iter"
	"sync"

	"github.com/burmudar/go-receipt/receipt"
)

// Store coordinates one service's intake snapshots and cleanup. SQLite remains
// authoritative; this store contains only transient discovery state. Its zero
// value is ready to use. Do not copy it after use.
//
// Protection is deliberately source-wide: local mounts and WebDAV references may
// alias without a known mapping. Blocking all cleanup avoids guessing destructive
// mappings and also protects persisted sources read during automatic processing.
// It does not coordinate other processes or external writers.
type Store struct {
	once sync.Once
	turn chan struct{}
}

func (s *Store) acquire(ctx context.Context) (func(), error) {
	s.once.Do(func() { s.turn = make(chan struct{}, 1); s.turn <- struct{}{} })
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.turn:
	}
	release := func() { s.turn <- struct{}{} }
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// WithSnapshot waits for already-started cleanup before calling discover, then
// protects the snapshot through work (including processing persisted originals).
// discover must finish listing before returning; its iterator reads bytes lazily.
// Work must not retain the iterator beyond this call. Protection is released on
// every exit, including cancellation, errors and panics. No mutex or database
// transaction is held during I/O. Queued cleanup gets a turn between snapshots.
func (s *Store) WithSnapshot(ctx context.Context,
	discover func(context.Context) (iter.Seq2[receipt.File, error], error),
	work func(iter.Seq2[receipt.File, error], error) error,
) error {
	release, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	files, err := discover(ctx)
	return work(files, err)
}

// Cleanup runs an entire cleanup pass outside any active snapshot. The callback
// retains responsibility for durable eligibility, hash checks and progress.
func (s *Store) Cleanup(ctx context.Context, work func() error) error {
	release, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	return work()
}
