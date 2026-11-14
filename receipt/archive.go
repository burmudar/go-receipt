package receipt

import "context"

type Archive struct {
	State, TaskID, Error, CompletedAt string
	DocumentID                        int
}

func (s *Store) Archives(ctx context.Context) (map[string]Archive, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT receipt_sha256,state,task_id,document_id,last_error,completed_at FROM receipt_archive")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]Archive{}
	for rows.Next() {
		var hash string
		var a Archive
		if err = rows.Scan(&hash, &a.State, &a.TaskID, &a.DocumentID, &a.Error, &a.CompletedAt); err != nil {
			return nil, err
		}
		result[hash] = a
	}
	return result, rows.Err()
}
