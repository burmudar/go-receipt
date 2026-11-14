// Package paperless archives attached receipts. Run one Manager per database.
package paperless

import (
	"bytes"
	"context"
	"crypto/md5" // Paperless uses MD5 for checksum lookup; SHA-256 verifies identity.
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/burmudar/go-receipt/receipt"
)

type Config struct {
	URL, Token string
	Tags       []int
	Timeout    time.Duration
}
type Manager struct {
	db   *sql.DB
	cfg  Config
	http *http.Client
	mu   sync.Mutex
}

func New(db *sql.DB, cfg Config) (*Manager, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("Paperless URL must be an absolute http(s) URL without credentials, query or fragment")
	}
	if strings.TrimSpace(cfg.Token) == "" || strings.ContainsAny(cfg.Token, "\r\n") {
		return nil, fmt.Errorf("Paperless token is required")
	}
	if cfg.Timeout <= 0 {
		return nil, fmt.Errorf("Paperless timeout must be positive")
	}
	if len(cfg.Tags) == 0 {
		return nil, fmt.Errorf("configure at least one Paperless receipt tag ID")
	}
	for _, id := range cfg.Tags {
		if id <= 0 {
			return nil, fmt.Errorf("Paperless tag IDs must be positive")
		}
	}
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	return &Manager{db: db, cfg: cfg, http: &http.Client{Timeout: cfg.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

const maxBytes = 10_000_000

// ReceiptFailure retains identity without leaking source paths or remote response bodies.
type ReceiptFailure struct {
	SHA256 string
	Err    error
}
type BatchError struct{ Failures []ReceiptFailure }

func (e *BatchError) Error() string {
	return fmt.Sprintf("%d receipt archives failed", len(e.Failures))
}
func (e *BatchError) Unwrap() []error {
	var errs []error
	for _, f := range e.Failures {
		errs = append(errs, f.Err)
	}
	return errs
}

type responseError struct {
	status int
	fields string
}

func (e *responseError) Error() string {
	return fmt.Sprintf("Paperless returned HTTP %d%s", e.status, e.fields)
}

var errUncertainUpload = errors.New("uncertain Paperless upload; no checksum match yet; automatic resubmission blocked")

func (m *Manager) request(ctx context.Context, method, path, kind string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, m.cfg.URL+"/api/"+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("invalid Paperless request")
	}
	req.Header.Set("Authorization", "Token "+m.cfg.Token)
	req.Header.Set("Accept", "application/json")
	if kind != "" {
		req.Header.Set("Content-Type", kind)
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Paperless request failed; remote outcome may be uncertain")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		e := &responseError{status: resp.StatusCode}
		// Only report known field names, never server-provided values or error messages.
		if resp.StatusCode == http.StatusBadRequest {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
			var fields map[string]json.RawMessage
			if json.Unmarshal(data, &fields) == nil {
				var names []string
				for _, name := range []string{"tags", "document", "title", "created"} {
					if _, ok := fields[name]; ok {
						names = append(names, name)
					}
				}
				if len(names) > 0 {
					e.fields = "; invalid fields: " + strings.Join(names, ", ")
				}
			}
		}
		return nil, e
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil || len(data) > maxBytes {
		return nil, fmt.Errorf("invalid or oversized Paperless response")
	}
	return data, nil
}

type job struct {
	hash, source, name, tenant, target, task, checksum, endpoint, previousError string
	dispatched                                                                  bool
	analysis                                                                    receipt.Analysis
}

// Process resumes only incomplete archive work. Dispatch is recorded before POST.
// An uncertain POST is never repeated automatically, even if search finds nothing.
func (m *Manager) Process(ctx context.Context, source receipt.Source) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, err := m.db.ExecContext(ctx, `INSERT OR IGNORE INTO receipt_archive(receipt_sha256,endpoint) SELECT receipt_sha256,? FROM receipt_attachment WHERE state='confirmed'`, m.cfg.URL)
	if err != nil {
		return err
	}
	rows, err := m.db.QueryContext(ctx, `SELECT a.receipt_sha256,r.source,r.name,x.tenant_id,x.transaction_id,a.task_id,a.checksum,a.endpoint,a.last_error,a.dispatched,COALESCE(v.corrected_analysis,r.analysis) FROM receipt_archive a JOIN receipt r ON r.sha256=a.receipt_sha256 JOIN receipt_attachment x ON x.receipt_sha256=a.receipt_sha256 JOIN receipt_review v ON v.receipt_sha256=a.receipt_sha256 WHERE a.state!='confirmed' AND x.state='confirmed'`)
	if err != nil {
		return err
	}
	var jobs []job
	for rows.Next() {
		var j job
		var analysis string
		if err = rows.Scan(&j.hash, &j.source, &j.name, &j.tenant, &j.target, &j.task, &j.checksum, &j.endpoint, &j.previousError, &j.dispatched, &analysis); err != nil {
			break
		}
		if err = json.Unmarshal([]byte(analysis), &j.analysis); err != nil {
			break
		}
		jobs = append(jobs, j)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	var failures []ReceiptFailure
	for _, j := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err = m.archive(ctx, source, j); err != nil {
			_, saveErr := m.db.ExecContext(context.WithoutCancel(ctx), `UPDATE receipt_archive SET state='failed',last_error=CASE WHEN ? AND last_error!='' THEN last_error ELSE ? END WHERE receipt_sha256=? AND state!='confirmed'`, errors.Is(err, errUncertainUpload), err.Error(), j.hash)
			failures = append(failures, ReceiptFailure{SHA256: j.hash, Err: errors.Join(err, saveErr)})
		}
	}
	if len(failures) > 0 {
		return &BatchError{Failures: failures}
	}
	return nil
}
func (m *Manager) archive(ctx context.Context, source receipt.Source, j job) error {
	if j.endpoint != m.cfg.URL {
		return fmt.Errorf("Paperless endpoint changed; restore original endpoint before recovery")
	}
	if j.task != "" {
		data, err := m.request(ctx, "GET", "tasks/?task_id="+url.QueryEscape(j.task), "", nil)
		if err != nil {
			return err
		}
		var tasks []struct {
			TaskID   string          `json:"task_id"`
			Status   string          `json:"status"`
			Document json.RawMessage `json:"related_document"`
		}
		if json.Unmarshal(data, &tasks) != nil {
			return fmt.Errorf("invalid Paperless task response")
		}
		for _, t := range tasks {
			if t.TaskID != j.task {
				continue
			}
			switch t.Status {
			case "SUCCESS":
				var id int
				_ = json.Unmarshal(t.Document, &id)
				if id == 0 {
					var s string
					_ = json.Unmarshal(t.Document, &s)
					id, _ = strconv.Atoi(s)
				}
				if id <= 0 {
					return fmt.Errorf("successful Paperless task has no document reference")
				}
				return m.confirm(ctx, j, id)
			case "FAILURE", "REVOKED":
				return m.recover(ctx, j, "Paperless consumption failed; checksum recovery continues, manual follow-up required before any new upload")
			default:
				_, err = m.db.ExecContext(ctx, `UPDATE receipt_archive SET state='consuming',last_error='' WHERE receipt_sha256=?`, j.hash)
				return err
			}
		}
		return m.recover(ctx, j, "Paperless task unavailable; checksum recovery continues without resubmission")
	}
	if j.checksum != "" {
		id, err := m.lookup(ctx, j.checksum)
		if err != nil {
			return err
		}
		if id > 0 {
			return m.confirm(ctx, j, id)
		}
	}
	if j.dispatched {
		if j.previousError != "" && !strings.Contains(j.previousError, errUncertainUpload.Error()) {
			return fmt.Errorf("%w; original failure: %s", errUncertainUpload, j.previousError)
		}
		return errUncertainUpload
	}
	if source == nil {
		return fmt.Errorf("receipt source is not configured")
	}
	data, err := source.Read(ctx, j.source)
	if err != nil {
		return fmt.Errorf("receipt original unavailable; restore source and retry")
	}
	if len(data) > maxBytes {
		return fmt.Errorf("receipt exceeds archive size limit")
	}
	if (receipt.File{Data: data}).SHA256() != j.hash {
		return fmt.Errorf("receipt original has changed")
	}
	sum := md5.Sum(data)
	j.checksum = hex.EncodeToString(sum[:])
	if _, err = m.db.ExecContext(ctx, `UPDATE receipt_archive SET checksum=? WHERE receipt_sha256=?`, j.checksum, j.hash); err != nil {
		return err
	}
	id, err := m.lookup(ctx, j.checksum)
	if err != nil {
		return err
	}
	if id > 0 {
		return m.confirm(ctx, j, id)
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile("document", filepath.Base(j.name))
	if err != nil {
		return err
	}
	if _, err = part.Write(data); err != nil {
		return err
	}
	_ = w.WriteField("title", m.title(j))
	_ = w.WriteField("created", j.analysis.Date.Format("2006-01-02"))
	for _, tag := range m.cfg.Tags {
		_ = w.WriteField("tags", strconv.Itoa(tag))
	}
	if err = w.Close(); err != nil {
		return err
	}
	if _, err = m.db.ExecContext(ctx, `UPDATE receipt_archive SET dispatched=1,state='uploading' WHERE receipt_sha256=?`, j.hash); err != nil {
		return err
	}
	response, err := m.request(ctx, "POST", "documents/post_document/", w.FormDataContentType(), body.Bytes())
	if err != nil {
		var rejection *responseError
		if errors.As(err, &rejection) {
			switch rejection.status {
			case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType:
				// Paperless rejected the request before enqueuing consumption. Other failures
				// (including 5xx and lost responses) remain uncertain and must not be resent.
				_, saveErr := m.db.ExecContext(context.WithoutCancel(ctx), `UPDATE receipt_archive SET dispatched=0 WHERE receipt_sha256=? AND task_id=''`, j.hash)
				return errors.Join(err, saveErr)
			}
		}
		return err
	}
	var task string
	if json.Unmarshal(response, &task) != nil || strings.TrimSpace(task) == "" {
		return fmt.Errorf("uncertain Paperless upload response; checksum recovery required")
	}
	_, err = m.db.ExecContext(ctx, `UPDATE receipt_archive SET task_id=?,state='consuming',last_error='' WHERE receipt_sha256=?`, task, j.hash)
	return err
}
func (m *Manager) recover(ctx context.Context, j job, message string) error {
	if j.checksum != "" {
		id, err := m.lookup(ctx, j.checksum)
		if err != nil {
			return err
		}
		if id > 0 {
			return m.confirm(ctx, j, id)
		}
	}
	return errors.New(message)
}
func (m *Manager) lookup(ctx context.Context, checksum string) (int, error) {
	data, err := m.request(ctx, "GET", "documents/?checksum__iexact="+url.QueryEscape(checksum), "", nil)
	if err != nil {
		return 0, err
	}
	var list struct {
		Count   *int `json:"count"`
		Results []struct {
			ID int `json:"id"`
		} `json:"results"`
	}
	if json.Unmarshal(data, &list) != nil || list.Count == nil || list.Results == nil {
		return 0, fmt.Errorf("invalid Paperless checksum response")
	}
	if *list.Count > 1 || len(list.Results) > 1 {
		return 0, fmt.Errorf("multiple Paperless checksum matches; manual follow-up required")
	}
	if *list.Count == 1 && len(list.Results) == 1 && list.Results[0].ID > 0 {
		return list.Results[0].ID, nil
	}
	if *list.Count != 0 || len(list.Results) != 0 {
		return 0, fmt.Errorf("incomplete Paperless checksum response")
	}
	return 0, nil
}
func (m *Manager) title(j job) string {
	title := []rune(fmt.Sprintf("%s — %s %s", j.analysis.Merchant, j.analysis.Total, j.analysis.Currency))
	if len(title) > 128 {
		title = title[:128]
	}
	return string(title)
}
func (m *Manager) confirm(ctx context.Context, j job, id int) error {
	// MD5 is only a search hint: verify the retained remote original with SHA-256.
	data, err := m.request(ctx, "GET", fmt.Sprintf("documents/%d/download/?original=true", id), "", nil)
	if err != nil {
		return err
	}
	if (receipt.File{Data: data}).SHA256() != j.hash {
		return fmt.Errorf("Paperless original checksum mismatch; manual follow-up required")
	}
	// Preserve existing tags while applying final corrected metadata. PATCH is retryable.
	data, err = m.request(ctx, "GET", fmt.Sprintf("documents/%d/", id), "", nil)
	if err != nil {
		return err
	}
	var doc struct {
		Tags []int `json:"tags"`
	}
	if json.Unmarshal(data, &doc) != nil {
		return fmt.Errorf("invalid Paperless document metadata")
	}
	for _, tag := range m.cfg.Tags {
		found := false
		for _, old := range doc.Tags {
			if tag == old {
				found = true
			}
		}
		if !found {
			doc.Tags = append(doc.Tags, tag)
		}
	}
	body, _ := json.Marshal(map[string]any{"title": m.title(j), "created": j.analysis.Date.Format("2006-01-02"), "tags": doc.Tags})
	if _, err = m.request(ctx, "PATCH", fmt.Sprintf("documents/%d/", id), "application/json", body); err != nil {
		return err
	}
	note := fmt.Sprintf("Receipt SHA-256: %s\nMerchant: %s\nTotal: %s %s\nXero organisation: %s\nBank transaction: %s", j.hash, j.analysis.Merchant, j.analysis.Total, j.analysis.Currency, j.tenant, j.target)
	path := fmt.Sprintf("documents/%d/notes/", id)
	data, err = m.request(ctx, "GET", path, "", nil)
	if err != nil {
		return err
	}
	var notes []struct {
		Note string `json:"note"`
	}
	if json.Unmarshal(data, &notes) != nil {
		return fmt.Errorf("invalid Paperless notes response")
	}
	found := false
	for _, n := range notes {
		if n.Note == note {
			found = true
		}
	}
	if !found {
		body, _ = json.Marshal(map[string]string{"note": note})
		data, err = m.request(ctx, "POST", path, "application/json", body)
		if err != nil {
			return err
		}
		if json.Unmarshal(data, &notes) != nil {
			return fmt.Errorf("invalid Paperless note confirmation")
		}
		for _, n := range notes {
			if n.Note == note {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("Paperless note confirmation missing")
		}
	}
	_, err = m.db.ExecContext(ctx, `UPDATE receipt_archive SET state='confirmed',document_id=?,completed_at=CURRENT_TIMESTAMP,last_error='' WHERE receipt_sha256=?`, id, j.hash)
	return err
}
