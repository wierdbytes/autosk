package workflow_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"autosk/internal/agent"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
)

func newWFFixture(t *testing.T) (*workflow.Store, *agent.Store, *doltlite.Store, func()) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	ts := doltlite.New()
	if err := ts.Open(ctx, filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := ts.Migrate(ctx); err != nil {
		_ = ts.Close()
		t.Fatalf("Migrate: %v", err)
	}
	ag := agent.New(ts.DB())
	for _, name := range []string{"developer", "code-reviewer", "task-validator"} {
		if _, err := ag.Create(ctx, name, false); err != nil {
			_ = ts.Close()
			t.Fatalf("create agent %s: %v", name, err)
		}
	}
	return workflow.New(ts.DB(), ag), ag, ts, func() { _ = ts.Close() }
}

func TestCreate_RoundTripExample(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()

	def, err := workflow.ParseFile("../../docs/examples/workflows/workflow-example.json")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	w, err := wf.Create(ctx, def, false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if w.Name != "feature-dev" {
		t.Errorf("name: %q", w.Name)
	}
	if len(w.Steps) != 3 {
		t.Fatalf("steps: %d", len(w.Steps))
	}
	if w.Steps[0].Name != "dev" || w.Steps[1].Name != "review" || w.Steps[2].Name != "validator" {
		t.Errorf("step order: %v", stepNames(w.Steps))
	}
	if w.FirstStepID != w.Steps[0].ID {
		t.Errorf("first_step_id != steps[0].id")
	}
	// Validator has one sibling and one task_status transition.
	val := w.Steps[2]
	if len(val.Transitions) != 2 {
		t.Fatalf("validator transitions: %d", len(val.Transitions))
	}
	if val.Transitions[0].NextStepName != "dev" || val.Transitions[0].IsTaskStatus() {
		t.Errorf("transition 0: %+v", val.Transitions[0])
	}
	if val.Transitions[1].TaskStatus != "human" {
		t.Errorf("transition 1 task_status: %q", val.Transitions[1].TaskStatus)
	}
}

func TestCreate_DuplicateNameRejected(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	def, _ := workflow.ParseFile("../../docs/examples/workflows/workflow-example.json")
	if _, err := wf.Create(ctx, def, false); err != nil {
		t.Fatal(err)
	}
	_, err := wf.Create(ctx, def, false)
	if !errors.Is(err, workflow.ErrAlreadyExist) {
		t.Fatalf("want ErrAlreadyExist, got %v", err)
	}
}

func TestCreate_AgentMissingFailsValidate(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	body := `{
		"name": "bad", "first_step": "a",
		"steps": {"a": {"agent": {"name": "nobody"}, "next_steps": [{"task_status":"done","prompt_rule":"."}]}}
	}`
	def, err := workflow.ParseReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, err = wf.Create(ctx, def, false)
	if err == nil || !strings.Contains(err.Error(), "nobody") {
		t.Fatalf("want agent-not-found error, got %v", err)
	}
}

func mustParseWorkflow(t *testing.T, body string) workflow.Definition {
	t.Helper()
	def, err := workflow.ParseReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return def
}

func TestCreateWithOrigin_OriginFailureRollsBackWorkflow(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	def := mustParseWorkflow(t, `{
		"name": "origin-create-fails", "first_step": "dev",
		"steps": {"dev": {"agent": {"name": "developer"}, "next_steps": [{"task_status":"done","prompt_rule":"."}]}}
	}`)

	_, err := wf.CreateWithOrigin(ctx, def, false, workflow.Origin{
		SourceType:     "global",
		Source:         def.Name,
		SourceMetadata: map[string]any{"bad": func() {}},
	})
	if err == nil || !strings.Contains(err.Error(), "marshal workflow origin metadata") {
		t.Fatalf("want origin marshal error, got %v", err)
	}
	if _, err := wf.GetByName(ctx, def.Name); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("workflow left behind after origin failure: %v", err)
	}
}

