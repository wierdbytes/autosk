// Package workflow owns the workflows / steps / step_transitions tables
// and the JSON definition format consumed by `autosk workflow create
// --file`. See docs/plans/20260517-Workflows-Plan.md §§4.1 and §6.2.
package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SyntheticPrefix is reserved for auto-generated `single:<agent>` workflows
// produced by EnsureSingle. User-defined workflows whose name starts with
// this prefix are rejected at parse time.
const SyntheticPrefix = "single:"

// Definition is the in-memory shape of a workflow JSON file. Steps are a
// map so callers can navigate by name; transition order within a step
// matches the source file order.
type Definition struct {
	Name        string
	Description string
	FirstStep   string
	// Isolation is the workflow-level execution-isolation mode. Default
	// IsolationNone preserves prior behaviour (steps spawn with
	// cwd=projectRoot). IsolationWorktree allocates a git worktree per
	// task at create/enroll time and spawns step runs inside it. See
	// docs/plans/20260521-Worktree-Isolation.md.
	Isolation IsolationMode
	Steps     map[string]StepDef
	// StepNames preserves source-file step order (helpful for stable
	// rendering / `workflow show` output).
	StepNames []string
}

// IsolationMode is the closed set of workflow-level isolation modes.
// Mirrors the CHECK in migration 005_workflow_isolation.sql.
type IsolationMode string

const (
	// IsolationNone (default) → steps spawn with cwd=projectRoot.
	IsolationNone IsolationMode = "none"
	// IsolationWorktree → CLI allocates a git worktree per task and the
	// daemon spawns step runs inside it.
	IsolationWorktree IsolationMode = "worktree"
)

// Valid reports whether m is one of the accepted modes.
func (m IsolationMode) Valid() bool {
	switch m {
	case IsolationNone, IsolationWorktree:
		return true
	}
	return false
}

// Normalize collapses the zero / empty value to IsolationNone.
func (m IsolationMode) Normalize() IsolationMode {
	if m == "" {
		return IsolationNone
	}
	return m
}

// StepDef is one entry under "steps".
//
// AgentName is the agent's npm package name (the `name` field of the
// step's `agent` object in the JSON file). AgentParams carries per-step
// overrides for the agent's package config; nil means "use the package's
// defaults verbatim". An empty (zero-valued) AgentParams is also treated
// as nil.
type StepDef struct {
	AgentName   string
	AgentParams *AgentParams
	NextSteps   []TransitionDef
	// MaxVisits caps how many times a single task may enter this step
	// across one workflow run. 0 (the default) means "unlimited". See
	// docs/plans/20260520-Step-Visit-Limits.md and docs/workflows.md.
	MaxVisits int
}

// AgentParams overrides the standard agent-package config fields per
// step. See docs/workflows.md for merge semantics. Pointer scalars
// disambiguate "absent" (nil → use package default) from "explicitly
// set" (non-nil — even when the value is the empty string).
//
// Arrays use nil → absent and non-nil → replace-the-package-default
// semantics. Note that, because the persisted JSON uses `omitempty`,
// a round-trip through the DB collapses an explicit empty slice into
// "absent"; this is intentional MVP behaviour. If you need to clear
// a package's default array, edit the package instead.
//
// FirstMessageFile is a *parse-time only* field: ParseFile inlines its
// contents into FirstMessage (resolved relative to the workflow JSON
// file) and zeroes the path so the field is never persisted to the DB.
// ParseReader rejects it because it has no base directory to resolve
// against.
//
// The `runner` field is intentionally NOT supported — agent overrides
// can only change standard-agent fields. Use the package's
// `autosk.agent.runner` for custom runners.
type AgentParams struct {
	Model            *string  `json:"model,omitempty"`
	Thinking         *string  `json:"thinking,omitempty"`
	FirstMessage     *string  `json:"first_message,omitempty"`
	FirstMessageFile string   `json:"first_message_file,omitempty"`
	ExtraArgs        []string `json:"extra_args,omitempty"`
	PiExtensions     []string `json:"pi_extensions,omitempty"`
	PiSkills         []string `json:"pi_skills,omitempty"`
}

