package workflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"autosk/internal/id"
)

// ManagedSyncReport describes how SyncManagedDefinition reconciled a managed
// workflow definition with the project DB.
type ManagedSyncReport struct {
	Workflow             Workflow
	Noop                 bool
	Replaced             bool
	Superseded           bool
	PreviousWorkflowID   string
	PreviousWorkflowName string
	SupersededName       string
}

// SyncManagedDefinition reconciles a managed workflow definition with the
// project database. If the active managed row already carries the same
// canonical definition hash, only provenance is refreshed. If the hash changed,
// the canonical name is moved to a fresh workflow row; referenced old rows are
// retained under a reserved revision name and linked to the successor.
func (s *Store) SyncManagedDefinition(ctx context.Context, def Definition, origin Origin) (ManagedSyncReport, error) {
	if s.db == nil {
		return ManagedSyncReport{}, ErrNotOpen
	}
	origin.SourceType = strings.TrimSpace(origin.SourceType)
	if origin.SourceType == "" {
		return ManagedSyncReport{}, errors.New("managed workflow sync: source_type is required")
	}
	if HasReservedRevisionSuffix(def.Name) {
		return ManagedSyncReport{}, fmt.Errorf("managed workflow sync: %w: %s", ErrAlreadyExist, def.Name)
	}
	hash, err := HashDefinition(def)
	if err != nil {
		return ManagedSyncReport{}, err
	}
	origin.DefinitionHash = hash

	existing, err := s.GetByName(ctx, def.Name)
	if errors.Is(err, ErrNotFound) {
		prepared, perr := s.prepareWorkflowInsert(ctx, def, false)
		if perr != nil {
			return ManagedSyncReport{}, perr
		}
		w, cerr := s.createManagedFresh(ctx, prepared, origin)
		if cerr != nil {
			return ManagedSyncReport{}, cerr
		}
		return ManagedSyncReport{Workflow: w}, nil
	}
	if err != nil {
		return ManagedSyncReport{}, err
	}

	existingOrigin, oerr := s.GetOrigin(ctx, existing.ID)
	if errors.Is(oerr, ErrNotFound) {
		return ManagedSyncReport{}, fmt.Errorf("%w: managed workflow %q collides with an unmanaged workflow", ErrAlreadyExist, def.Name)
	}
	if oerr != nil {
		return ManagedSyncReport{}, oerr
	}
	if existingOrigin.DefinitionHash == hash {
		active := true
		origin.WorkflowID = existing.ID
		origin.ActiveOverride = &active
		if _, err := s.UpsertOrigin(ctx, origin); err != nil {
			return ManagedSyncReport{}, err
		}
		w, err := s.GetByID(ctx, existing.ID)
		if err != nil {
			return ManagedSyncReport{}, err
		}
		return ManagedSyncReport{Workflow: w, Noop: true}, nil
	}

	prepared, err := s.prepareWorkflowInsert(ctx, def, false)
	if err != nil {
		return ManagedSyncReport{}, err
	}
	refs, err := s.workflowReferenceCount(ctx, existing.ID)
	if err != nil {
		return ManagedSyncReport{}, err
	}
	w, report, err := s.replaceManaged(ctx, existing, refs, prepared, origin)
	if err != nil {
		return ManagedSyncReport{}, err
	}
	report.Workflow = w
	return report, nil
}

type preparedWorkflowInsert struct {
	def         Definition
	workflowID  string
	stepIDs     map[string]string
	stepAgents  map[string]string
	firstStepID string
	names       []string
	isSynthetic int
	isolation   IsolationMode
}

