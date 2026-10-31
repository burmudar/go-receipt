package receipt

import "context"

type Attachment struct {
	State, ID, Error, CompletedAt string
	Locked                        bool
}

func (s *Store) Attachments(ctx context.Context) (map[string]Attachment, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT receipt_sha256,state,attachment_id,last_error,completed_at,locked FROM receipt_attachment")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]Attachment{}
	for rows.Next() {
		var hash string
		var a Attachment
		if err = rows.Scan(&hash, &a.State, &a.ID, &a.Error, &a.CompletedAt, &a.Locked); err != nil {
			return nil, err
		}
		result[hash] = a
	}
	return result, rows.Err()
}