// allowedParamKeys is the closed set of JSON keys accepted inside a
// step's `agent.params` block. Anything else (most notably `runner`)
// is rejected at parse time so misuse fails loudly.
var allowedParamKeys = map[string]struct{}{
	"model":              {},
	"thinking":           {},
	"first_message":      {},
	"first_message_file": {},
	"extra_args":         {},
	"pi_extensions":      {},
	"pi_skills":          {},
}

// IsZero reports whether the params block carries no overrides at all.
// Useful for the executor's "package has runner + params set" guard.
func (p *AgentParams) IsZero() bool {
	if p == nil {
		return true
	}
	return p.Model == nil && p.Thinking == nil && p.FirstMessage == nil &&
		p.FirstMessageFile == "" && p.ExtraArgs == nil &&
		p.PiExtensions == nil && p.PiSkills == nil
}

// TransitionDef is one entry under "steps.<name>.next_steps". Exactly one
// of Step and TaskStatus is set; the parser enforces that.
type TransitionDef struct {
	Step       string // target step name
	TaskStatus string // one of human|done|cancel
	PromptRule string
}

// IsTaskStatus reports whether this transition terminates the workflow
// (or routes to human) rather than advancing to a sibling step.
func (t TransitionDef) IsTaskStatus() bool { return t.TaskStatus != "" }

// rawDoc mirrors the on-disk shape one-to-one for json.Unmarshal.
type rawDoc struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	FirstStep   string             `json:"first_step"`
	Isolation   string             `json:"isolation,omitempty"`
	Steps       map[string]rawStep `json:"steps"`
}

type rawStep struct {
	// Agent is the per-step agent object: `{ "name": "...", "params": {...} }`.
	// The previous string-only form (`"agent": "name"`) is no longer
	// accepted; see parseAgentRef for the diagnostic.
	Agent     json.RawMessage `json:"agent"`
	NextSteps []rawTransition `json:"next_steps"`
	MaxVisits int             `json:"max_visits,omitempty"`
}

type rawAgentRef struct {
	Name   string          `json:"name"`
	Params json.RawMessage `json:"params,omitempty"`
}

type rawTransition struct {
	Step       string `json:"step,omitempty"`
	TaskStatus string `json:"task_status,omitempty"`
	PromptRule string `json:"prompt_rule"`
}

// ParseFile is a convenience over ParseReader that also resolves
// per-step `first_message_file` paths relative to the workflow JSON
// file's directory (and inlines their contents into FirstMessage).
func ParseFile(path string) (Definition, error) {
	f, err := os.Open(path)
	if err != nil {
		return Definition{}, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	def, err := parseReader(f, false /*rejectFirstMessageFile*/)
	if err != nil {
		return Definition{}, err
	}
	if err := resolveAgentParamFiles(&def, filepath.Dir(path)); err != nil {
		return Definition{}, err
	}
	return def, nil
}

// ParseReader reads a JSON workflow definition from r and returns its
// in-memory form. Performs only structural / format checks; deeper
// validation (agent existence, reserved name, etc.) is in Validate.
//
// ParseReader rejects per-step `first_message_file` because it has no
// base directory to resolve the path against. Callers with a path on
// disk should use ParseFile instead.
func ParseReader(r io.Reader) (Definition, error) {
	return parseReader(r, true /*rejectFirstMessageFile*/)
}

func parseReader(r io.Reader, rejectFirstMessageFile bool) (Definition, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return Definition{}, fmt.Errorf("read workflow: %w", err)
	}
	// First pass: standard unmarshal.
	var raw rawDoc
	if err := json.Unmarshal(body, &raw); err != nil {
		return Definition{}, fmt.Errorf("decode workflow: %w", err)
	}

	isoRaw := strings.TrimSpace(raw.Isolation)
	isolation := IsolationMode(isoRaw).Normalize()
	if !isolation.Valid() {
		return Definition{}, fmt.Errorf("unknown isolation mode: %q (want none|worktree)", isoRaw)
	}
	def := Definition{
		Name:        strings.TrimSpace(raw.Name),
		Description: strings.TrimSpace(raw.Description),
		FirstStep:   strings.TrimSpace(raw.FirstStep),
		Isolation:   isolation,
		Steps:       make(map[string]StepDef, len(raw.Steps)),
	}
	for stepName, s := range raw.Steps {
		name, params, perr := parseAgentRef(stepName, s.Agent)
		if perr != nil {
			return Definition{}, perr
		}
		if rejectFirstMessageFile && params != nil && params.FirstMessageFile != "" {
			return Definition{}, fmt.Errorf("step %q: agent.params.first_message_file requires a workflow file path; use ParseFile or move the prompt into `first_message`", stepName)
		}
		stepDef := StepDef{AgentName: name, AgentParams: params, MaxVisits: s.MaxVisits}
		for i, tr := range s.NextSteps {
			step := strings.TrimSpace(tr.Step)
			status := strings.TrimSpace(tr.TaskStatus)
			rule := strings.TrimSpace(tr.PromptRule)
			// Exactly-one constraint expressed here so Validate can focus
			// on referential checks.
			if (step == "") == (status == "") {
				return Definition{}, fmt.Errorf("step %q transition %d: exactly one of `step` or `task_status` must be set", stepName, i)
			}
			stepDef.NextSteps = append(stepDef.NextSteps, TransitionDef{
				Step:       step,
				TaskStatus: status,
				PromptRule: rule,
			})
		}
		def.Steps[stepName] = stepDef
	}

	// Recover step source order from the raw bytes.
	def.StepNames, err = recoverStepOrder(body)
	if err != nil {
		return Definition{}, err
	}
	return def, nil
}

