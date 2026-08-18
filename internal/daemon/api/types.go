// Package api defines the daemon's proto-v2 JSON-RPC wire types.
//
// These mirror the single source of truth in daemon/sdk/src/{types,transcript,
// proto}.ts. Every field that crosses the wire is snake_case and every
// timestamp is RFC3339 UTC (decoded straight into time.Time). The Go CLI + lazy
// TUI are pure JSON-RPC clients of autoskd; they never open a store, so these
// are read-only projections the daemon computes server-side.
//
// User-facing rendering (CLI text, TUI panes) routes through
// internal/timeformat; only the machine wire shapes here stay RFC3339 UTC.
package api

import (
	"encoding/json"
	"time"
)

// ---------------------------------------------------------------------------
// Task domain (types.ts).
// ---------------------------------------------------------------------------

// TaskStatus is the five-status task enum, unchanged from v1.
type TaskStatus string

const (
	StatusNew    TaskStatus = "new"
	StatusWork   TaskStatus = "work"
	StatusHuman  TaskStatus = "human"
	StatusDone   TaskStatus = "done"
	StatusCancel TaskStatus = "cancel"
)

// TaskRef is a lightweight reference to a related task (dependency edges).
type TaskRef struct {
	ID     string     `json:"id"`
	Status TaskStatus `json:"status"`
}

// TaskView is the enriched task view (plan §3.1). v2 drops priority and
// author_id. `blocked`/`blocks` are derived server-side. `workflow`/`step`
// are JSON null until the task is enrolled (decoded as ""). Metadata is the
// free-form, human-editable bag (always present; {} when none); the engine
// reserves the `step_visits` sub-object inside it.
type TaskView struct {
	ID           string         `json:"id"`
	Title        string         `json:"title"`
	Description  string         `json:"description"`
	Status       TaskStatus     `json:"status"`
	Workflow     string         `json:"workflow"`
	Step         string         `json:"step"`
	Blocked      bool           `json:"blocked"`
	BlockedBy    []TaskRef      `json:"blocked_by"`
	Blocks       []TaskRef      `json:"blocks"`
	CommentCount int            `json:"comment_count"`
	Metadata     map[string]any `json:"metadata"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

// Comment is one comment on a task (plan §3.1). v2 ids are strings (cm-…) and
// the author is a single rendered string. Comments are editable/deletable.
type Comment struct {
	ID        string    `json:"id"`
	Author    string    `json:"author"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TaskFilter narrows task.list. A nil Status sends no status filter; the daemon
// has no priority/author/search/limit filters in v2 (those are dropped).
type TaskFilter struct {
	Status   []TaskStatus `json:"status,omitempty"`
	Workflow string       `json:"workflow,omitempty"`
	Step     string       `json:"step,omitempty"`
	Blocked  *bool        `json:"blocked,omitempty"`
}

// ---------------------------------------------------------------------------
// Session domain (types.ts §3.2).
// ---------------------------------------------------------------------------

// SessionStatus is the session lifecycle state.
type SessionStatus string

const (
	SessionQueued  SessionStatus = "queued"
	SessionRunning SessionStatus = "running"
	SessionDone    SessionStatus = "done"
	SessionFailed  SessionStatus = "failed"
	SessionAborted SessionStatus = "aborted"
)

// SessionKind is a session's origin: a scheduler-claimed task/workflow step, or
// an interactive (taskless) chat. For an interactive session TaskID/Workflow/
// Step are the empty-string sentinel.
type SessionKind string

const (
	SessionTask        SessionKind = "task"
	SessionInteractive SessionKind = "interactive"
)

// SessionActivity is the live turn state of an interactive session: "busy" while
// the agent streams a turn, "idle" when it waits for the next user message.
// Empty for task sessions and once the session is terminal. Orthogonal to
// SessionStatus (the lifecycle); only meaningful while Status == "running".
type SessionActivity string

const (
	SessionIdle SessionActivity = "idle"
	SessionBusy SessionActivity = "busy"
)

// Usage is authoritative token and cost accounting reported by an agent for a
// completed provider turn, or the aggregate of those reports for one session.
type Usage struct {
	Input       int64     `json:"input"`
	Output      int64     `json:"output"`
	CacheRead   int64     `json:"cacheRead"`
	CacheWrite  int64     `json:"cacheWrite"`
	TotalTokens int64     `json:"totalTokens"`
	Cost        UsageCost `json:"cost"`
}

// UsageCost is the monetary-cost portion of Usage.
type UsageCost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	Total      float64 `json:"total"`
}

