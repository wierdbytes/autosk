package globalworkflow_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosk/internal/agent"
	"autosk/internal/agent/pkgregistry"
	"autosk/internal/globalworkflow"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
)

func TestSyncGlobalWorkflows_AddNoopAndDisabled(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	enabled := syncDefinition("global-enabled", "@autosk/dev-fixture", "v1")
	disabled := syncDefinition("global-disabled", "@autosk/dev-fixture", "v1")
	entry, err := r.StoreDefinition(enabled, globalworkflow.StoreOptions{Revision: "rev-1"})
	if err != nil {
		t.Fatal(err)
	}
	disabledFlag := false
	if _, err := r.StoreDefinition(disabled, globalworkflow.StoreOptions{Enabled: &disabledFlag}); err != nil {
		t.Fatal(err)
	}
	wf, ag, _, done := newSyncFixture(t)
	defer done()

	installedCalls := 0
	rep, err := globalworkflow.SyncGlobalWorkflows(ctx, r, wf, globalworkflow.SyncOptions{
		InstallAgents: ensureSyncAgents(t, ag, &installedCalls),
	})
	if err != nil {
		t.Fatalf("SyncGlobalWorkflows: %v", err)
	}
	if len(rep.Workflows) != 1 || rep.Workflows[0].Name != enabled.Name || rep.Workflows[0].Status != globalworkflow.SyncAdded || !rep.Mutated() {
		t.Fatalf("unexpected add report: %+v", rep)
	}
	if installedCalls != 1 || len(rep.Workflows[0].AutoInstalledAgents) != 1 {
		t.Fatalf("agent install not reported: calls=%d report=%+v", installedCalls, rep.Workflows[0])
	}
	local, err := wf.GetByName(ctx, enabled.Name)
	if err != nil {
		t.Fatalf("GetByName enabled: %v", err)
	}
	origin, err := wf.GetOrigin(ctx, local.ID)
	if err != nil {
		t.Fatalf("GetOrigin: %v", err)
	}
	if origin.SourceType != "global" || origin.Source != enabled.Name || origin.DefinitionHash != entry.DefinitionHash || !origin.Active {
		t.Fatalf("unexpected origin: %+v", origin)
	}
	if _, err := wf.GetByName(ctx, disabled.Name); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("disabled workflow materialized: %v", err)
	}

	rep, err = globalworkflow.SyncGlobalWorkflows(ctx, r, wf, globalworkflow.SyncOptions{
		InstallAgents: ensureSyncAgents(t, ag, &installedCalls),
	})
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if len(rep.Workflows) != 1 || rep.Workflows[0].Status != globalworkflow.SyncNoop || rep.Mutated() {
		t.Fatalf("unexpected noop report: %+v", rep)
	}
}

func TestSyncGlobalWorkflows_DryRunDoesNotInstallOrCreate(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	def := syncDefinition("dry-run-wf", "@autosk/dev-fixture", "v1")
	if _, err := r.StoreDefinition(def, globalworkflow.StoreOptions{}); err != nil {
		t.Fatal(err)
	}
	wf, ag, _, done := newSyncFixture(t)
	defer done()
	installCalled := false

	rep, err := globalworkflow.SyncGlobalWorkflows(ctx, r, wf, globalworkflow.SyncOptions{
		DryRun: true,
		InstallAgents: func(context.Context, workflow.Definition, globalworkflow.Entry) ([]pkgregistry.Entry, error) {
			installCalled = true
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("dry-run sync: %v", err)
	}
	if installCalled {
		t.Fatal("dry-run should not install agents")
	}
	if len(rep.Workflows) != 1 || rep.Workflows[0].Status != globalworkflow.SyncAdded || rep.Mutated() {
		t.Fatalf("unexpected dry-run report: %+v", rep)
	}
	if _, err := ag.GetByName(ctx, "@autosk/dev-fixture"); !errors.Is(err, agent.ErrNotFound) {
		t.Fatalf("dry-run installed agent: %v", err)
	}
	if _, err := wf.GetByName(ctx, def.Name); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("dry-run created workflow: %v", err)
	}
}

func TestSyncGlobalWorkflows_DryRunReportsInvalidAdd(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	def := syncDefinition("dry-run-invalid-add", "missing-bare-agent", "v1")
	if _, err := r.StoreDefinition(def, globalworkflow.StoreOptions{}); err != nil {
		t.Fatal(err)
	}
	wf, _, _, done := newSyncFixture(t)
	defer done()
	installCalled := false

	rep, err := globalworkflow.SyncGlobalWorkflows(ctx, r, wf, globalworkflow.SyncOptions{
		DryRun: true,
		InstallAgents: func(context.Context, workflow.Definition, globalworkflow.Entry) ([]pkgregistry.Entry, error) {
			installCalled = true
			return nil, nil
		},
	})
	if !errors.Is(err, globalworkflow.ErrSyncFailed) {
		t.Fatalf("err=%v, want ErrSyncFailed", err)
	}
	if installCalled {
		t.Fatal("dry-run should validate without installing agents")
	}
	if len(rep.Workflows) != 1 || rep.Workflows[0].Status != globalworkflow.SyncError || !strings.Contains(rep.Workflows[0].Error, "missing-bare-agent") || rep.Mutated() {
		t.Fatalf("unexpected invalid dry-run add report: %+v", rep)
	}
	if _, err := wf.GetByName(ctx, def.Name); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("dry-run created invalid workflow: %v", err)
	}
}

