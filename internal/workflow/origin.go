package workflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Origin records where a persisted workflow definition came from and
// which source revision/hash it represents.
type Origin struct {
	WorkflowID     string
	SourceType     string
	Source         string
	SourceMetadata map[string]any
	DefinitionHash string
	Revision       string
	// Active is populated on reads. During UpsertOrigin, true explicitly sets
	// the row active for compatibility; false preserves/defaults unless
	// ActiveOverride is set. Prefer SetOriginActive for normal toggles.
	Active bool
	// ActiveOverride optionally sets active during UpsertOrigin. Nil preserves
	// an existing row's active value or lets new rows use the DB default.
	ActiveOverride *bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// UpsertOrigin inserts or replaces provenance for one workflow row.
// SourceType is required; all other string fields may be empty.
func (s *Store) UpsertOrigin(ctx context.Context, o Origin) (Origin, error) {
	if s.db == nil {
		return Origin{}, ErrNotOpen
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Origin{}, fmt.Errorf("begin workflow origin upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.upsertOriginTx(ctx, tx, o); err != nil {
		return Origin{}, err
	}
	if err := tx.Commit(); err != nil {
		return Origin{}, fmt.Errorf("commit workflow origin upsert: %w", err)
	}
	return s.GetOrigin(ctx, strings.TrimSpace(o.WorkflowID))
}

func (s *Store) upsertOriginTx(ctx context.Context, tx *sql.Tx, o Origin) error {
	o.WorkflowID = strings.TrimSpace(o.WorkflowID)
	o.SourceType = strings.TrimSpace(o.SourceType)
	o.Source = strings.TrimSpace(o.Source)
	o.DefinitionHash = strings.TrimSpace(o.DefinitionHash)
	o.Revision = strings.TrimSpace(o.Revision)
	if o.WorkflowID == "" {
		return errors.New("workflow origin: workflow_id is required")
	}
	if o.SourceType == "" {
		return errors.New("workflow origin: source_type is required")
	}
	metadata, err := marshalOriginMetadata(o.SourceMetadata)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	active, setActive := originActiveOverride(o)
	var res sql.Result
	if setActive {
		res, err = tx.ExecContext(ctx, `
			UPDATE workflow_origins
			   SET source_type = ?, source = ?, source_metadata = ?,
			       definition_hash = ?, revision = ?, active = ?, updated_at = ?
			 WHERE workflow_id = ?`,
			o.SourceType, o.Source, metadata, o.DefinitionHash, o.Revision, active, now, o.WorkflowID)
	} else {
		res, err = tx.ExecContext(ctx, `
			UPDATE workflow_origins
			   SET source_type = ?, source = ?, source_metadata = ?,
			       definition_hash = ?, revision = ?, updated_at = ?
			 WHERE workflow_id = ?`,
			o.SourceType, o.Source, metadata, o.DefinitionHash, o.Revision, now, o.WorkflowID)
	}
	if err != nil {
		return fmt.Errorf("update workflow origin: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if setActive {
			_, err = tx.ExecContext(ctx, `
				INSERT INTO workflow_origins(
					workflow_id, source_type, source, source_metadata,
					definition_hash, revision, active, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				o.WorkflowID, o.SourceType, o.Source, metadata, o.DefinitionHash, o.Revision, active, now, now)
		} else {
			_, err = tx.ExecContext(ctx, `
				INSERT INTO workflow_origins(
					workflow_id, source_type, source, source_metadata,
					definition_hash, revision, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				o.WorkflowID, o.SourceType, o.Source, metadata, o.DefinitionHash, o.Revision, now, now)
		}
		if err != nil {
			return fmt.Errorf("insert workflow origin: %w", err)
		}
	}
	return nil
}

// GetOrigin returns provenance for workflowID, or ErrNotFound.
func (s *Store) GetOrigin(ctx context.Context, workflowID string) (Origin, error) {
	if s.db == nil {
		return Origin{}, ErrNotOpen
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT workflow_id, source_type, source, source_metadata,
		       definition_hash, revision, active, created_at, updated_at
		  FROM workflow_origins
		 WHERE workflow_id = ?`, workflowID)
	o, err := scanOrigin(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Origin{}, ErrNotFound
	}
	if err != nil {
		return Origin{}, err
	}
	return o, nil
}

// ListOrigins returns workflow origins ordered by source type/source.
// When activeOnly is true, inactive origins are hidden.
func (s *Store) ListOrigins(ctx context.Context, activeOnly bool) ([]Origin, error) {
	if s.db == nil {
		return nil, ErrNotOpen
	}
	q := `SELECT workflow_id, source_type, source, source_metadata,
	             definition_hash, revision, active, created_at, updated_at
	        FROM workflow_origins`
	if activeOnly {
		q += ` WHERE active = 1`
	}
	q += ` ORDER BY source_type ASC, source ASC, workflow_id ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list workflow origins: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Origin
	for rows.Next() {
		o, err := scanOrigin(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// SetOriginActive toggles the active bit for an existing origin row.
func (s *Store) SetOriginActive(ctx context.Context, workflowID string, active bool) (Origin, error) {
	if s.db == nil {
		return Origin{}, ErrNotOpen
	}
	activeInt := 0
	if active {
		activeInt = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE workflow_origins SET active = ?, updated_at = ? WHERE workflow_id = ?`,
		activeInt, time.Now().Unix(), workflowID)
	if err != nil {
		return Origin{}, fmt.Errorf("update workflow origin active: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Origin{}, ErrNotFound
	}
	return s.GetOrigin(ctx, workflowID)
}

type originScanner interface {
	Scan(dest ...any) error
}

func originActiveOverride(o Origin) (int, bool) {
	if o.ActiveOverride != nil {
		if *o.ActiveOverride {
			return 1, true
		}
		return 0, true
	}
	if o.Active {
		return 1, true
	}
	return 0, false
}

func scanOrigin(sc originScanner) (Origin, error) {
	var (
		o           Origin
		metadataRaw sql.NullString
		active      int
		created     int64
		updated     int64
	)
	if err := sc.Scan(&o.WorkflowID, &o.SourceType, &o.Source, &metadataRaw,
		&o.DefinitionHash, &o.Revision, &active, &created, &updated); err != nil {
		return Origin{}, err
	}
	md, err := unmarshalOriginMetadata(metadataRaw)
	if err != nil {
		return Origin{}, err
	}
	o.SourceMetadata = md
	o.Active = active != 0
	o.CreatedAt = time.Unix(created, 0).UTC()
	o.UpdatedAt = time.Unix(updated, 0).UTC()
	return o, nil
}

func marshalOriginMetadata(m map[string]any) (any, error) {
	if len(m) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal workflow origin metadata: %w", err)
	}
	return string(b), nil
}

func unmarshalOriginMetadata(raw sql.NullString) (map[string]any, error) {
	if !raw.Valid || strings.TrimSpace(raw.String) == "" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw.String), &m); err != nil {
		return nil, fmt.Errorf("unmarshal workflow origin metadata: %w", err)
	}
	return m, nil
}