func TestReplaceWithOrigin_OriginFailureKeepsOldWorkflow(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	v1 := mustParseWorkflow(t, `{
		"name": "origin-replace-fails", "description": "v1", "first_step": "dev",
		"steps": {"dev": {"agent": {"name": "developer"}, "next_steps": [{"task_status":"done","prompt_rule":"."}]}}
	}`)
	created, err := wf.CreateWithOrigin(ctx, v1, false, workflow.Origin{SourceType: "global", Source: v1.Name, DefinitionHash: "h1"})
	if err != nil {
		t.Fatal(err)
	}
	v2 := mustParseWorkflow(t, `{
		"name": "origin-replace-fails", "description": "v2", "first_step": "dev",
		"steps": {"dev": {"agent": {"name": "developer"}, "next_steps": [{"task_status":"done","prompt_rule":"."}]}}
	}`)

	_, err = wf.ReplaceWithOrigin(ctx, v1.Name, v2, workflow.Origin{
		SourceType:     "global",
		Source:         v1.Name,
		DefinitionHash: "h2",
		SourceMetadata: map[string]any{"bad": func() {}},
	})
	if err == nil || !strings.Contains(err.Error(), "marshal workflow origin metadata") {
		t.Fatalf("want origin marshal error, got %v", err)
	}
	got, err := wf.GetByName(ctx, v1.Name)
	if err != nil {
		t.Fatalf("old workflow missing after failed replace: %v", err)
	}
	if got.ID != created.ID || got.Description != "v1" {
		t.Fatalf("old workflow not preserved: got=%+v created=%+v", got, created)
	}
	origin, err := wf.GetOrigin(ctx, got.ID)
	if err != nil {
		t.Fatalf("old origin missing after failed replace: %v", err)
	}
	if origin.DefinitionHash != "h1" {
		t.Fatalf("old origin not preserved: %+v", origin)
	}
}

