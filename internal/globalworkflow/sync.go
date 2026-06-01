package globalworkflow

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"autosk/internal/agent"
	"autosk/internal/agent/pkgregistry"
	"autosk/internal/workflow"
)

// SyncStatus is the per-workflow outcome emitted by SyncGlobalWorkflows.
type SyncStatus string

const (
	SyncAdded    SyncStatus = "added"
	SyncNoop     SyncStatus = "noop"
	SyncSkipped  SyncStatus = "skipped"
	SyncConflict SyncStatus = "conflict"
	SyncUpdated  SyncStatus = "updated"
	SyncError    SyncStatus = "error"
)

// ErrSyncFailed is returned when at least one enabled global workflow could
// not be loaded, installed, or materialized. The returned SyncReport still
// carries every per-workflow outcome observed before/after the failure.
var ErrSyncFailed = errors.New("global workflow sync failed")

// InstallAgentsFunc installs any missing agents referenced by def and returns
// the package registry entries installed by this sync run.
type InstallAgentsFunc func(ctx context.Context, def workflow.Definition) ([]pkgregistry.Entry, error)

// SyncOptions controls project materialization of enabled global workflows.
type SyncOptions struct {
	DryRun        bool
	Force         bool
	InstallAgents InstallAgentsFunc
}

// SyncReport is the structured outcome for one sync run.
type SyncReport struct {
	Prefix    string               `json:"prefix"`
	DryRun    bool                 `json:"dry_run"`
	Force     bool                 `json:"force"`
	Workflows []SyncWorkflowReport `json:"workflows"`
}

// Mutated reports whether this non-dry-run sync changed the project DB.
func (r SyncReport) Mutated() bool {
	if r.DryRun {
		return false
	}
	for _, wf := range r.Workflows {
		if wf.Mutated {
			return true
		}
	}
	return false
}

// SyncWorkflowReport is the structured per-workflow sync outcome.
type SyncWorkflowReport struct {
	Name                string              `json:"name"`
	Status              SyncStatus          `json:"status"`
	WorkflowID          string              `json:"workflow_id,omitempty"`
	DefinitionHash      string              `json:"definition_hash,omitempty"`
	PreviousHash        string              `json:"previous_hash,omitempty"`
	Revision            string              `json:"revision,omitempty"`
	Reason              string              `json:"reason,omitempty"`
	Error               string              `json:"error,omitempty"`
	AutoInstalledAgents []pkgregistry.Entry `json:"auto_installed_agents,omitempty"`
	Mutated             bool                `json:"-"`
}

// SyncGlobalWorkflows materializes every enabled workflow from r into the
// project workflow store. Local same-name workflows that are not already
// managed by the matching global registry entry are reported as conflicts and
// are never overwritten. Changed managed workflows are updated only when Force
// is set; the update path uses delete+create and therefore relies on the store's
// normal in-use protections until revisioned updates are available.
func SyncGlobalWorkflows(ctx context.Context, r *Registry, wf *workflow.Store, opts SyncOptions) (SyncReport, error) {
	report := SyncReport{DryRun: opts.DryRun, Force: opts.Force}
	if r != nil {
		report.Prefix = r.Prefix()
	}
	if r == nil {
		return report, fmt.Errorf("%w: registry is nil", ErrSyncFailed)
	}
	if wf == nil {
		return report, fmt.Errorf("%w: workflow store is nil", ErrSyncFailed)
	}
	entries, err := r.List(false)
	if err != nil {
		return report, fmt.Errorf("%w: list enabled global workflows: %w", ErrSyncFailed, err)
	}
	failed := false
	for _, entry := range entries {
		item := SyncWorkflowReport{
			Name:           entry.Name,
			DefinitionHash: entry.DefinitionHash,
			Revision:       entry.Revision,
		}
		def, err := r.LoadDefinition(entry.Name)
		if err != nil {
			item.Status = SyncError
			item.Error = err.Error()
			report.Workflows = append(report.Workflows, item)
			failed = true
			continue
		}
		local, err := wf.GetByName(ctx, entry.Name)
		switch {
		case errors.Is(err, workflow.ErrNotFound):
			item = syncAdd(ctx, wf, opts, item, entry, def)
		case err != nil:
			item.Status = SyncError
			item.Error = fmt.Sprintf("check local workflow: %v", err)
			failed = true
		default:
			item.WorkflowID = local.ID
			item = syncExisting(ctx, wf, opts, item, entry, def, local)
		}
		if item.Status == SyncError {
			failed = true
		}
		report.Workflows = append(report.Workflows, item)
	}
	if failed {
		return report, ErrSyncFailed
	}
	return report, nil
}

func syncAdd(ctx context.Context, wf *workflow.Store, opts SyncOptions, item SyncWorkflowReport, entry Entry, def workflow.Definition) SyncWorkflowReport {
	item.Status = SyncAdded
	item.Reason = "enabled global workflow is absent locally"
	if opts.DryRun {
		if err := validateMaterializationPreflight(ctx, wf, opts, def); err != nil {
			item.Status = SyncError
			item.Error = fmt.Sprintf("validate workflow: %v", err)
		}
		return item
	}
	if err := validateMaterializationPreflight(ctx, wf, opts, def); err != nil {
		item.Status = SyncError
		item.Error = fmt.Sprintf("validate workflow: %v", err)
		return item
	}
	installed, err := installAgents(ctx, opts, def)
	item.AutoInstalledAgents = installed
	if err != nil {
		item.Status = SyncError
		item.Error = err.Error()
		item.Mutated = len(installed) > 0
		return item
	}
	created, err := wf.CreateWithOrigin(ctx, def, false, globalOrigin(entry))
	if err != nil {
		item.Status = SyncError
		item.Error = fmt.Sprintf("create workflow: %v", err)
		item.Mutated = len(installed) > 0
		return item
	}
	item.WorkflowID = created.ID
	item.Mutated = true
	return item
}

