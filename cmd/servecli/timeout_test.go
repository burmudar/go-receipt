package servecli

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/burmudar/go-receipt/receipt"
)

type blockingScanner struct{}

func (blockingScanner) Scan(ctx context.Context, _ receipt.File) (receipt.Analysis, error) {
	<-ctx.Done()
	return receipt.Analysis{}, ctx.Err()
}

func TestTimedScannerTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const timeout = 2 * time.Minute
		scanner := timedScanner{Scanner: blockingScanner{}, timeout: timeout}
		result := make(chan error, 1)
		go func() {
			_, err := scanner.Scan(t.Context(), receipt.File{})
			result <- err
		}()

		time.Sleep(timeout - time.Nanosecond)
		synctest.Wait()
		select {
		case err := <-result:
			t.Fatalf("scanner returned before the timeout: %v", err)
		default:
		}

		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if err := <-result; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("scanner error = %v, want deadline exceeded", err)
		}
	})
}