// SessionMeta is one session's metadata (replaces the v1 Job). Listing a task's
// sessions = filtering metas by task_id.
type SessionMeta struct {
	ID   string      `json:"id"`
	Kind SessionKind `json:"kind"`
	// TaskID/Workflow/Step are "" for an interactive (taskless) session.
	TaskID    string          `json:"task_id"`
	Workflow  string          `json:"workflow"`
	Step      string          `json:"step"`
	Agent     string          `json:"agent"`
	Status    SessionStatus   `json:"status"`
	Activity  SessionActivity `json:"activity,omitempty"`
	Error     string          `json:"error,omitempty"`
	StartedAt *time.Time      `json:"started_at"`
	EndedAt   *time.Time      `json:"ended_at"`
	Usage     *Usage          `json:"usage,omitempty"`
}

// ---------------------------------------------------------------------------
// Registry domain (types.ts / registry.workflow.*).
// ---------------------------------------------------------------------------

// StepTarget mirrors the workflow StepTarget union ({step} XOR {status}).
// Exactly one field is set on the wire.
type StepTarget struct {
	Step   string `json:"step,omitempty"`
	Status string `json:"status,omitempty"`
}

// WorkflowStepInfo is one step of a workflow as rendered from code.
type WorkflowStepInfo struct {
	Name string `json:"name"`
	// Status is the terminal/park status for a statusStep ("done"/"cancel"/
	// "human"), or nil/absent for an agent step (whose Name is the agent name).
	Status  *string      `json:"status"`
	Targets []StepTarget `json:"targets"`
}

// WorkflowInfo is a workflow rendered from code (read-only projection).
type WorkflowInfo struct {
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	FirstStep   string             `json:"first_step"`
	Steps       []WorkflowStepInfo `json:"steps"`
}

// ---------------------------------------------------------------------------
// Meta / project (proto.ts).
// ---------------------------------------------------------------------------

// VersionInfo is meta.version.
type VersionInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// OkResult is the generic success acknowledgement.
type OkResult struct {
	OK bool `json:"ok"`
}

// HealthProject is one project in the aggregated health view.
type HealthProject struct {
	Root     string    `json:"root"`
	Queued   int       `json:"queued"`
	Running  int       `json:"running"`
	OpenedAt time.Time `json:"opened_at"`
}

// Health is meta.healthz. v2 drops the scoped db_path/project_root fields; the
// view is always the cross-project aggregate under Projects.
type Health struct {
	OK       bool            `json:"ok"`
	Workers  int             `json:"workers"`
	Queued   int             `json:"queued"`
	Running  int             `json:"running"`
	Projects []HealthProject `json:"projects"`
}

// ProjectInfo is a project in the persisted registry (~/.autosk/projects.json).
// v2 drops db_path — there is no database.
type ProjectInfo struct {
	Root string `json:"root"`
	Name string `json:"name"`
}

// ExtensionLoadError is one extension load error surfaced via project.diagnostics.
type ExtensionLoadError struct {
	Source string `json:"source"`
	Error  string `json:"error"`
}

// ProjectDiagnostics is the project.diagnostics result.
type ProjectDiagnostics struct {
	Root       string               `json:"root"`
	Extensions []ExtensionLoadError `json:"extensions"`
}

// ---------------------------------------------------------------------------
// Extension management (autosk ext) — proto.ts extension.* methods.
// ---------------------------------------------------------------------------

// ExtensionInstallResult is the extension.install result. Scope is
// "global"|"project"; Source is the canonical settings entry written
// (npm:<spec> | <abs-path>); Installed reports whether an npm install ran
// (false for a local-path source). Reloaded/ReloadedProjects report the live
// hot-reload applied to open projects (no daemon restart): Reloaded is true
// when ≥1 open project's registry was rebuilt, ReloadedProjects is the count.
type ExtensionInstallResult struct {
	Scope            string `json:"scope"`
	Source           string `json:"source"`
	SettingsPath     string `json:"settings_path"`
	Installed        bool   `json:"installed"`
	Reloaded         bool   `json:"reloaded"`
	ReloadedProjects int    `json:"reloaded_projects"`
}