func syncExisting(ctx context.Context, wf *workflow.Store, opts SyncOptions, item SyncWorkflowReport, entry Entry, def workflow.Definition, local workflow.Workflow) SyncWorkflowReport {
	origin, err := wf.GetOrigin(ctx, local.ID)
	if errors.Is(err, workflow.ErrNotFound) {
		item.Status = SyncConflict
		item.Reason = "local workflow with same name has no global origin"
		return item
	}
	if err != nil {
		item.Status = SyncError
		item.Error = fmt.Sprintf("load workflow origin: %v", err)
		return item
	}
	item.PreviousHash = origin.DefinitionHash
	if origin.SourceType != "global" || origin.Source != entry.Name {
		item.Status = SyncConflict
		item.Reason = fmt.Sprintf("local workflow is managed by %s source %q", origin.SourceType, origin.Source)
		return item
	}
	if !origin.Active {
		item.Status = SyncSkipped
		item.Reason = "local global origin is inactive"
		return item
	}
	if origin.DefinitionHash == entry.DefinitionHash {
		item.Status = SyncNoop
		item.Reason = "already up to date"
		return item
	}
	item.Status = SyncUpdated
	item.Reason = "global definition hash changed"
	if !opts.Force {
		item.Status = SyncSkipped
		item.Reason = "global definition hash changed; pass --force to update"
		return item
	}
	if err := validateDefinitionStructure(ctx, def); err != nil {
		item.Status = SyncError
		item.Error = fmt.Sprintf("validate workflow: %v", err)
		return item
	}
	if err := wf.CheckReplaceAllowed(ctx, local.Name, def); err != nil {
		item.Status = SyncError
		item.Error = fmt.Sprintf("replace workflow: %v", err)
		return item
	}
	if opts.DryRun {
		if err := validateMaterializationPreflight(ctx, wf, opts, def); err != nil {
			item.Status = SyncError
			item.Error = fmt.Sprintf("validate workflow: %v", err)
		}
		return item
	}
	if err := validateMaterializationPreflight(ctx, wf, opts, def); err != nil {
		item.Status = SyncError
		item.Error = fmt.Sprintf("validate workflow: %v", err)
		return item
	}
	installed, err := installAgents(ctx, opts, def)
	item.AutoInstalledAgents = installed
	if err != nil {
		item.Status = SyncError
		item.Error = err.Error()
		item.Mutated = len(installed) > 0
		return item
	}
	created, err := wf.ReplaceWithOrigin(ctx, local.Name, def, globalOrigin(entry))
	if err != nil {
		item.Status = SyncError
		item.Error = fmt.Sprintf("replace workflow: %v", err)
		item.Mutated = len(installed) > 0
		return item
	}
	item.WorkflowID = created.ID
	item.Mutated = true
	return item
}

func installAgents(ctx context.Context, opts SyncOptions, def workflow.Definition) ([]pkgregistry.Entry, error) {
	if opts.InstallAgents == nil {
		return nil, nil
	}
	installed, err := opts.InstallAgents(ctx, def)
	if err != nil {
		return installed, err
	}
	return installed, nil
}

func validateDefinitionStructure(ctx context.Context, def workflow.Definition) error {
	return workflow.Validate(ctx, def, nil, workflow.ValidateOpts{})
}

func validateMaterializationPreflight(ctx context.Context, wf *workflow.Store, opts SyncOptions, def workflow.Definition) error {
	if err := validateDefinitionStructure(ctx, def); err != nil {
		return err
	}
	ag := wf.Agents()
	if ag == nil {
		return errors.New("workflow store has no agent store")
	}

	var problems []string
	for _, name := range dryRunAgentNames(def) {
		if name == agent.HumanAgentName {
			continue
		}
		if _, err := ag.GetByName(ctx, name); err == nil {
			continue
		} else if !errors.Is(err, agent.ErrNotFound) {
			problems = append(problems, fmt.Sprintf("check agent %q: %v", name, err))
			continue
		}
		if opts.InstallAgents != nil && looksLikeScopedNpmName(name) {
			continue
		}
		problems = append(problems, fmt.Sprintf("agent %q is referenced by a step but is not installed (run `autosk agent install %s`)", name, name))
	}
	if len(problems) > 0 {
		return fmt.Errorf("workflow %q has %d dry-run materialization problem(s):\n  - %s", def.Name, len(problems), strings.Join(problems, "\n  - "))
	}
	return nil
}

func dryRunAgentNames(def workflow.Definition) []string {
	seen := make(map[string]struct{}, len(def.Steps))
	for _, step := range def.Steps {
		name := strings.TrimSpace(step.AgentName)
		if name == "" {
			continue
		}
		seen[name] = struct{}{}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func looksLikeScopedNpmName(s string) bool {
	if len(s) < 3 || s[0] != '@' {
		return false
	}
	slash := strings.IndexByte(s, '/')
	return slash > 1 && slash < len(s)-1
}

func globalOrigin(entry Entry) workflow.Origin {
	active := true
	return workflow.Origin{
		SourceType:     "global",
		Source:         entry.Name,
		SourceMetadata: entry.SourceMetadata,
		DefinitionHash: entry.DefinitionHash,
		Revision:       entry.Revision,
		ActiveOverride: &active,
	}
}