// TestCreate_RoundTripsAgentParams verifies that per-step agent.params
// blocks are persisted through the steps table and read back as a
// non-nil AgentParams.
func TestCreate_RoundTripsAgentParams(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	body := `{
		"name": "params-wf",
		"first_step": "a",
		"steps": {
			"a": {
				"agent": {
					"name": "developer",
					"params": {
						"model": "claude-sonnet-4-6",
						"thinking": "high",
						"first_message": "You are generic agent",
						"extra_args": ["--no-tool", "web_fetch"]
					}
				},
				"next_steps": [{"task_status": "done", "prompt_rule": "."}]
			}
		}}`
	def, err := workflow.ParseReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	w, err := wf.Create(ctx, def, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Steps) != 1 {
		t.Fatalf("steps: %d", len(w.Steps))
	}
	p := w.Steps[0].AgentParams
	if p == nil {
		t.Fatal("AgentParams lost during round-trip")
	}
	if p.Model == nil || *p.Model != "claude-sonnet-4-6" {
		t.Errorf("model: %v", p.Model)
	}
	if p.Thinking == nil || *p.Thinking != "high" {
		t.Errorf("thinking: %v", p.Thinking)
	}
	if p.FirstMessage == nil || *p.FirstMessage != "You are generic agent" {
		t.Errorf("first_message: %v", p.FirstMessage)
	}
	if len(p.ExtraArgs) != 2 || p.ExtraArgs[1] != "web_fetch" {
		t.Errorf("extra_args: %v", p.ExtraArgs)
	}
	// Sanity-check the path used by the executor too.
	st, err := wf.FindStepByID(ctx, w.Steps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.AgentParams == nil || st.AgentParams.Model == nil || *st.AgentParams.Model != "claude-sonnet-4-6" {
		t.Fatalf("FindStepByID lost params: %+v", st.AgentParams)
	}
}

// TestCreate_RoundTripsMaxVisits verifies that per-step max_visits is
// persisted through the steps table and read back on every scan path
// (GetByName, FindStepByID, FindStepByName).
func TestCreate_RoundTripsMaxVisits(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	body := `{
		"name": "caps",
		"first_step": "dev",
		"steps": {
			"dev":    {"agent": {"name": "developer"},    "max_visits": 3, "next_steps": [{"step": "review", "prompt_rule": "."}]},
			"review": {"agent": {"name": "code-reviewer"}, "max_visits": 2, "next_steps": [{"task_status": "done", "prompt_rule": "."}]}
		}}`
	def, err := workflow.ParseReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	w, err := wf.Create(ctx, def, false)
	if err != nil {
		t.Fatal(err)
	}
	if w.Steps[0].MaxVisits != 3 || w.Steps[1].MaxVisits != 2 {
		t.Fatalf("caps via GetByName: %d/%d", w.Steps[0].MaxVisits, w.Steps[1].MaxVisits)
	}
	st, err := wf.FindStepByID(ctx, w.Steps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.MaxVisits != 3 {
		t.Fatalf("FindStepByID lost cap: %+v", st.MaxVisits)
	}
	st2, err := wf.FindStepByName(ctx, w.ID, "review")
	if err != nil {
		t.Fatal(err)
	}
	if st2.MaxVisits != 2 {
		t.Fatalf("FindStepByName lost cap: %+v", st2.MaxVisits)
	}
}

// TestEnsureSingle_MaxVisitsIsZero verifies the synthetic single-agent
// workflow stays uncapped (cap=0).
// TestCreate_RoundTripsIsolation verifies the workflow's isolation
// field survives a Create + GetByName/GetByID round-trip.
func TestCreate_RoundTripsIsolation(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	body := `{
		"name": "isolated",
		"first_step": "dev",
		"isolation": "worktree",
		"steps": {
			"dev": {"agent": {"name": "developer"}, "next_steps": [{"task_status": "done", "prompt_rule": "."}]}
		}}`
	def, err := workflow.ParseReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	w, err := wf.Create(ctx, def, false)
	if err != nil {
		t.Fatal(err)
	}
	if w.Isolation != workflow.IsolationWorktree {
		t.Fatalf("Create.Isolation: %q (want worktree)", w.Isolation)
	}
	got, err := wf.GetByName(ctx, "isolated")
	if err != nil {
		t.Fatal(err)
	}
	if got.Isolation != workflow.IsolationWorktree {
		t.Fatalf("GetByName.Isolation: %q", got.Isolation)
	}
	got2, err := wf.GetByID(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Isolation != workflow.IsolationWorktree {
		t.Fatalf("GetByID.Isolation: %q", got2.Isolation)
	}
	list, err := wf.List(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, x := range list {
		if x.Name == "isolated" {
			if x.Isolation != workflow.IsolationWorktree {
				t.Fatalf("List row Isolation: %q", x.Isolation)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("workflow not in List")
	}
}

// TestCreate_DefaultIsolationIsNone verifies that workflows without
// `isolation` set on disk get the default `none` on scan paths.
func TestCreate_DefaultIsolationIsNone(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	def, err := workflow.ParseFile("../../docs/examples/workflows/workflow-example.json")
	if err != nil {
		t.Fatal(err)
	}
	w, err := wf.Create(ctx, def, false)
	if err != nil {
		t.Fatal(err)
	}
	if w.Isolation != workflow.IsolationNone {
		t.Fatalf("default Isolation: %q (want none)", w.Isolation)
	}
}

func TestEnsureSingle_MaxVisitsIsZero(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	w, err := wf.EnsureSingle(ctx, "developer")
	if err != nil {
		t.Fatal(err)
	}
	if w.Steps[0].MaxVisits != 0 {
		t.Fatalf("synthetic step should be uncapped, got max_visits=%d", w.Steps[0].MaxVisits)
	}
}

func TestList_HidesSyntheticByDefault(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()

	// Real workflow.
	def, _ := workflow.ParseFile("../../docs/examples/workflows/workflow-example.json")
	if _, err := wf.Create(ctx, def, false); err != nil {
		t.Fatal(err)
	}
	// Synthetic.
	if _, err := wf.EnsureSingle(ctx, "developer"); err != nil {
		t.Fatal(err)
	}

	visible, err := wf.List(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 || visible[0].Name != "feature-dev" {
		t.Fatalf("visible list: %v", workflowNames(visible))
	}
	all, err := wf.List(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("all list len: %d", len(all))
	}
}

// TestEnsureSingle_IsolationIsNone verifies the synthetic workflow's
// isolation is pinned to `none`.
func TestEnsureSingle_IsolationIsNone(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	w, err := wf.EnsureSingle(ctx, "developer")
	if err != nil {
		t.Fatal(err)
	}
	if w.Isolation != workflow.IsolationNone {
		t.Fatalf("synthetic workflow isolation: %q", w.Isolation)
	}
}

func TestEnsureSingle_Idempotent(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	w1, err := wf.EnsureSingle(ctx, "developer")
	if err != nil {
		t.Fatal(err)
	}
	w2, err := wf.EnsureSingle(ctx, "developer")
	if err != nil {
		t.Fatal(err)
	}
	if w1.ID != w2.ID {
		t.Fatalf("not idempotent: %s vs %s", w1.ID, w2.ID)
	}
	if !w1.IsSynthetic {
		t.Fatal("synthetic flag not set")
	}
	if len(w1.Steps) != 1 || w1.Steps[0].Name != "do" {
		t.Fatalf("synthetic shape: %+v", stepNames(w1.Steps))
	}
	if len(w1.Steps[0].Transitions) != 3 {
		t.Fatalf("expected 3 transitions, got %d", len(w1.Steps[0].Transitions))
	}
}

func TestEnsureSingle_ConcurrentCreate(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	const N = 4
	results := make(chan workflow.Workflow, N)
	errs := make(chan error, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w, err := wf.EnsureSingle(ctx, "developer")
			if err != nil {
				errs <- err
				return
			}
			results <- w
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	if len(errs) != 0 {
		for e := range errs {
			t.Errorf("concurrent EnsureSingle returned error: %v", e)
		}
	}
	var id string
	for w := range results {
		if id == "" {
			id = w.ID
		} else if w.ID != id {
			t.Fatalf("different ids from concurrent ensure: %s vs %s", id, w.ID)
		}
	}
}

func TestDelete_RefusesInUse(t *testing.T) {
	wf, _, dl, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	def, _ := workflow.ParseFile("../../docs/examples/workflows/workflow-example.json")
	w, err := wf.Create(ctx, def, false)
	if err != nil {
		t.Fatal(err)
	}
	// Add a task that points at the workflow.
	tk, err := dl.CreateTask(ctx, store.Task{Title: "ref", Status: store.StatusNew, Priority: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dl.DB().ExecContext(ctx,
		`UPDATE tasks SET workflow_id = ? WHERE id = ?`, w.ID, tk.ID); err != nil {
		t.Fatal(err)
	}
	err = wf.Delete(ctx, w.Name)
	if !errors.Is(err, workflow.ErrInUse) {
		t.Fatalf("want ErrInUse, got %v", err)
	}
}

func TestDelete_Ok(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	def, _ := workflow.ParseFile("../../docs/examples/workflows/workflow-example.json")
	if _, err := wf.Create(ctx, def, false); err != nil {
		t.Fatal(err)
	}
	if err := wf.Delete(ctx, "feature-dev"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := wf.GetByName(ctx, "feature-dev")
	if !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("post-delete GetByName: %v", err)
	}
}

// TestDelete_AllowsImmediateRecreate is the regression guard for the
// doltlite v0.10.8 phantom-unique-index bug: a DELETE against a row
// with a UNIQUE constraint (workflows.name) used to leave the matching
// entry in the implicit unique index, so a subsequent Create with the
// same name tripped ErrAlreadyExist even though GetByName reported the
// row as gone. Store.Delete works around the bug by running REINDEX
// on the workflows table immediately after the DELETE; this test
// pins that behaviour so a future doltlite bump that removes the
// REINDEX call has to re-prove the round-trip still works.
func TestDelete_AllowsImmediateRecreate(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	def, _ := workflow.ParseFile("../../docs/examples/workflows/workflow-example.json")
	if _, err := wf.Create(ctx, def, false); err != nil {
		t.Fatalf("Create #1: %v", err)
	}
	if err := wf.Delete(ctx, def.Name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Sanity: GetByName must NOT see the row.
	if _, err := wf.GetByName(ctx, def.Name); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("post-delete GetByName: want ErrNotFound, got %v", err)
	}
	// The key assertion: re-Create with the same name must succeed.
	if _, err := wf.Create(ctx, def, false); err != nil {
		t.Fatalf("Create #2 after Delete: %v (the phantom-index workaround in Store.Delete may have regressed)", err)
	}
}

func TestFindStepByName(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	def, _ := workflow.ParseFile("../../docs/examples/workflows/workflow-example.json")
	w, err := wf.Create(ctx, def, false)
	if err != nil {
		t.Fatal(err)
	}
	st, err := wf.FindStepByName(ctx, w.ID, "review")
	if err != nil {
		t.Fatal(err)
	}
	if st.Name != "review" || st.AgentName != "code-reviewer" {
		t.Fatalf("unexpected step: %+v", st)
	}
	if len(st.Transitions) != 2 {
		t.Fatalf("transitions: %d", len(st.Transitions))
	}
	_, err = wf.FindStepByName(ctx, w.ID, "nope")
	if !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func stepNames(steps []workflow.Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out
}

func workflowNames(ws []workflow.Workflow) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.Name
	}
	return out
}
