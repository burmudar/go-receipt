package server

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Worker health and scheduling survive restarts. Run one service per database.
type workerHealth struct {
	Name, State, LastError           string
	AttemptedAt, SucceededAt, NextAt int64
	Failures                         int
}

func retryDelay(interval time.Duration, failures int) time.Duration {
	delay := interval
	for i := 0; i < failures && delay < time.Hour; i++ {
		delay *= 2
	}
	if delay > time.Hour {
		delay = time.Hour
	}
	return delay
}

func (s *Server) jobPeriodically(ctx context.Context, name string, interval time.Duration, work func(context.Context) error) {
	periodic(ctx, interval, func(ctx context.Context) {
		if err := s.runJob(ctx, name, interval, work); err != nil && ctx.Err() == nil {
			s.cfg.Logger.Warn("background worker", "worker", name, "error", err)
		}
	})
}

func (s *Server) runJob(ctx context.Context, name string, interval time.Duration, work func(context.Context) error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var paused bool
	if err := s.cfg.DB.QueryRowContext(ctx, `SELECT paused FROM service_control WHERE id=1`).Scan(&paused); err != nil {
		return err
	}
	if paused {
		return nil
	}
	if _, err := s.cfg.DB.ExecContext(ctx, `INSERT INTO service_worker(name) VALUES (?) ON CONFLICT DO NOTHING`, name); err != nil {
		return err
	}
	var next int64
	var failures int
	if err := s.cfg.DB.QueryRowContext(ctx, `SELECT next_at,failures FROM service_worker WHERE name=?`, name).Scan(&next, &failures); err != nil {
		return err
	}
	if time.Now().UnixMilli() < next {
		return nil
	}
	if _, err := s.cfg.DB.ExecContext(ctx, `UPDATE service_worker SET state='running',attempted_at=? WHERE name=?`, time.Now().UnixMilli(), name); err != nil {
		return err
	}
	jobCtx, cancel := context.WithTimeout(ctx, s.cfg.JobTimeout)
	err := work(jobCtx)
	if err == nil {
		err = jobCtx.Err()
	}
	cancel()
	// Cancellation must not prevent recording the interrupted cycle. Domain stores
	// retain their own per-receipt checkpoints and side-effect recovery rules.
	saveCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()
	state, message := "waiting", ""
	delay := interval
	success := int64(0)
	if ctx.Err() != nil {
		state, message, delay = "interrupted", "Interrupted; resumes after restart", 0
	} else if err != nil {
		failures++
		state, message = "retrying", "Cycle failed; inspect receipt failures and connection status."
		if name == "receipt-import" {
			message = "Receipt import or processing encountered errors. Inspect receipt failures and source configuration."
		}
		delay = retryDelay(interval, failures)
	} else {
		failures = 0
		success = time.Now().UnixMilli()
	}
	_, saveErr := s.cfg.DB.ExecContext(saveCtx, `UPDATE service_worker SET state=?,last_error=?,failures=?,next_at=?,succeeded_at=CASE WHEN ?>0 THEN ? ELSE succeeded_at END WHERE name=?`, state, message, failures, time.Now().Add(delay).UnixMilli(), success, success, name)
	if saveErr != nil {
		return fmt.Errorf("save worker health: %w", saveErr)
	}
	return err
}

func (s *Server) processingControl(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", 400)
		return
	}
	action := r.PostFormValue("action")
	if action != "pause" && action != "resume" {
		http.Error(w, "invalid action", 400)
		return
	}
	if _, err := s.cfg.DB.ExecContext(r.Context(), `UPDATE service_control SET paused=? WHERE id=1`, action == "pause"); err != nil {
		s.fail(w, 500, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) workerStatus(ctx context.Context) (bool, []workerHealth, error) {
	var paused bool
	if err := s.cfg.DB.QueryRowContext(ctx, `SELECT paused FROM service_control WHERE id=1`).Scan(&paused); err != nil {
		return false, nil, err
	}
	rows, err := s.cfg.DB.QueryContext(ctx, `SELECT name,state,last_error,attempted_at,succeeded_at,next_at,failures FROM service_worker ORDER BY name`)
	if err != nil {
		return false, nil, err
	}
	defer rows.Close()
	var workers []workerHealth
	for rows.Next() {
		var h workerHealth
		if err := rows.Scan(&h.Name, &h.State, &h.LastError, &h.AttemptedAt, &h.SucceededAt, &h.NextAt, &h.Failures); err != nil {
			return false, nil, err
		}
		workers = append(workers, h)
	}
	return paused, workers, rows.Err()
}

func (h workerHealth) LastSuccess() string {
	if h.SucceededAt == 0 {
		return "Never"
	}
	return time.UnixMilli(h.SucceededAt).UTC().Format(time.RFC3339)
}
func (h workerHealth) NextRun() string { return time.UnixMilli(h.NextAt).UTC().Format(time.RFC3339) }