func (s *Store) prepareWorkflowInsert(ctx context.Context, def Definition, isSynthetic bool) (preparedWorkflowInsert, error) {
	if err := Validate(ctx, def, s.agent, ValidateOpts{AllowSyntheticName: isSynthetic}); err != nil {
		return preparedWorkflowInsert{}, err
	}
	stepAgents := make(map[string]string, len(def.Steps))
	for stepName, sd := range def.Steps {
		a, err := s.agent.GetByName(ctx, sd.AgentName)
		if err != nil {
			return preparedWorkflowInsert{}, fmt.Errorf("resolve agent %q for step %q: %w", sd.AgentName, stepName, err)
		}
		stepAgents[stepName] = a.ID
	}
	wfID, err := id.NewUnique(WorkflowIDPrefix, func(candidate string) (bool, error) {
		return s.workflowIDExists(ctx, candidate)
	})
	if err != nil {
		return preparedWorkflowInsert{}, fmt.Errorf("generate workflow id: %w", err)
	}
	stepIDs := make(map[string]string, len(def.Steps))
	for _, name := range orderedStepNames(def) {
		stepID, err := id.NewUnique(StepIDPrefix, func(candidate string) (bool, error) {
			return s.stepIDExists(ctx, candidate)
		})
		if err != nil {
			return preparedWorkflowInsert{}, fmt.Errorf("generate step id: %w", err)
		}
		stepIDs[name] = stepID
	}
	firstStepID, ok := stepIDs[def.FirstStep]
	if !ok {
		return preparedWorkflowInsert{}, fmt.Errorf("internal: first_step %q has no id", def.FirstStep)
	}
	synthetic := 0
	if isSynthetic {
		synthetic = 1
	}
	isolation := def.Isolation.Normalize()
	if isSynthetic && isolation != IsolationNone {
		return preparedWorkflowInsert{}, fmt.Errorf("synthetic workflow %q cannot use isolation=%q", def.Name, isolation)
	}
	return preparedWorkflowInsert{
		def:         def,
		workflowID:  wfID,
		stepIDs:     stepIDs,
		stepAgents:  stepAgents,
		firstStepID: firstStepID,
		names:       orderedStepNames(def),
		isSynthetic: synthetic,
		isolation:   isolation,
	}, nil
}

func (s *Store) createManagedFresh(ctx context.Context, prepared preparedWorkflowInsert, origin Origin) (Workflow, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Workflow{}, fmt.Errorf("begin managed workflow create: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.insertPreparedWorkflowTx(ctx, tx, prepared); err != nil {
		return Workflow{}, err
	}
	origin.WorkflowID = prepared.workflowID
	if err := insertOriginTx(ctx, tx, origin, true); err != nil {
		return Workflow{}, err
	}
	if err := tx.Commit(); err != nil {
		return Workflow{}, fmt.Errorf("commit managed workflow create: %w", err)
	}
	return s.GetByID(ctx, prepared.workflowID)
}