// parseAgentRef decodes the `agent` field of one step. The accepted
// shape is the object form `{ "name": "...", "params": {...} }`; the
// legacy string form (`"agent": "name"`) is rejected with a hint.
func parseAgentRef(stepName string, raw json.RawMessage) (string, *AgentParams, error) {
	if len(raw) == 0 {
		return "", nil, fmt.Errorf("step %q: `agent` is required", stepName)
	}
	// Quick-discriminate: leading non-whitespace token.
	trimmed := strings.TrimLeft(string(raw), " \t\n\r")
	if trimmed == "" {
		return "", nil, fmt.Errorf("step %q: empty `agent`", stepName)
	}
	if trimmed[0] == '"' {
		return "", nil, fmt.Errorf("step %q: `agent` must be an object `{ \"name\": \"...\", \"params\": {...} }`; the bare-string form is no longer supported", stepName)
	}
	if trimmed[0] != '{' {
		return "", nil, fmt.Errorf("step %q: `agent` must be an object, got %s", stepName, firstRune(trimmed))
	}
	var ref rawAgentRef
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ref); err != nil {
		return "", nil, fmt.Errorf("step %q: parse agent object: %w", stepName, err)
	}
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return "", nil, fmt.Errorf("step %q: agent.name is required", stepName)
	}
	if len(ref.Params) == 0 {
		return name, nil, nil
	}
	params, perr := parseAgentParams(stepName, ref.Params)
	if perr != nil {
		return "", nil, perr
	}
	return name, params, nil
}

// parseAgentParams decodes the params object with strict unknown-key
// rejection so misuse (e.g. `runner`, typos) fails loudly.
func parseAgentParams(stepName string, raw json.RawMessage) (*AgentParams, error) {
	// First validate the key set against the whitelist. Decoding into
	// the typed struct with DisallowUnknownFields would also catch
	// unknown keys, but a custom check lets us spell out the closed
	// set in the error message.
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &asMap); err != nil {
		return nil, fmt.Errorf("step %q: agent.params must be an object: %w", stepName, err)
	}
	for k := range asMap {
		if _, ok := allowedParamKeys[k]; !ok {
			return nil, fmt.Errorf("step %q: agent.params has unknown key %q (allowed: %s)",
				stepName, k, allowedParamKeysList())
		}
	}
	var p AgentParams
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("step %q: parse agent.params: %w", stepName, err)
	}
	if p.IsZero() {
		return nil, nil
	}
	return &p, nil
}

