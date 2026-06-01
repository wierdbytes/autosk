package workflow_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"autosk/internal/store"
	"autosk/internal/workflow"
)

func TestSyncManagedDefinition_CreateNoopAndUnreferencedReplace(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	def := managedTestDefinition("managed", "v1")

	created, err := wf.SyncManagedDefinition(ctx, def, workflow.Origin{SourceType: "bootstrap", Source: "embedded", Revision: "r1"})
	if err != nil {
		t.Fatalf("SyncManagedDefinition create: %v", err)
	}
	if created.Workflow.Name != "managed" || created.Noop || created.Replaced || created.Superseded {
		t.Fatalf("unexpected create report: %+v", created)
	}
	origin, err := wf.GetOrigin(ctx, created.Workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if origin.DefinitionHash == "" || !origin.Active || origin.Revision != "r1" {
		t.Fatalf("unexpected origin after create: %+v", origin)
	}

	noop, err := wf.SyncManagedDefinition(ctx, def, workflow.Origin{SourceType: "bootstrap", Source: "embedded", Revision: "r1b"})
	if err != nil {
		t.Fatalf("SyncManagedDefinition noop: %v", err)
	}
	if !noop.Noop || noop.Workflow.ID != created.Workflow.ID {
		t.Fatalf("unexpected noop report: %+v", noop)
	}
	origin, err = wf.GetOrigin(ctx, created.Workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if origin.Revision != "r1b" || !origin.Active {
		t.Fatalf("noop should refresh active origin metadata: %+v", origin)
	}

	updatedDef := managedTestDefinition("managed", "v2")
	replaced, err := wf.SyncManagedDefinition(ctx, updatedDef, workflow.Origin{SourceType: "bootstrap", Source: "embedded", Revision: "r2"})
	if err != nil {
		t.Fatalf("SyncManagedDefinition replace: %v", err)
	}
	if !replaced.Replaced || replaced.Superseded || replaced.Workflow.ID == created.Workflow.ID {
		t.Fatalf("unexpected replace report: %+v", replaced)
	}
	if _, err := wf.GetByID(ctx, created.Workflow.ID); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("old unreferenced workflow should be deleted, got %v", err)
	}
	current, err := wf.GetByName(ctx, "managed")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != replaced.Workflow.ID {
		t.Fatalf("canonical name points at %s, want %s", current.ID, replaced.Workflow.ID)
	}
}

func TestSyncManagedDefinition_SupersedesReferencedWorkflow(t *testing.T) {
	wf, _, dl, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	def := managedTestDefinition("managed", "v1")

	created, err := wf.SyncManagedDefinition(ctx, def, workflow.Origin{SourceType: "global", Source: "managed", Revision: "r1"})
	if err != nil {
		t.Fatalf("SyncManagedDefinition create: %v", err)
	}
	tk, err := dl.CreateTask(ctx, store.Task{Title: "ref", Status: store.StatusNew, Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dl.DB().ExecContext(ctx,
		`UPDATE tasks SET workflow_id = ?, current_step_id = ?, status = 'work' WHERE id = ?`,
		created.Workflow.ID, created.Workflow.Steps[0].ID, tk.ID); err != nil {
		t.Fatal(err)
	}

	updatedDef := managedTestDefinition("managed", "v2")
	rep, err := wf.SyncManagedDefinition(ctx, updatedDef, workflow.Origin{SourceType: "global", Source: "managed", Revision: "r2"})
	if err != nil {
		t.Fatalf("SyncManagedDefinition supersede: %v", err)
	}
	if !rep.Superseded || rep.Replaced || rep.Workflow.ID == created.Workflow.ID || rep.SupersededName == "" {
		t.Fatalf("unexpected supersede report: %+v", rep)
	}
	if !strings.Contains(rep.SupersededName, workflow.RevisionSuffixMarker) {
		t.Fatalf("superseded name %q missing reserved marker", rep.SupersededName)
	}

	old, err := wf.GetByID(ctx, created.Workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Name != rep.SupersededName || old.SupersededByID != rep.Workflow.ID {
		t.Fatalf("old revision not renamed/linked: %+v", old)
	}
	oldOrigin, err := wf.GetOrigin(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if oldOrigin.Active {
		t.Fatalf("old origin should be inactive: %+v", oldOrigin)
	}
	newOrigin, err := wf.GetOrigin(ctx, rep.Workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !newOrigin.Active || newOrigin.DefinitionHash == oldOrigin.DefinitionHash || newOrigin.Revision != "r2" {
		t.Fatalf("new origin not active/current: %+v old=%+v", newOrigin, oldOrigin)
	}
	current, err := wf.GetByName(ctx, "managed")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != rep.Workflow.ID {
		t.Fatalf("canonical name points at %s, want %s", current.ID, rep.Workflow.ID)
	}
	var taskWorkflow string
	if err := dl.DB().QueryRowContext(ctx, `SELECT workflow_id FROM tasks WHERE id = ?`, tk.ID).Scan(&taskWorkflow); err != nil {
		t.Fatal(err)
	}
	if taskWorkflow != old.ID {
		t.Fatalf("existing task should keep old workflow id, got %s want %s", taskWorkflow, old.ID)
	}
}

func TestSyncManagedDefinition_SupersedesSuccessorReferencedByOlderRevision(t *testing.T) {
	wf, _, dl, done := newWFFixture(t)
	defer done()
	ctx := context.Background()

	v1, err := wf.SyncManagedDefinition(ctx, managedTestDefinition("managed", "v1"), workflow.Origin{SourceType: "global", Source: "managed", Revision: "r1"})
	if err != nil {
		t.Fatalf("SyncManagedDefinition v1: %v", err)
	}
	tk, err := dl.CreateTask(ctx, store.Task{Title: "ref", Status: store.StatusNew, Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dl.DB().ExecContext(ctx,
		`UPDATE tasks SET workflow_id = ?, current_step_id = ?, status = 'work' WHERE id = ?`,
		v1.Workflow.ID, v1.Workflow.Steps[0].ID, tk.ID); err != nil {
		t.Fatal(err)
	}

	v2, err := wf.SyncManagedDefinition(ctx, managedTestDefinition("managed", "v2"), workflow.Origin{SourceType: "global", Source: "managed", Revision: "r2"})
	if err != nil {
		t.Fatalf("SyncManagedDefinition v2: %v", err)
	}
	if !v2.Superseded {
		t.Fatalf("v2 sync should supersede referenced v1: %+v", v2)
	}

	v3, err := wf.SyncManagedDefinition(ctx, managedTestDefinition("managed", "v3"), workflow.Origin{SourceType: "global", Source: "managed", Revision: "r3"})
	if err != nil {
		t.Fatalf("SyncManagedDefinition v3: %v", err)
	}
	if !v3.Superseded || v3.Replaced {
		t.Fatalf("v3 sync should supersede v2 because v1 links to it: %+v", v3)
	}

	oldV1, err := wf.GetByID(ctx, v1.Workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldV2, err := wf.GetByID(ctx, v2.Workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if oldV1.SupersededByID != oldV2.ID {
		t.Fatalf("v1 should still link to v2, got %+v want %s", oldV1, oldV2.ID)
	}
	if oldV2.Name != v3.SupersededName || oldV2.SupersededByID != v3.Workflow.ID {
		t.Fatalf("v2 should be retained and linked to v3: %+v report=%+v", oldV2, v3)
	}
	oldV2Origin, err := wf.GetOrigin(ctx, oldV2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if oldV2Origin.Active {
		t.Fatalf("v2 origin should be inactive after v3 supersession: %+v", oldV2Origin)
	}
	current, err := wf.GetByName(ctx, "managed")
	if err != nil {
		t.Fatal(err)
	}
	if current.ID != v3.Workflow.ID {
		t.Fatalf("canonical name points at %s, want %s", current.ID, v3.Workflow.ID)
	}
}

func TestSyncManagedDefinition_SupersedesDaemonRunReferencedWorkflow(t *testing.T) {
	wf, _, dl, done := newWFFixture(t)
	defer done()
	ctx := context.Background()

	created, err := wf.SyncManagedDefinition(ctx, managedTestDefinition("managed", "v1"), workflow.Origin{SourceType: "global", Source: "managed", Revision: "r1"})
	if err != nil {
		t.Fatalf("SyncManagedDefinition create: %v", err)
	}
	tk, err := dl.CreateTask(ctx, store.Task{Title: "run ref", Status: store.StatusNew, Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dl.DB().ExecContext(ctx, `
		INSERT INTO daemon_runs(job_id, task_id, step_id, status, created_at)
		VALUES (?, ?, ?, 'queued', ?)`, "job-sync-run-ref", tk.ID, created.Workflow.Steps[0].ID, int64(1)); err != nil {
		t.Fatal(err)
	}

	rep, err := wf.SyncManagedDefinition(ctx, managedTestDefinition("managed", "v2"), workflow.Origin{SourceType: "global", Source: "managed", Revision: "r2"})
	if err != nil {
		t.Fatalf("SyncManagedDefinition update: %v", err)
	}
	if !rep.Superseded || rep.Replaced {
		t.Fatalf("daemon_runs step ref should force supersession: %+v", rep)
	}
	old, err := wf.GetByID(ctx, created.Workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Name != rep.SupersededName || old.SupersededByID != rep.Workflow.ID {
		t.Fatalf("old run-referenced workflow not renamed/linked: %+v report=%+v", old, rep)
	}
	oldOrigin, err := wf.GetOrigin(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if oldOrigin.Active {
		t.Fatalf("old run-referenced origin should be inactive: %+v", oldOrigin)
	}
	var runWorkflowID string
	if err := dl.DB().QueryRowContext(ctx, `
		SELECT st.workflow_id
		  FROM daemon_runs r
		  JOIN steps st ON st.id = r.step_id
		 WHERE r.job_id = ?`, "job-sync-run-ref").Scan(&runWorkflowID); err != nil {
		t.Fatal(err)
	}
	if runWorkflowID != old.ID {
		t.Fatalf("daemon run step should still point at old workflow, got %s want %s", runWorkflowID, old.ID)
	}
}

func TestSyncManagedDefinition_RejectsReservedRevisionName(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	def := managedTestDefinition("managed"+workflow.RevisionSuffixMarker+"abc", "v1")
	_, err := wf.SyncManagedDefinition(ctx, def, workflow.Origin{SourceType: "global"})
	if err == nil {
		t.Fatal("SyncManagedDefinition should reject reserved revision suffix names")
	}
}

func managedTestDefinition(name, prompt string) workflow.Definition {
	return workflow.Definition{
		Name:        name,
		Description: "managed test",
		FirstStep:   "dev",
		Isolation:   workflow.IsolationNone,
		Steps: map[string]workflow.StepDef{
			"dev": {
				AgentName: "developer",
				NextSteps: []workflow.TransitionDef{{TaskStatus: "done", PromptRule: prompt}},
			},
		},
		StepNames: []string{"dev"},
	}
}