func (s *Store) replaceManaged(ctx context.Context, old Workflow, refs int, prepared preparedWorkflowInsert, origin Origin) (Workflow, ManagedSyncReport, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Workflow{}, ManagedSyncReport{}, fmt.Errorf("begin managed workflow replace: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	report := ManagedSyncReport{PreviousWorkflowID: old.ID, PreviousWorkflowName: old.Name}
	if refs > 0 {
		revName := revisionName(old.Name, old.ID)
		if _, err := tx.ExecContext(ctx,
			`UPDATE workflows SET name = ? WHERE id = ?`, revName, old.ID); err != nil {
			return Workflow{}, report, fmt.Errorf("rename superseded workflow: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `REINDEX workflows`); err != nil {
			return Workflow{}, report, fmt.Errorf("reindex workflows after managed workflow rename: %w", err)
		}
		if err := setOriginActiveTx(ctx, tx, old.ID, false); err != nil {
			return Workflow{}, report, err
		}
		report.Superseded = true
		report.SupersededName = revName
	} else {
		if _, err := tx.ExecContext(ctx, `DELETE FROM workflows WHERE id = ?`, old.ID); err != nil {
			return Workflow{}, report, fmt.Errorf("delete unreferenced managed workflow: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `REINDEX workflows`); err != nil {
			return Workflow{}, report, fmt.Errorf("reindex workflows after managed workflow delete: %w", err)
		}
		report.Replaced = true
	}

	if err := s.insertPreparedWorkflowTx(ctx, tx, prepared); err != nil {
		return Workflow{}, report, err
	}
	if refs > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE workflows SET superseded_by_id = ? WHERE id = ?`, prepared.workflowID, old.ID); err != nil {
			return Workflow{}, report, fmt.Errorf("link superseded workflow: %w", err)
		}
	}
	origin.WorkflowID = prepared.workflowID
	if err := insertOriginTx(ctx, tx, origin, true); err != nil {
		return Workflow{}, report, err
	}
	if err := tx.Commit(); err != nil {
		return Workflow{}, report, fmt.Errorf("commit managed workflow replace: %w", err)
	}
	w, err := s.GetByID(ctx, prepared.workflowID)
	return w, report, err
}

func (s *Store) insertPreparedWorkflowTx(ctx context.Context, tx *sql.Tx, p preparedWorkflowInsert) error {
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workflows(id, name, description, first_step_id, is_synthetic, isolation, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, p.workflowID, p.def.Name, p.def.Description, p.firstStepID, p.isSynthetic, string(p.isolation), now); err != nil {
		if isUniqueErr(err, "workflows.name") {
			return fmt.Errorf("%w: %s", ErrAlreadyExist, p.def.Name)
		}
		return fmt.Errorf("insert workflow: %w", err)
	}
	for seq, stepName := range p.names {
		sd := p.def.Steps[stepName]
		paramsJSON, perr := marshalAgentParams(sd.AgentParams)
		if perr != nil {
			return fmt.Errorf("marshal agent_params for step %q: %w", stepName, perr)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO steps(id, workflow_id, name, agent_id, seq, agent_params, max_visits) VALUES (?, ?, ?, ?, ?, ?, ?)
		`, p.stepIDs[stepName], p.workflowID, stepName, p.stepAgents[stepName], seq, paramsJSON, sd.MaxVisits); err != nil {
			return fmt.Errorf("insert step %q: %w", stepName, err)
		}
	}
	for _, stepName := range p.names {
		sd := p.def.Steps[stepName]
		for i, tr := range sd.NextSteps {
			var nextID, status any
			if tr.IsTaskStatus() {
				nextID = nil
				status = tr.TaskStatus
			} else {
				nid, ok := p.stepIDs[tr.Step]
				if !ok {
					return fmt.Errorf("internal: transition %d in step %q targets unknown step %q", i, stepName, tr.Step)
				}
				nextID = nid
				status = nil
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO step_transitions(step_id, next_step_id, task_status, prompt_rule)
				VALUES (?, ?, ?, ?)
			`, p.stepIDs[stepName], nextID, status, tr.PromptRule); err != nil {
				return fmt.Errorf("insert transition %d for step %q: %w", i, stepName, err)
			}
		}
	}
	return nil
}

func (s *Store) workflowReferenceCount(ctx context.Context, workflowID string) (int, error) {
	var taskRefs int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks WHERE workflow_id = ?`, workflowID).Scan(&taskRefs); err != nil {
		return 0, fmt.Errorf("count workflow task refs: %w", err)
	}
	var runRefs int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM daemon_runs r
		  JOIN steps st ON st.id = r.step_id
		 WHERE st.workflow_id = ?`, workflowID).Scan(&runRefs); err != nil {
		return 0, fmt.Errorf("count workflow run refs: %w", err)
	}
	var revisionRefs int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workflows WHERE superseded_by_id = ?`, workflowID).Scan(&revisionRefs); err != nil {
		return 0, fmt.Errorf("count workflow revision refs: %w", err)
	}
	return taskRefs + runRefs + revisionRefs, nil
}

func insertOriginTx(ctx context.Context, tx *sql.Tx, o Origin, active bool) error {
	metadata, err := marshalOriginMetadata(o.SourceMetadata)
	if err != nil {
		return err
	}
	activeInt := 0
	if active {
		activeInt = 1
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workflow_origins(
			workflow_id, source_type, source, source_metadata,
			definition_hash, revision, active, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.WorkflowID, strings.TrimSpace(o.SourceType), strings.TrimSpace(o.Source), metadata,
		strings.TrimSpace(o.DefinitionHash), strings.TrimSpace(o.Revision), activeInt, now, now); err != nil {
		return fmt.Errorf("insert workflow origin: %w", err)
	}
	return nil
}

func setOriginActiveTx(ctx context.Context, tx *sql.Tx, workflowID string, active bool) error {
	activeInt := 0
	if active {
		activeInt = 1
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE workflow_origins SET active = ?, updated_at = ? WHERE workflow_id = ?`,
		activeInt, time.Now().Unix(), workflowID)
	if err != nil {
		return fmt.Errorf("update workflow origin active: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