func allowedParamKeysList() string {
	keys := make([]string, 0, len(allowedParamKeys))
	for k := range allowedParamKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// resolveAgentParamFiles reads any per-step `first_message_file` from
// disk (paths are interpreted relative to baseDir) and inlines the
// contents into FirstMessage. After this call, no step's params should
// have a non-empty FirstMessageFile.
//
// Errors out for absolute paths and paths that escape baseDir, mirror
// of pkgregistry.resolveInsidePkg. The containment check is both lexical
// (`..`) and symlink-aware so packaged workflows cannot inline host files
// outside the workflow JSON directory.
func resolveAgentParamFiles(def *Definition, baseDir string) error {
	root, err := filepath.Abs(baseDir)
	if err != nil {
		return err
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve workflow directory %q: %w", root, err)
	}
	for stepName, sd := range def.Steps {
		if sd.AgentParams == nil || sd.AgentParams.FirstMessageFile == "" {
			continue
		}
		if sd.AgentParams.FirstMessage != nil {
			return fmt.Errorf("step %q: agent.params has both first_message and first_message_file (pick one)", stepName)
		}
		path := sd.AgentParams.FirstMessageFile
		if filepath.IsAbs(path) {
			return fmt.Errorf("step %q: agent.params.first_message_file must be a relative path inside the workflow file's directory, got %q", stepName, path)
		}
		abs, err := resolveInsideWorkflowDir(root, rootReal, path)
		if err != nil {
			return fmt.Errorf("step %q: agent.params.first_message_file %q: %w", stepName, path, err)
		}
		body, err := os.ReadFile(abs)
		if err != nil {
			return fmt.Errorf("step %q: read first_message_file %q: %w", stepName, abs, err)
		}
		s := string(body)
		sd.AgentParams.FirstMessage = &s
		sd.AgentParams.FirstMessageFile = ""
		def.Steps[stepName] = sd
	}
	return nil
}

func resolveInsideWorkflowDir(root, rootReal, rel string) (string, error) {
	clean, err := filepath.Abs(filepath.Join(root, rel))
	if err != nil {
		return "", err
	}
	if !pathInside(root, clean) {
		return "", fmt.Errorf("path escapes workflow file's directory")
	}
	real, err := filepath.EvalSymlinks(clean)
	if err == nil {
		if !pathInside(rootReal, real) {
			return "", fmt.Errorf("path escapes workflow file's directory through symlink")
		}
		return clean, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("resolve first_message_file symlinks: %w", err)
	}
	// Let os.ReadFile below report the concrete missing-file error.
	return clean, nil
}

func pathInside(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !hasParentPathPrefix(rel)
}

func hasParentPathPrefix(p string) bool {
	if len(p) < 2 {
		return false
	}
	return p[0] == '.' && p[1] == '.' && (len(p) == 2 || p[2] == filepath.Separator || p[2] == '/')
}

func firstRune(s string) string {
	if s == "" {
		return "(empty)"
	}
	for _, r := range s {
		return fmt.Sprintf("%q", r)
	}
	return "(empty)"
}

// recoverStepOrder rescans the JSON body to extract the textual order of
// step names. This lets `workflow show` print steps the way the author
// wrote them rather than in random map order.
func recoverStepOrder(body []byte) ([]string, error) {
	var probe struct {
		Steps json.RawMessage `json:"steps"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("decode steps for order: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(probe.Steps)))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil // empty / missing steps; Validate will complain
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, errors.New("`steps` must be an object")
	}
	var names []string
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("decode step name: %w", err)
		}
		key, ok := t.(string)
		if !ok {
			return nil, fmt.Errorf("step name token: %v", t)
		}
		names = append(names, key)
		// Skip the step's value (an object).
		if err := skipValue(dec); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// skipValue advances the decoder past one JSON value. Handles nested
// objects and arrays.
func skipValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := tok.(json.Delim); ok {
		// Balance braces/brackets.
		depth := 1
		for depth > 0 {
			t, err := dec.Token()
			if err != nil {
				return err
			}
			if d, ok := t.(json.Delim); ok {
				switch d {
				case '{', '[':
					depth++
				case '}', ']':
					depth--
				}
			}
			_ = delim
		}
	}
	return nil
}