func TestSyncGlobalWorkflows_DryRunForceReportsInvalidReplacement(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	v1 := syncDefinition("dry-run-invalid-replacement", "developer", "v1")
	if _, err := r.StoreDefinition(v1, globalworkflow.StoreOptions{}); err != nil {
		t.Fatal(err)
	}
	wf, ag, _, done := newSyncFixture(t)
	defer done()
	if _, err := ag.Create(ctx, "developer", false); err != nil {
		t.Fatal(err)
	}
	if _, err := globalworkflow.SyncGlobalWorkflows(ctx, r, wf, globalworkflow.SyncOptions{}); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	v2 := syncDefinition("dry-run-invalid-replacement", "developer", "v2")
	v2.FirstStep = "missing"
	if _, err := r.StoreDefinition(v2, globalworkflow.StoreOptions{Revision: "rev-2"}); err != nil {
		t.Fatal(err)
	}

	rep, err := globalworkflow.SyncGlobalWorkflows(ctx, r, wf, globalworkflow.SyncOptions{DryRun: true, Force: true})
	if !errors.Is(err, globalworkflow.ErrSyncFailed) {
		t.Fatalf("err=%v, want ErrSyncFailed", err)
	}
	if len(rep.Workflows) != 1 || rep.Workflows[0].Status != globalworkflow.SyncError || !strings.Contains(rep.Workflows[0].Error, "first_step") || rep.Mutated() {
		t.Fatalf("unexpected invalid dry-run replacement report: %+v", rep)
	}
	got, err := wf.GetByName(ctx, v1.Name)
	if err != nil {
		t.Fatalf("old workflow missing after dry-run: %v", err)
	}
	if got.Description != "v1" {
		t.Fatalf("dry-run changed workflow: %+v", got)
	}
}

