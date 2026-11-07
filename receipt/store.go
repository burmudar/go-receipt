package receipt

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// Import records the content hash and first source location without copying the
// original bytes. Content identity is shared by all input sources. Existing
// records are unchanged.
func (s *Store) Import(ctx context.Context, file File) (hash string, added bool, err error) {
	if len(file.Data) == 0 {
		return "", false, fmt.Errorf("receipt is empty")
	}
	hash = file.SHA256()

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO receipt (sha256, name, source)
		VALUES (?, ?, ?)
		ON CONFLICT (sha256) DO NOTHING
	`, hash, file.Name, file.Source)
	if err != nil {
		return "", false, err
	}
	n, err := result.RowsAffected()
	return hash, n == 1, err
}

func (s *Store) List(ctx context.Context) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sha256, name, source, status, analysis, analysis_source,
		       attempts, last_error, created_at, updated_at, extracted_at
		FROM receipt
		ORDER BY created_at, sha256
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := []Record{}
	for rows.Next() {
		var r Record
		var analysis sql.NullString
		var extracted sql.NullTime

		if err := rows.Scan(
			&r.SHA256,
			&r.Name,
			&r.Source,
			&r.Status,
			&analysis,
			&r.AnalysisSource,
			&r.Attempts,
			&r.LastError,
			&r.CreatedAt,
			&r.UpdatedAt,
			&extracted,
		); err != nil {
			return nil, err
		}

		if analysis.Valid {
			if err := json.Unmarshal([]byte(analysis.String), &r.Analysis); err != nil {
				return nil, fmt.Errorf("decode receipt %s: %w", r.SHA256, err)
			}
		}

		if extracted.Valid {
			r.ExtractedAt = &extracted.Time
		}

		records = append(records, r)
	}

	return records, rows.Err()
}

func (s *Store) startAttempt(ctx context.Context, hash string, automatic bool) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE receipt
		SET attempts = attempts + 1,
            status = CASE WHEN ? THEN 'pending' ELSE status END,
		    updated_at = CURRENT_TIMESTAMP
		WHERE sha256 = ? AND status != 'extracted'
	`, automatic, hash)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n != 0, err
}

func (s *Store) finish(ctx context.Context, hash string, analysis Analysis, scanErr error) (bool, error) {
	if scanErr != nil {
		result, err := s.db.ExecContext(ctx, `
			UPDATE receipt
			SET status = 'failed',
			    last_error = ?,
			    updated_at = CURRENT_TIMESTAMP
			WHERE sha256 = ? AND status != 'extracted'
		`, scanErr.Error(), hash)
		if err != nil {
			return false, err
		}
		n, err := result.RowsAffected()
		return n != 0, err
	}

	data, err := json.Marshal(analysis)
	if err != nil {
		return false, err
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE receipt
		SET status = 'extracted',
		    analysis = ?,
		    last_error = '',
		    updated_at = CURRENT_TIMESTAMP,
		    extracted_at = CURRENT_TIMESTAMP
		WHERE sha256 = ? AND status != 'extracted'
	`, string(data), hash)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n != 0, err
}
