-- 003_workflow_revisions.sql — safe supersession links for workflow revisions.
--
-- Managed workflow updates may need to keep an old workflow row alive when
-- tasks or daemon runs still reference its steps. The old row is renamed to an
-- internal revision name, the canonical name is reused by the new active row,
-- and this nullable link records the successor.

ALTER TABLE workflows
  ADD COLUMN superseded_by_id TEXT REFERENCES workflows(id) ON DELETE SET NULL;

CREATE INDEX idx_workflows_superseded_by ON workflows(superseded_by_id);