// ExtensionRemoveResult is the extension.remove result. Removed reports whether
// a matching settings entry was dropped (node_modules is left untouched).
// Reloaded/ReloadedProjects mirror ExtensionInstallResult (live hot-reload of
// open projects, no restart).
type ExtensionRemoveResult struct {
	Scope            string `json:"scope"`
	Source           string `json:"source"`
	SettingsPath     string `json:"settings_path"`
	Removed          bool   `json:"removed"`
	Reloaded         bool   `json:"reloaded"`
	ReloadedProjects int    `json:"reloaded_projects"`
}

// ExtensionReloadParkedTask is one task parked by a hot-reload because its
// workflow/step vanished from the rebuilt registry.
type ExtensionReloadParkedTask struct {
	TaskID string `json:"task_id"`
	Error  string `json:"error"`
}

// ExtensionReloadResult is the extension.reload result: the rebuilt project's
// root, the new registry's load diagnostics, its registered workflow names, and
// any non-live work tasks parked because their workflow/step disappeared.
type ExtensionReloadResult struct {
	Root        string                      `json:"root"`
	Diagnostics []ExtensionLoadError        `json:"diagnostics"`
	Workflows   []string                    `json:"workflows"`
	Parked      []ExtensionReloadParkedTask `json:"parked"`
}

// ExtensionEntryInfo is one classified settings.json#extensions entry
// (extension.list). Kind is "npm"|"local"|"invalid"; Resolved reports whether it
// currently resolves to a loadable extension.
type ExtensionEntryInfo struct {
	Source   string `json:"source"`
	Scope    string `json:"scope"`
	Kind     string `json:"kind"`
	Resolved bool   `json:"resolved"`
}

// ExtensionListResult is the extension.list result.
type ExtensionListResult struct {
	Entries []ExtensionEntryInfo `json:"entries"`
}

