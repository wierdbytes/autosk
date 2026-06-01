-- 002_workflow_origins.sql — provenance for workflow definitions.
--
-- Tracks where a project workflow came from (global registry, package,
-- local file, bootstrap, etc.), the canonical definition hash/revision,
-- arbitrary source metadata, and whether that origin is currently active.

CREATE TABLE workflow_origins (
  workflow_id     TEXT PRIMARY KEY REFERENCES workflows(id) ON DELETE CASCADE,
  source_type     TEXT NOT NULL,
  source          TEXT NOT NULL DEFAULT '',
  source_metadata TEXT,
  definition_hash TEXT NOT NULL DEFAULT '',
  revision        TEXT NOT NULL DEFAULT '',
  active          INTEGER NOT NULL DEFAULT 1
                  CHECK (active IN (0,1)),
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);

CREATE INDEX idx_workflow_origins_source ON workflow_origins(source_type, source);
CREATE INDEX idx_workflow_origins_active ON workflow_origins(active);