func TestSyncGlobalWorkflows_ForceInUsePreflightPreventsAgentInstall(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	v1 := syncDefinition("managed-in-use", "developer", "v1")
	if _, err := r.StoreDefinition(v1, globalworkflow.StoreOptions{}); err != nil {
		t.Fatal(err)
	}
	wf, ag, dl, done := newSyncFixture(t)
	defer done()
	if _, err := ag.Create(ctx, "developer", false); err != nil {
		t.Fatal(err)
	}
	if _, err := globalworkflow.SyncGlobalWorkflows(ctx, r, wf, globalworkflow.SyncOptions{}); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	local, err := wf.GetByName(ctx, v1.Name)
	if err != nil {
		t.Fatal(err)
	}
	tk, err := dl.CreateTask(ctx, store.Task{Title: "ref", Status: store.StatusNew, Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dl.DB().ExecContext(ctx, `UPDATE tasks SET workflow_id = ? WHERE id = ?`, local.ID, tk.ID); err != nil {
		t.Fatal(err)
	}
	v2 := syncDefinition("managed-in-use", "@autosk/new-scoped", "v2")
	if _, err := r.StoreDefinition(v2, globalworkflow.StoreOptions{Revision: "rev-2"}); err != nil {
		t.Fatal(err)
	}

	rep, err := globalworkflow.SyncGlobalWorkflows(ctx, r, wf, globalworkflow.SyncOptions{DryRun: true, Force: true})
	if !errors.Is(err, globalworkflow.ErrSyncFailed) {
		t.Fatalf("dry-run err=%v, want ErrSyncFailed", err)
	}
	if len(rep.Workflows) != 1 || rep.Workflows[0].Status != globalworkflow.SyncError || !strings.Contains(rep.Workflows[0].Error, workflow.ErrInUse.Error()) || rep.Mutated() {
		t.Fatalf("unexpected dry-run in-use report: %+v", rep)
	}

	installCalled := false
	rep, err = globalworkflow.SyncGlobalWorkflows(ctx, r, wf, globalworkflow.SyncOptions{
		Force: true,
		InstallAgents: func(ctx context.Context, def workflow.Definition, entry globalworkflow.Entry) ([]pkgregistry.Entry, error) {
			installCalled = true
			if _, err := ag.Create(ctx, "@autosk/new-scoped", false); err != nil {
				return nil, err
			}
			return []pkgregistry.Entry{{Name: "@autosk/new-scoped", Version: "1.0.0", InstalledAt: time.Now().UTC()}}, nil
		},
	})
	if !errors.Is(err, globalworkflow.ErrSyncFailed) {
		t.Fatalf("force err=%v, want ErrSyncFailed", err)
	}
	if installCalled {
		t.Fatal("force sync installed agents before replace preflight")
	}
	if len(rep.Workflows) != 1 || rep.Workflows[0].Status != globalworkflow.SyncError || rep.Workflows[0].Mutated || rep.Mutated() {
		t.Fatalf("unexpected force in-use report: %+v", rep)
	}
	if _, err := ag.GetByName(ctx, "@autosk/new-scoped"); !errors.Is(err, agent.ErrNotFound) {
		t.Fatalf("force sync installed scoped agent despite in-use replace: %v", err)
	}
	got, err := wf.GetByName(ctx, v1.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "v1" {
		t.Fatalf("force sync replaced in-use workflow: %+v", got)
	}
}

func TestSyncGlobalWorkflows_InvalidDefinitionPreventsAgentInstall(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	def := syncDefinition("invalid-scoped-add", "@autosk/invalid-scoped", "v1")
	def.FirstStep = "missing"
	if _, err := r.StoreDefinition(def, globalworkflow.StoreOptions{}); err != nil {
		t.Fatal(err)
	}
	wf, ag, _, done := newSyncFixture(t)
	defer done()
	installCalled := false

	rep, err := globalworkflow.SyncGlobalWorkflows(ctx, r, wf, globalworkflow.SyncOptions{
		InstallAgents: func(ctx context.Context, def workflow.Definition, entry globalworkflow.Entry) ([]pkgregistry.Entry, error) {
			installCalled = true
			if _, err := ag.Create(ctx, "@autosk/invalid-scoped", false); err != nil {
				return nil, err
			}
			return []pkgregistry.Entry{{Name: "@autosk/invalid-scoped", Version: "1.0.0", InstalledAt: time.Now().UTC()}}, nil
		},
	})
	if !errors.Is(err, globalworkflow.ErrSyncFailed) {
		t.Fatalf("err=%v, want ErrSyncFailed", err)
	}
	if installCalled {
		t.Fatal("sync installed agents before structural validation")
	}
	if len(rep.Workflows) != 1 || rep.Workflows[0].Status != globalworkflow.SyncError || !strings.Contains(rep.Workflows[0].Error, "first_step") || rep.Workflows[0].Mutated || rep.Mutated() {
		t.Fatalf("unexpected invalid definition report: %+v", rep)
	}
	if _, err := ag.GetByName(ctx, "@autosk/invalid-scoped"); !errors.Is(err, agent.ErrNotFound) {
		t.Fatalf("sync installed scoped agent despite invalid definition: %v", err)
	}
	if _, err := wf.GetByName(ctx, def.Name); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("sync created invalid workflow: %v", err)
	}
}

func TestSyncGlobalWorkflows_MissingBareAgentPreflightPreventsScopedInstallOnAdd(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	def := mixedAgentDefinition("mixed-agent-add", "v1")
	if _, err := r.StoreDefinition(def, globalworkflow.StoreOptions{}); err != nil {
		t.Fatal(err)
	}
	wf, ag, _, done := newSyncFixture(t)
	defer done()
	installCalled := false

	rep, err := globalworkflow.SyncGlobalWorkflows(ctx, r, wf, globalworkflow.SyncOptions{
		InstallAgents: func(ctx context.Context, def workflow.Definition, entry globalworkflow.Entry) ([]pkgregistry.Entry, error) {
			installCalled = true
			if _, err := ag.Create(ctx, "@autosk/missing-scoped", false); err != nil {
				return nil, err
			}
			return []pkgregistry.Entry{{Name: "@autosk/missing-scoped", Version: "1.0.0", InstalledAt: time.Now().UTC()}}, nil
		},
	})
	if !errors.Is(err, globalworkflow.ErrSyncFailed) {
		t.Fatalf("err=%v, want ErrSyncFailed", err)
	}
	if installCalled {
		t.Fatal("sync installed scoped agents before missing bare-agent preflight")
	}
	if len(rep.Workflows) != 1 || rep.Workflows[0].Status != globalworkflow.SyncError || !strings.Contains(rep.Workflows[0].Error, "missing-bare-agent") || rep.Workflows[0].Mutated || rep.Mutated() {
		t.Fatalf("unexpected mixed-agent add report: %+v", rep)
	}
	if _, err := ag.GetByName(ctx, "@autosk/missing-scoped"); !errors.Is(err, agent.ErrNotFound) {
		t.Fatalf("sync installed scoped agent despite missing bare agent: %v", err)
	}
	if _, err := wf.GetByName(ctx, def.Name); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("sync created workflow despite missing bare agent: %v", err)
	}
}

func TestSyncGlobalWorkflows_MissingBareAgentPreflightPreventsScopedInstallOnForceUpdate(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	v1 := syncDefinition("mixed-agent-force", "developer", "v1")
	if _, err := r.StoreDefinition(v1, globalworkflow.StoreOptions{}); err != nil {
		t.Fatal(err)
	}
	wf, ag, _, done := newSyncFixture(t)
	defer done()
	if _, err := ag.Create(ctx, "developer", false); err != nil {
		t.Fatal(err)
	}
	if _, err := globalworkflow.SyncGlobalWorkflows(ctx, r, wf, globalworkflow.SyncOptions{}); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	v2 := mixedAgentDefinition("mixed-agent-force", "v2")
	if _, err := r.StoreDefinition(v2, globalworkflow.StoreOptions{Revision: "rev-2"}); err != nil {
		t.Fatal(err)
	}
	installCalled := false

	rep, err := globalworkflow.SyncGlobalWorkflows(ctx, r, wf, globalworkflow.SyncOptions{
		Force: true,
		InstallAgents: func(ctx context.Context, def workflow.Definition, entry globalworkflow.Entry) ([]pkgregistry.Entry, error) {
			installCalled = true
			if _, err := ag.Create(ctx, "@autosk/missing-scoped", false); err != nil {
				return nil, err
			}
			return []pkgregistry.Entry{{Name: "@autosk/missing-scoped", Version: "1.0.0", InstalledAt: time.Now().UTC()}}, nil
		},
	})
	if !errors.Is(err, globalworkflow.ErrSyncFailed) {
		t.Fatalf("err=%v, want ErrSyncFailed", err)
	}
	if installCalled {
		t.Fatal("force sync installed scoped agents before missing bare-agent preflight")
	}
	if len(rep.Workflows) != 1 || rep.Workflows[0].Status != globalworkflow.SyncError || !strings.Contains(rep.Workflows[0].Error, "missing-bare-agent") || rep.Workflows[0].Mutated || rep.Mutated() {
		t.Fatalf("unexpected mixed-agent force report: %+v", rep)
	}
	if _, err := ag.GetByName(ctx, "@autosk/missing-scoped"); !errors.Is(err, agent.ErrNotFound) {
		t.Fatalf("force sync installed scoped agent despite missing bare agent: %v", err)
	}
	got, err := wf.GetByName(ctx, v1.Name)
	if err != nil {
		t.Fatalf("old workflow missing after failed force: %v", err)
	}
	if got.Description != "v1" {
		t.Fatalf("force sync replaced workflow despite missing bare agent: %+v", got)
	}
}

func TestSyncGlobalWorkflows_ConflictDoesNotOverwriteLocalWorkflow(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	global := syncDefinition("same-name", "developer", "global")
	if _, err := r.StoreDefinition(global, globalworkflow.StoreOptions{}); err != nil {
		t.Fatal(err)
	}
	wf, ag, _, done := newSyncFixture(t)
	defer done()
	if _, err := ag.Create(ctx, "developer", false); err != nil {
		t.Fatal(err)
	}
	local := syncDefinition("same-name", "developer", "local")
	if _, err := wf.Create(ctx, local, false); err != nil {
		t.Fatal(err)
	}

	rep, err := globalworkflow.SyncGlobalWorkflows(ctx, r, wf, globalworkflow.SyncOptions{})
	if err != nil {
		t.Fatalf("conflict should be reported without sync error: %v", err)
	}
	if len(rep.Workflows) != 1 || rep.Workflows[0].Status != globalworkflow.SyncConflict || rep.Mutated() {
		t.Fatalf("unexpected conflict report: %+v", rep)
	}
	got, err := wf.GetByName(ctx, "same-name")
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "local" {
		t.Fatalf("local workflow overwritten: %+v", got)
	}
}

func TestSyncGlobalWorkflows_ChangedManagedWorkflowRequiresForce(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	v1 := syncDefinition("managed", "developer", "v1")
	if _, err := r.StoreDefinition(v1, globalworkflow.StoreOptions{}); err != nil {
		t.Fatal(err)
	}
	wf, ag, _, done := newSyncFixture(t)
	defer done()
	if _, err := ag.Create(ctx, "developer", false); err != nil {
		t.Fatal(err)
	}
	if _, err := globalworkflow.SyncGlobalWorkflows(ctx, r, wf, globalworkflow.SyncOptions{}); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	v2 := syncDefinition("managed", "developer", "v2")
	entry2, err := r.StoreDefinition(v2, globalworkflow.StoreOptions{Revision: "rev-2"})
	if err != nil {
		t.Fatal(err)
	}

	rep, err := globalworkflow.SyncGlobalWorkflows(ctx, r, wf, globalworkflow.SyncOptions{})
	if err != nil {
		t.Fatalf("non-force changed sync: %v", err)
	}
	if len(rep.Workflows) != 1 || rep.Workflows[0].Status != globalworkflow.SyncSkipped || rep.Mutated() {
		t.Fatalf("unexpected non-force report: %+v", rep)
	}
	unchanged, err := wf.GetByName(ctx, "managed")
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Description != "v1" {
		t.Fatalf("non-force changed workflow: %+v", unchanged)
	}

	rep, err = globalworkflow.SyncGlobalWorkflows(ctx, r, wf, globalworkflow.SyncOptions{Force: true})
	if err != nil {
		t.Fatalf("force changed sync: %v", err)
	}
	if len(rep.Workflows) != 1 || rep.Workflows[0].Status != globalworkflow.SyncUpdated || !rep.Mutated() {
		t.Fatalf("unexpected force report: %+v", rep)
	}
	updated, err := wf.GetByName(ctx, "managed")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Description != "v2" {
		t.Fatalf("force did not update workflow: %+v", updated)
	}
	origin, err := wf.GetOrigin(ctx, updated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if origin.DefinitionHash != entry2.DefinitionHash || origin.Revision != "rev-2" {
		t.Fatalf("origin not updated: %+v", origin)
	}
}

func TestSyncGlobalWorkflows_LoadErrorReported(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	def := syncDefinition("tampered", "developer", "v1")
	entry, err := r.StoreDefinition(def, globalworkflow.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	body, err := workflow.CanonicalDefinitionJSON(syncDefinition("tampered", "developer", "changed"))
	if err != nil {
		t.Fatal(err)
	}
	if err := osWriteFile(filepath.Join(r.Prefix(), filepath.FromSlash(entry.DefinitionFile)), append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	wf, _, _, done := newSyncFixture(t)
	defer done()

	rep, err := globalworkflow.SyncGlobalWorkflows(ctx, r, wf, globalworkflow.SyncOptions{})
	if !errors.Is(err, globalworkflow.ErrSyncFailed) {
		t.Fatalf("err=%v, want ErrSyncFailed", err)
	}
	if len(rep.Workflows) != 1 || rep.Workflows[0].Status != globalworkflow.SyncError || rep.Workflows[0].Error == "" {
		t.Fatalf("unexpected error report: %+v", rep)
	}
}

func TestSyncGlobalWorkflows_ForceInvalidReplacementKeepsOldWorkflow(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	v1 := syncDefinition("managed-invalid-replacement", "developer", "v1")
	if _, err := r.StoreDefinition(v1, globalworkflow.StoreOptions{}); err != nil {
		t.Fatal(err)
	}
	wf, ag, _, done := newSyncFixture(t)
	defer done()
	if _, err := ag.Create(ctx, "developer", false); err != nil {
		t.Fatal(err)
	}
	if _, err := globalworkflow.SyncGlobalWorkflows(ctx, r, wf, globalworkflow.SyncOptions{}); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	v2 := syncDefinition("managed-invalid-replacement", "missing-bare-agent", "v2")
	if _, err := r.StoreDefinition(v2, globalworkflow.StoreOptions{Revision: "rev-2"}); err != nil {
		t.Fatal(err)
	}

	rep, err := globalworkflow.SyncGlobalWorkflows(ctx, r, wf, globalworkflow.SyncOptions{Force: true})
	if !errors.Is(err, globalworkflow.ErrSyncFailed) {
		t.Fatalf("err=%v, want ErrSyncFailed", err)
	}
	if len(rep.Workflows) != 1 || rep.Workflows[0].Status != globalworkflow.SyncError || rep.Workflows[0].Mutated {
		t.Fatalf("unexpected failed force report: %+v", rep)
	}
	got, err := wf.GetByName(ctx, "managed-invalid-replacement")
	if err != nil {
		t.Fatalf("old workflow missing after failed force: %v", err)
	}
	if got.Description != "v1" {
		t.Fatalf("old workflow was replaced: %+v", got)
	}
	origin, err := wf.GetOrigin(ctx, got.ID)
	if err != nil {
		t.Fatalf("old origin missing after failed force: %v", err)
	}
	if origin.Revision == "rev-2" {
		t.Fatalf("origin was advanced despite failed replace: %+v", origin)
	}
}

func TestSyncGlobalWorkflows_OriginFailureRollsBackCreatedWorkflow(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	def := syncDefinition("origin-failure", "human", "v1")
	if _, err := r.StoreDefinition(def, globalworkflow.StoreOptions{}); err != nil {
		t.Fatal(err)
	}
	wf, _, dl, done := newSyncFixture(t)
	defer done()
	if _, err := dl.DB().ExecContext(ctx, `DROP TABLE workflow_origins`); err != nil {
		t.Fatal(err)
	}

	rep, err := globalworkflow.SyncGlobalWorkflows(ctx, r, wf, globalworkflow.SyncOptions{})
	if !errors.Is(err, globalworkflow.ErrSyncFailed) {
		t.Fatalf("err=%v, want ErrSyncFailed", err)
	}
	if len(rep.Workflows) != 1 || rep.Workflows[0].Status != globalworkflow.SyncError || rep.Workflows[0].Mutated {
		t.Fatalf("unexpected origin failure report: %+v", rep)
	}
	if _, err := wf.GetByName(ctx, "origin-failure"); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("workflow left behind after origin failure: %v", err)
	}
}

func TestSyncGlobalWorkflows_PartialAgentInstallErrorIsReportedAsMutation(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	def := syncDefinition("partial-agent-install", "@autosk/partial", "v1")
	if _, err := r.StoreDefinition(def, globalworkflow.StoreOptions{}); err != nil {
		t.Fatal(err)
	}
	wf, ag, _, done := newSyncFixture(t)
	defer done()

	rep, err := globalworkflow.SyncGlobalWorkflows(ctx, r, wf, globalworkflow.SyncOptions{
		InstallAgents: func(ctx context.Context, def workflow.Definition, entry globalworkflow.Entry) ([]pkgregistry.Entry, error) {
			if _, err := ag.Create(ctx, "@autosk/partial", false); err != nil {
				return nil, err
			}
			return []pkgregistry.Entry{{Name: "@autosk/partial", Version: "1.0.0", InstalledAt: time.Now().UTC()}}, errors.New("boom")
		},
	})
	if !errors.Is(err, globalworkflow.ErrSyncFailed) {
		t.Fatalf("err=%v, want ErrSyncFailed", err)
	}
	if len(rep.Workflows) != 1 || rep.Workflows[0].Status != globalworkflow.SyncError || len(rep.Workflows[0].AutoInstalledAgents) != 1 || !rep.Workflows[0].Mutated || !rep.Mutated() {
		t.Fatalf("partial install not surfaced as mutation: %+v", rep)
	}
	if _, err := ag.GetByName(ctx, "@autosk/partial"); err != nil {
		t.Fatalf("partial agent side effect missing: %v", err)
	}
	if _, err := wf.GetByName(ctx, "partial-agent-install"); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("workflow should not be created after install error: %v", err)
	}
}

func newRegistry(t *testing.T) *globalworkflow.Registry {
	t.Helper()
	r, err := globalworkflow.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func newSyncFixture(t *testing.T) (*workflow.Store, *agent.Store, *doltlite.Store, func()) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dl := doltlite.New()
	if err := dl.Open(ctx, filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := dl.Migrate(ctx); err != nil {
		_ = dl.Close()
		t.Fatalf("Migrate: %v", err)
	}
	ag := agent.New(dl.DB())
	return workflow.New(dl.DB(), ag), ag, dl, func() { _ = dl.Close() }
}

func syncDefinition(name, agentName, description string) workflow.Definition {
	return workflow.Definition{
		Name:        name,
		Description: description,
		FirstStep:   "dev",
		Steps: map[string]workflow.StepDef{
			"dev": {
				AgentName: agentName,
				NextSteps: []workflow.TransitionDef{{TaskStatus: "done", PromptRule: "done"}},
			},
		},
	}
}

func mixedAgentDefinition(name, description string) workflow.Definition {
	return workflow.Definition{
		Name:        name,
		Description: description,
		FirstStep:   "scoped",
		StepNames:   []string{"scoped", "bare"},
		Steps: map[string]workflow.StepDef{
			"scoped": {
				AgentName: "@autosk/missing-scoped",
				NextSteps: []workflow.TransitionDef{{Step: "bare", PromptRule: "next"}},
			},
			"bare": {
				AgentName: "missing-bare-agent",
				NextSteps: []workflow.TransitionDef{{TaskStatus: "done", PromptRule: "done"}},
			},
		},
	}
}

func ensureSyncAgents(t *testing.T, ag *agent.Store, calls *int) globalworkflow.InstallAgentsFunc {
	t.Helper()
	return func(ctx context.Context, def workflow.Definition, entry globalworkflow.Entry) ([]pkgregistry.Entry, error) {
		(*calls)++
		installed := make([]pkgregistry.Entry, 0, len(def.Steps))
		for _, step := range def.Steps {
			if step.AgentName == agent.HumanAgentName {
				continue
			}
			if _, err := ag.GetByName(ctx, step.AgentName); err == nil {
				continue
			} else if !errors.Is(err, agent.ErrNotFound) {
				return nil, err
			}
			if _, err := ag.Create(ctx, step.AgentName, false); err != nil {
				return nil, err
			}
			installed = append(installed, pkgregistry.Entry{Name: step.AgentName, Version: "1.0.0", InstalledAt: time.Now().UTC()})
		}
		return installed, nil
	}
}

var osWriteFile = os.WriteFile