// ExtensionUpdateEntry is one extension considered by extension.update. Status
// is updated|up-to-date|failed (real run), available|unknown (dry-run), or
// skipped (version-pinned npm / local-path entry). FromVersion is the installed
// version before; ToVersion the latest (or installed-after on a real update);
// Reason explains a skip/failure.
type ExtensionUpdateEntry struct {
	Source      string `json:"source"`
	Name        string `json:"name"`
	Scope       string `json:"scope"`
	Status      string `json:"status"`
	FromVersion string `json:"from_version,omitempty"`
	ToVersion   string `json:"to_version,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// ExtensionUpdateResult is the extension.update result. DryRun reports whether
// the run installed nothing; Changed reports whether anything was actually
// updated (drives the daemon restart hint).
type ExtensionUpdateResult struct {
	Entries []ExtensionUpdateEntry `json:"entries"`
	DryRun  bool                   `json:"dry_run"`
	Changed bool                   `json:"changed"`
}

// ---------------------------------------------------------------------------
// Notification payloads (proto.ts §4).
// ---------------------------------------------------------------------------

// TaskChangedParams is the `task-changed` payload (v2 carries the task view).
type TaskChangedParams struct {
	Root string   `json:"root"`
	Task TaskView `json:"task"`
}

// ProjectChangedParams is the `project-changed` payload.
type ProjectChangedParams struct {
	Project ProjectInfo `json:"project"`
}

// SessionEventParams is the `session-event` payload. Kind names the frame
// variant: message | status | done | error | partial. A `partial` frame carries
// an ephemeral, cumulative in-progress assistant snapshot in Partial; it is
// never persisted, carries no Line, and does not advance the subscription
// cursor (the eventual committed `message` frame supersedes it). Live rendering
// of partials in the lazy TUI is a follow-up; for now an unknown-to-render
// partial frame is simply ignored.
type SessionEventParams struct {
	Root      string             `json:"root"`
	SessionID string             `json:"session_id"`
	Kind      string             `json:"kind"`
	Event     *TranscriptLine    `json:"event,omitempty"`
	Session   *SessionMeta       `json:"session,omitempty"`
	Error     string             `json:"error,omitempty"`
	Line      int                `json:"line,omitempty"`
	Partial   *TranscriptMessage `json:"partial,omitempty"`
}

// SessionChangedParams is the `session-changed` payload: a project-scoped push
// of one session's metadata whenever it is created or changes status (queued →
// running → terminal). Delivered to connections that issued
// session.subscribeProject. Unlike SessionEventParams it never carries a
// transcript line — only the decorated SessionMeta — so a client keeps its
// session list live without subscribing to each session id.
type SessionChangedParams struct {
	Root    string      `json:"root"`
	Session SessionMeta `json:"session"`
}

// RegistryChangedParams is the `registry-changed` payload: a project's extension
// registry was hot-reloaded (an ext add/remove/reload rebuilt + swapped it).
// Carries only the affected Root; subscribers re-fetch registry.workflow.list /
// extension.list / project.diagnostics to refresh their view.
type RegistryChangedParams struct {
	Root string `json:"root"`
}

// ---------------------------------------------------------------------------
// Session transcript (transcript.ts §3.2) — pi-format entries.
// ---------------------------------------------------------------------------

// SessionTranscriptResult is the session.transcript result.
type SessionTranscriptResult struct {
	Entries []TranscriptLine `json:"entries"`
	// NextLine is the cursor to pass as the next from_line for tailing.
	NextLine int `json:"next_line"`
}

// TranscriptLine is any line of a transcript file: the header (line 1), a
// pi-format message entry, or a custom entry (incl. the engine's autosk:*
// structural entries). Modelled as a single flat struct keyed on Type so the
// renderer can switch on it without a custom unmarshaller; unmodelled fields
// survive in Raw for future use.
type TranscriptLine struct {
	// Type is "session" (header) | "message" | "custom".
	Type string `json:"type"`

	// ---- header (Type == "session") ----
	Version  int    `json:"version,omitempty"`
	TaskID   string `json:"task_id,omitempty"`
	Workflow string `json:"workflow,omitempty"`
	Step     string `json:"step,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Cwd      string `json:"cwd,omitempty"`

	// ---- entry base (Type != "session") ----
	ID string `json:"id,omitempty"`
	// Timestamp is RFC3339 UTC on the header + every entry base.
	Timestamp string `json:"timestamp,omitempty"`

	// ---- message entry (Type == "message") ----
	Message *TranscriptMessage `json:"message,omitempty"`

	// ---- custom entry (Type == "custom") ----
	CustomType string          `json:"customType,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// TranscriptMessage is a pi message-schema entry. Content is a string (user) or
// an array of content blocks; kept raw and flattened via Text().
type TranscriptMessage struct {
	Role    string          `json:"role"` // user | assistant | toolResult
	Content json.RawMessage `json:"content"`
	// Unix timestamp in milliseconds (pi message schema).
	Timestamp int64 `json:"timestamp,omitempty"`

	// assistant
	Provider   string `json:"provider,omitempty"`
	Model      string `json:"model,omitempty"`
	StopReason string `json:"stopReason,omitempty"`

	// toolResult
	ToolCallID string `json:"toolCallId,omitempty"`
	ToolName   string `json:"toolName,omitempty"`
	IsError    bool   `json:"isError,omitempty"`
}

// ContentBlock is one block of pi message content.
type ContentBlock struct {
	Type      string         `json:"type"` // text | thinking | image | toolCall
	Text      string         `json:"text,omitempty"`
	Thinking  string         `json:"thinking,omitempty"`
	MimeType  string         `json:"mimeType,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// Blocks decodes the message content into typed blocks. A user message whose
// content is a bare string is returned as a single text block.
func (m *TranscriptMessage) Blocks() []ContentBlock {
	if m == nil || len(m.Content) == 0 {
		return nil
	}
	// Bare string content (user messages).
	if m.Content[0] == '"' {
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			return []ContentBlock{{Type: "text", Text: s}}
		}
		return nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil
	}
	return blocks
}

// Text flattens every text/thinking block (and bare-string content) into one
// string, for compact transcript rendering.
func (m *TranscriptMessage) Text() string {
	var out []string
	for _, b := range m.Blocks() {
		switch b.Type {
		case "text":
			if b.Text != "" {
				out = append(out, b.Text)
			}
		case "thinking":
			if b.Thinking != "" {
				out = append(out, b.Thinking)
			}
		}
	}
	if len(out) == 0 {
		return ""
	}
	s := out[0]
	for _, x := range out[1:] {
		s += "\n" + x
	}
	return s
}
