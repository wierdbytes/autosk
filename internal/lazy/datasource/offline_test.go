package datasource_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"autosk/internal/agent"
	"autosk/internal/agent/pkgregistry"
	"autosk/internal/globalworkflow"
	"autosk/internal/lazy/datasource"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
)

func newOfflineFx(t *testing.T) (*datasource.Offline, *doltlite.Store, func()) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	ts := doltlite.New()
	if err := ts.Open(ctx, filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := ts.Migrate(ctx); err != nil {
		_ = ts.Close()
		t.Fatalf("migrate: %v", err)
	}
	ds, err := datasource.NewOffline(ts, dir, nil)
	if err != nil {
		_ = ts.Close()
		t.Fatalf("offline: %v", err)
	}
	return ds, ts, func() { _ = ts.Close() }
}

func TestOffline_CreateAndListTasks(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()

	id, err := ds.CreateTask(ctx, "Refactor token validator", "", 1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	tasks, err := ds.Tasks(ctx, datasource.DefaultTaskFilter())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ID != id {
		t.Fatalf("got %d tasks, want 1; ids=%v", len(tasks), tasks)
	}
	if tasks[0].Title != "Refactor token validator" {
		t.Fatalf("title=%q", tasks[0].Title)
	}
	if tasks[0].Status != store.StatusNew {
		t.Fatalf("status=%s", tasks[0].Status)
	}
}

func TestOffline_FilterByPriority(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()
	_, _ = ds.CreateTask(ctx, "lo", "", 3)
	_, _ = ds.CreateTask(ctx, "hi", "", 0)
	p := 0
	tasks, err := ds.Tasks(ctx, datasource.TaskFilter{Statuses: store.OpenStatuses(), Priority: &p})
	if err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Title != "hi" {
		t.Fatalf("p0 returned %v", tasks)
	}
}

func TestOffline_FilterBySearch(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()
	_, _ = ds.CreateTask(ctx, "auth thing", "", 2)
	_, _ = ds.CreateTask(ctx, "logging thing", "", 2)
	tasks, _ := ds.Tasks(ctx, datasource.TaskFilter{Statuses: store.OpenStatuses(), Search: "AUTH"})
	if len(tasks) != 1 || tasks[0].Title != "auth thing" {
		t.Fatalf("search=AUTH: %v", tasks)
	}
}

func TestOffline_BlockAndUnblock(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()
	a, _ := ds.CreateTask(ctx, "parent", "", 2)
	b, _ := ds.CreateTask(ctx, "child", "", 2)
	if err := ds.Block(ctx, a, b); err != nil {
		t.Fatalf("block: %v", err)
	}
	got, err := ds.GetTask(ctx, a)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Blocked || len(got.BlockedBy) != 1 || got.BlockedBy[0].ID != b {
		t.Fatalf("expected blocked by %s, got blocked=%v blockedBy=%v", b, got.Blocked, got.BlockedBy)
	}
	if err := ds.Unblock(ctx, a, b); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	got, _ = ds.GetTask(ctx, a)
	if got.Blocked {
		t.Fatalf("still blocked after unblock")
	}
}

func TestOffline_UpdateStatusAndPriority(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()
	id, _ := ds.CreateTask(ctx, "x", "", 2)
	if err := ds.UpdateStatus(ctx, id, store.StatusDone); err != nil {
		t.Fatalf("update status: %v", err)
	}
	if err := ds.UpdatePriority(ctx, id, 0); err != nil {
		t.Fatalf("update priority: %v", err)
	}
	got, _ := ds.GetTask(ctx, id)
	if got.Status != store.StatusDone || got.Priority != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestOffline_CommentRoundTrip(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()
	id, _ := ds.CreateTask(ctx, "x", "", 2)
	if err := ds.AddComment(ctx, id, "hello"); err != nil {
		t.Fatalf("add: %v", err)
	}
	cs, err := ds.Comments(ctx, id)
	if err != nil {
		t.Fatalf("list comments: %v", err)
	}
	if len(cs) != 1 || cs[0].Text != "hello" {
		t.Fatalf("comments: %v", cs)
	}
	tk, _ := ds.GetTask(ctx, id)
	if tk.CommentCount != 1 {
		t.Fatalf("comment count = %d", tk.CommentCount)
	}
}

// TestOffline_UpdateTitleDescription pins the lazy `c` (edit) write
// path: a full pair must replace both columns and dolt-commit, while
// an empty title (post-trim) must fail without touching the row.
func TestOffline_UpdateTitleDescription(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()

	id, err := ds.CreateTask(ctx, "old title", "old body", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := ds.UpdateTitleDescription(ctx, id, "new title", "new body"); err != nil {
		t.Fatalf("update: %v", err)
	}
	tk, err := ds.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if tk.Title != "new title" || tk.Description != "new body" {
		t.Fatalf("got title=%q desc=%q, want new title / new body", tk.Title, tk.Description)
	}

	// Empty title (or whitespace-only) is rejected and the columns
	// stay at their previous values.
	if err := ds.UpdateTitleDescription(ctx, id, "   ", "whatever"); err == nil {
		t.Fatal("expected error on empty title")
	}
	tk2, _ := ds.GetTask(ctx, id)
	if tk2.Title != "new title" || tk2.Description != "new body" {
		t.Fatalf("empty-title path mutated columns: title=%q desc=%q", tk2.Title, tk2.Description)
	}
}

// TestOffline_SetMetadata pins the lazy `M` (metadata) write path:
// a wholesale replace works whether the column starts empty or
// already has keys, and an empty map clears the column back to nil.
func TestOffline_SetMetadata(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()

	id, err := ds.CreateTask(ctx, "x", "", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Seed: replace nil metadata with two keys.
	in := map[string]any{"foo": "bar", "n": float64(42)}
	if err := ds.SetMetadata(ctx, id, in); err != nil {
		t.Fatalf("set: %v", err)
	}
	tk, _ := ds.GetTask(ctx, id)
	if tk.Metadata["foo"] != "bar" {
		t.Fatalf("metadata[foo]=%v want bar", tk.Metadata["foo"])
	}
	if v, _ := tk.Metadata["n"].(float64); v != 42 {
		t.Fatalf("metadata[n]=%v want 42", tk.Metadata["n"])
	}

	// Wholesale replace: only the new keys survive.
	if err := ds.SetMetadata(ctx, id, map[string]any{"baz": "qux"}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	tk, _ = ds.GetTask(ctx, id)
	if _, ok := tk.Metadata["foo"]; ok {
		t.Fatalf("foo leaked after replace: %+v", tk.Metadata)
	}
	if tk.Metadata["baz"] != "qux" {
		t.Fatalf("metadata[baz]=%v want qux", tk.Metadata["baz"])
	}

	// Empty map clears the column. The store renders SQL NULL as a
	// nil/empty map on read.
	if err := ds.SetMetadata(ctx, id, map[string]any{}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	tk, _ = ds.GetTask(ctx, id)
	if len(tk.Metadata) != 0 {
		t.Fatalf("metadata not cleared: %+v", tk.Metadata)
	}
}

func TestOffline_HealthIsDown(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()
	h, _ := ds.Healthz(ctx)
	if h.Daemon != "down" {
		t.Fatalf("offline must report down, got %q", h.Daemon)
	}
}

func TestOffline_StreamLiveReturnsErrDaemonRequired(t *testing.T) {
	ctx := context.Background()
	ds, _, closeFn := newOfflineFx(t)
	defer closeFn()
	_, err := ds.StreamLive(ctx, "job-xxxx")
	if err != datasource.ErrDaemonRequired {
		t.Fatalf("want ErrDaemonRequired, got %v", err)
	}
}

// TestOffline_Workflows_OriginFields verifies that the Workflows list
// surfaces origin metadata (source_type, source, hash, revision) and
// computes IsStale by comparing the local hash with the global registry.
func TestOffline_Workflows_OriginFields(t *testing.T) {
	ctx := context.Background()
	ds, ts, closeFn := newOfflineFx(t)
	defer closeFn()

	// Seed a workflow with a global origin.
	ag := agent.New(ts.DB())
	if _, err := ag.EnsureByName(ctx, "human"); err != nil {
		t.Fatalf("ensure human: %v", err)
	}
	ws := workflow.New(ts.DB(), ag)
	def, _ := workflow.ParseReader(strings.NewReader(`{"name":"global-wf","first_step":"do","steps":{"do":{"agent":{"name":"human"},"next_steps":[{"step":"do","prompt_rule":"."}]}}}`))
	realHash, err := workflow.HashDefinition(def)
	if err != nil {
		t.Fatalf("hash definition: %v", err)
	}
	wf, err := ws.CreateWithOrigin(ctx, def, false, workflow.Origin{
		SourceType:     "global",
		Source:         "global-wf",
		DefinitionHash: realHash,
		Revision:       "rev-1",
	})
	if err != nil {
		t.Fatalf("create with origin: %v", err)
	}

	// Isolate from the developer/CI machine's real default global
	// workflow registry so the "registry missing" case is hermetic.
	t.Setenv("AUTOSK_WORKFLOWS", t.TempDir())

	// Without a global registry the workflow should still report its
	// origin but be marked stale (registry missing).
	wfs, err := ds.Workflows(ctx, true)
	if err != nil {
		t.Fatalf("workflows: %v", err)
	}
	var found datasource.Workflow
	for _, w := range wfs {
		if w.ID == wf.ID {
			found = w
			break
		}
	}
	if found.ID == "" {
		t.Fatal("workflow not found in list")
	}
	if found.SourceType != "global" {
		t.Fatalf("SourceType=%q want global", found.SourceType)
	}
	if found.Source != "global-wf" {
		t.Fatalf("Source=%q want global-wf", found.Source)
	}
	if found.DefinitionHash != realHash {
		t.Fatalf("DefinitionHash=%q want %s", found.DefinitionHash, realHash)
	}
	if found.Revision != "rev-1" {
		t.Fatalf("Revision=%q want rev-1", found.Revision)
	}
	if !found.IsStale {
		t.Fatal("expected IsStale=true when global registry is missing")
	}

	// Create a matching global registry entry with the SAME hash.
	globalDir := t.TempDir()
	t.Setenv("AUTOSK_WORKFLOWS", globalDir)
	reg, err := globalworkflow.Default()
	if err != nil {
		t.Fatalf("global registry: %v", err)
	}
	if err := reg.EnsurePrefix(); err != nil {
		t.Fatalf("ensure prefix: %v", err)
	}
	// Store the definition with the same hash.
	if _, err := reg.StoreDefinition(def, globalworkflow.StoreOptions{Revision: "rev-1"}); err != nil {
		t.Fatalf("store definition: %v", err)
	}

	wfs2, err := ds.Workflows(ctx, true)
	if err != nil {
		t.Fatalf("workflows second: %v", err)
	}
	for _, w := range wfs2 {
		if w.ID == wf.ID {
			found = w
			break
		}
	}
	if found.IsStale {
		t.Fatal("expected IsStale=false when hashes match")
	}

	// Update the global registry with a different hash.
	def2, _ := workflow.ParseReader(strings.NewReader(`{"name":"global-wf","first_step":"do","steps":{"do":{"agent":{"name":"human"},"next_steps":[{"step":"extra","prompt_rule":"."}]},"extra":{"agent":{"name":"human"},"next_steps":[{"step":"do","prompt_rule":"."}]}}}`))
	if _, err := reg.StoreDefinition(def2, globalworkflow.StoreOptions{Revision: "rev-2"}); err != nil {
		t.Fatalf("store definition 2: %v", err)
	}

	wfs3, err := ds.Workflows(ctx, true)
	if err != nil {
		t.Fatalf("workflows third: %v", err)
	}
	for _, w := range wfs3 {
		if w.ID == wf.ID {
			found = w
			break
		}
	}
	if !found.IsStale {
		t.Fatal("expected IsStale=true after global hash changed")
	}
}

// TestOffline_Workflows_DisabledGlobalNotStale verifies that a
// disabled global registry entry with a matching hash does NOT
// produce a false [stale] marker. Only hash mismatches or missing
// entries should trigger IsStale=true.
func TestOffline_Workflows_DisabledGlobalNotStale(t *testing.T) {
	ctx := context.Background()
	ds, ts, closeFn := newOfflineFx(t)
	defer closeFn()

	ag := agent.New(ts.DB())
	if _, err := ag.EnsureByName(ctx, "human"); err != nil {
		t.Fatalf("ensure human: %v", err)
	}
	ws := workflow.New(ts.DB(), ag)
	def, _ := workflow.ParseReader(strings.NewReader(`{"name":"global-wf","first_step":"do","steps":{"do":{"agent":{"name":"human"},"next_steps":[{"step":"do","prompt_rule":"."}]}}}`))
	realHash, err := workflow.HashDefinition(def)
	if err != nil {
		t.Fatalf("hash definition: %v", err)
	}
	wf, err := ws.CreateWithOrigin(ctx, def, false, workflow.Origin{
		SourceType:     "global",
		Source:         "global-wf",
		DefinitionHash: realHash,
		Revision:       "rev-1",
	})
	if err != nil {
		t.Fatalf("create with origin: %v", err)
	}

	// Seed a global registry entry with the same hash, then disable it.
	globalDir := t.TempDir()
	t.Setenv("AUTOSK_WORKFLOWS", globalDir)
	reg, err := globalworkflow.Default()
	if err != nil {
		t.Fatalf("global registry: %v", err)
	}
	if err := reg.EnsurePrefix(); err != nil {
		t.Fatalf("ensure prefix: %v", err)
	}
	if _, err := reg.StoreDefinition(def, globalworkflow.StoreOptions{Revision: "rev-1"}); err != nil {
		t.Fatalf("store definition: %v", err)
	}
	if _, err := reg.Disable("global-wf"); err != nil {
		t.Fatalf("disable: %v", err)
	}

	wfs, err := ds.Workflows(ctx, true)
	if err != nil {
		t.Fatalf("workflows: %v", err)
	}
	var found datasource.Workflow
	for _, w := range wfs {
		if w.ID == wf.ID {
			found = w
			break
		}
	}
	if found.ID == "" {
		t.Fatal("workflow not found in list")
	}
	if found.IsStale {
		t.Fatal("expected IsStale=false when disabled global entry hash matches")
	}

	// Now change the global hash; even disabled, it should flip stale.
	def2, _ := workflow.ParseReader(strings.NewReader(`{"name":"global-wf","first_step":"do","steps":{"do":{"agent":{"name":"human"},"next_steps":[{"step":"extra","prompt_rule":"."}]},"extra":{"agent":{"name":"human"},"next_steps":[{"step":"do","prompt_rule":"."}]}}}`))
	if _, err := reg.StoreDefinition(def2, globalworkflow.StoreOptions{Revision: "rev-2"}); err != nil {
		t.Fatalf("store definition 2: %v", err)
	}
	// Re-disable because StoreDefinition re-enables.
	if _, err := reg.Disable("global-wf"); err != nil {
		t.Fatalf("re-disable: %v", err)
	}

	wfs2, err := ds.Workflows(ctx, true)
	if err != nil {
		t.Fatalf("workflows second: %v", err)
	}
	for _, w := range wfs2 {
		if w.ID == wf.ID {
			found = w
			break
		}
	}
	if !found.IsStale {
		t.Fatal("expected IsStale=true after global hash changed (still disabled)")
	}
}

// TestOffline_SyncWorkflows_MaterializesGlobal verifies that
// SyncWorkflows uses the same code path as the CLI and returns a
// report the TUI can surface.
func TestOffline_SyncWorkflows_MaterializesGlobal(t *testing.T) {
	ctx := context.Background()
	ds, ts, closeFn := newOfflineFx(t)
	defer closeFn()

	// Seed a global workflow.
	globalDir := t.TempDir()
	t.Setenv("AUTOSK_WORKFLOWS", globalDir)
	reg, err := globalworkflow.Default()
	if err != nil {
		t.Fatalf("global registry: %v", err)
	}
	if err := reg.EnsurePrefix(); err != nil {
		t.Fatalf("ensure prefix: %v", err)
	}
	ag := agent.New(ts.DB())
	if _, err := ag.EnsureByName(ctx, "human"); err != nil {
		t.Fatalf("ensure human: %v", err)
	}
	def, _ := workflow.ParseReader(strings.NewReader(`{"name":"sync-wf","first_step":"do","steps":{"do":{"agent":{"name":"human"},"next_steps":[{"step":"do","prompt_rule":"."}]}}}`))
	if _, err := reg.StoreDefinition(def, globalworkflow.StoreOptions{Revision: "r1"}); err != nil {
		t.Fatalf("store definition: %v", err)
	}

	// Sync should materialize it.
	rep, err := ds.SyncWorkflows(ctx, false, false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if !rep.Mutated() {
		t.Fatal("expected sync to mutate")
	}
	if len(rep.Workflows) != 1 {
		t.Fatalf("expected 1 workflow report, got %d", len(rep.Workflows))
	}
	if rep.Workflows[0].Name != "sync-wf" {
		t.Fatalf("name=%q want sync-wf", rep.Workflows[0].Name)
	}
	if rep.Workflows[0].Status != "added" {
		t.Fatalf("status=%q want added", rep.Workflows[0].Status)
	}

	// Second sync should be noop.
	rep2, err := ds.SyncWorkflows(ctx, false, false)
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if rep2.Mutated() {
		t.Fatal("expected second sync to be noop")
	}
	if len(rep2.Workflows) != 1 || rep2.Workflows[0].Status != "noop" {
		t.Fatalf("second sync status=%q want noop", rep2.Workflows[0].Status)
	}

	// Dry-run should not mutate.
	rep3, err := ds.SyncWorkflows(ctx, true, false)
	if err != nil {
		t.Fatalf("dry-run sync: %v", err)
	}
	if rep3.Mutated() {
		t.Fatal("dry-run should not mutate")
	}
	if !rep3.DryRun {
		t.Fatal("DryRun flag not set")
	}

	// Verify the workflow exists in the DB.
	ws := workflow.New(ts.DB(), ag)
	wf, err := ws.GetByName(ctx, "sync-wf")
	if err != nil {
		t.Fatalf("get by name: %v", err)
	}
	origin, err := ws.GetOrigin(ctx, wf.ID)
	if err != nil {
		t.Fatalf("get origin: %v", err)
	}
	if origin.SourceType != "global" {
		t.Fatalf("origin.SourceType=%q want global", origin.SourceType)
	}
}

// seedTwoStepWF installs developer + code-reviewer agents and a tiny
// workflow with two steps named "dev" and "review". Used by the
// Enroll / Resume tests below to exercise workflow.EnterStep without
// pulling in the full executor fixture.
func seedTwoStepWF(t *testing.T, ts *doltlite.Store, maxVisitsDev, maxVisitsReview int) (workflow.Workflow, string, string) {
	t.Helper()
	ctx := context.Background()
	ag := agent.New(ts.DB())
	for _, name := range []string{"developer", "code-reviewer"} {
		if _, err := ag.EnsureByName(ctx, name); err != nil {
			t.Fatalf("ensure agent %s: %v", name, err)
		}
	}
	ws := workflow.New(ts.DB(), ag)
	body := `{
		"name": "twostep",
		"first_step": "dev",
		"steps": {
			"dev":    {"agent": {"name": "developer"},     "max_visits": ` +
		strconv.Itoa(maxVisitsDev) + `, "next_steps": [{"step": "review", "prompt_rule": "."}]},
			"review": {"agent": {"name": "code-reviewer"}, "max_visits": ` +
		strconv.Itoa(maxVisitsReview) + `, "next_steps": [{"step": "dev", "prompt_rule": "."}]}
		}}`
	def, err := workflow.ParseReader(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse wf: %v", err)
	}
	wf, err := ws.Create(ctx, def, false)
	if err != nil {
		t.Fatalf("create wf: %v", err)
	}
	var devID, revID string
	for _, s := range wf.Steps {
		switch s.Name {
		case "dev":
			devID = s.ID
		case "review":
			revID = s.ID
		}
	}
	return wf, devID, revID
}

// TestOfflineEnroll_BumpsFirstStepCounter is the regression test for
// (B): lazy.Enroll must route through workflow.EnterStep so the
// step_visits counter on the entry step is bumped to 1 — same shape
// as `autosk enroll` on the CLI. Before the fix the lazy code path
// did a raw UPDATE that left step_visits empty, so an ask-00290f-style
// loop ended up off-by-one on the source counter.
func TestOfflineEnroll_BumpsFirstStepCounter(t *testing.T) {
	ctx := context.Background()
	ds, ts, closeFn := newOfflineFx(t)
	defer closeFn()
	wf, devID, _ := seedTwoStepWF(t, ts, 5, 5)

	id, err := ds.CreateTask(ctx, "x", "", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := ds.Enroll(ctx, id, wf.Name, ""); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	tk, err := ts.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if tk.WorkflowID != wf.ID {
		t.Fatalf("workflow_id: %q (want %q)", tk.WorkflowID, wf.ID)
	}
	if tk.CurrentStepID != devID {
		t.Fatalf("current_step_id: %q (want dev=%q)", tk.CurrentStepID, devID)
	}
	if tk.Status != store.StatusWork {
		t.Fatalf("status: %s (want work)", tk.Status)
	}
	sv, _ := tk.Metadata["step_visits"].(map[string]any)
	if sv == nil {
		t.Fatalf("step_visits missing; metadata=%+v", tk.Metadata)
	}
	if v, _ := sv[devID].(float64); int(v) != 1 {
		t.Fatalf("step_visits[dev]=%v (want 1)", sv[devID])
	}
}

// TestOfflineEnroll_HonoursExplicitStep pins the CLI-parity branch
// of the lazy two-pane picker (ask-52bb49): when stepName is
// non-empty, Enroll lands on the named step instead of the
// workflow's first_step, and the per-step step_visits counter for
// the chosen step bumps to 1 (NOT first_step). Mirrors the CLI's
// `autosk enroll --step review` shape covered in
// cmd/autosk/enroll_test.go.
func TestOfflineEnroll_HonoursExplicitStep(t *testing.T) {
	ctx := context.Background()
	ds, ts, closeFn := newOfflineFx(t)
	defer closeFn()
	wf, devID, revID := seedTwoStepWF(t, ts, 5, 5)

	id, err := ds.CreateTask(ctx, "x", "", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := ds.Enroll(ctx, id, wf.Name, "review"); err != nil {
		t.Fatalf("enroll --step review: %v", err)
	}
	tk, err := ts.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if tk.WorkflowID != wf.ID {
		t.Fatalf("workflow_id: %q (want %q)", tk.WorkflowID, wf.ID)
	}
	if tk.CurrentStepID != revID {
		t.Fatalf("current_step_id: %q (want review=%q; first_step was dev=%q)", tk.CurrentStepID, revID, devID)
	}
	if tk.Status != store.StatusWork {
		t.Fatalf("status: %s (want work)", tk.Status)
	}
	sv, _ := tk.Metadata["step_visits"].(map[string]any)
	if sv == nil {
		t.Fatalf("step_visits missing; metadata=%+v", tk.Metadata)
	}
	if v, _ := sv[revID].(float64); int(v) != 1 {
		t.Fatalf("step_visits[review]=%v (want 1)", sv[revID])
	}
	if v, _ := sv[devID].(float64); int(v) != 0 {
		t.Fatalf("step_visits[dev]=%v (want 0; --step review must not bump first_step)", sv[devID])
	}

	// Unknown step name surfaces a clear hint and does NOT mutate
	// the task (rolled back to a fresh task to exercise the empty
	// starting state).
	id2, err := ds.CreateTask(ctx, "y", "", 2)
	if err != nil {
		t.Fatalf("create2: %v", err)
	}
	err = ds.Enroll(ctx, id2, wf.Name, "nope")
	if err == nil {
		t.Fatal("expected error on unknown step name")
	}
	if !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), "not found") {
		t.Errorf("err %q should mention the missing step name", err.Error())
	}
	// CLI-parity (review R3): the error must enumerate the available
	// step names so the lazy flash is actionable. Mirrors the CLI's
	// TestEnroll_AtSpecificStep_Unknown assertion in
	// cmd/autosk/enroll_test.go.
	if !strings.Contains(err.Error(), "available:") ||
		!strings.Contains(err.Error(), "dev") ||
		!strings.Contains(err.Error(), "review") {
		t.Errorf("err %q should list available step names (dev, review)", err.Error())
	}
	tk2, _ := ts.GetTask(ctx, id2)
	if tk2.WorkflowID != "" || tk2.CurrentStepID != "" || tk2.Status != store.StatusNew {
		t.Fatalf("unknown-step path mutated task: %+v", tk2)
	}
}

// TestOfflineResume_WithTo_BumpsTargetStep covers the deliberate-
// transition branch of Resume. Park a task on "dev", Resume(--to
// review), assert the target counter bumped exactly once and
// current_step_id moved to review.
func TestOfflineResume_WithTo_BumpsTargetStep(t *testing.T) {
	ctx := context.Background()
	ds, ts, closeFn := newOfflineFx(t)
	defer closeFn()
	wf, devID, revID := seedTwoStepWF(t, ts, 5, 5)

	id, err := ds.CreateTask(ctx, "z", "", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := ds.Enroll(ctx, id, wf.Name, ""); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	// Park on dev so Resume is callable. We bypass the executor by
	// flipping status directly — the only Resume contract is that the
	// task is in human.
	parked := store.StatusHuman
	if _, err := ts.UpdateTask(ctx, id, store.TaskPatch{Status: &parked}); err != nil {
		t.Fatalf("park: %v", err)
	}
	if err := ds.Resume(ctx, id, "review"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	tk, _ := ts.GetTask(ctx, id)
	if tk.CurrentStepID != revID {
		t.Fatalf("current_step_id: %q (want review=%q)", tk.CurrentStepID, revID)
	}
	if tk.Status != store.StatusWork {
		t.Fatalf("status: %s (want work)", tk.Status)
	}
	sv, _ := tk.Metadata["step_visits"].(map[string]any)
	if v, _ := sv[revID].(float64); int(v) != 1 {
		t.Fatalf("step_visits[review]=%v (want 1)", sv[revID])
	}
	// dev counter remains at 1 from the original Enroll (no resume bump).
	if v, _ := sv[devID].(float64); int(v) != 1 {
		t.Fatalf("step_visits[dev]=%v (want 1; resume --to STEP must not touch the source)", sv[devID])
	}
}

// TestOfflineResume_NoTo_NoCounterBump covers the no-transition
// branch of Resume. Park a task on "dev", Resume(""), assert the
// step pointer and counters are unchanged — only the status flips
// back to work. Pins the documented "no transition" rule
// (docs/plans/20260520-Step-Visit-Limits.md) for the lazy surface.
func TestOfflineResume_NoTo_NoCounterBump(t *testing.T) {
	ctx := context.Background()
	ds, ts, closeFn := newOfflineFx(t)
	defer closeFn()
	wf, devID, _ := seedTwoStepWF(t, ts, 5, 5)

	id, err := ds.CreateTask(ctx, "r", "", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := ds.Enroll(ctx, id, wf.Name, ""); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	pre, _ := ts.GetTask(ctx, id)
	// Park (status flip only — leaves current_step_id / metadata alone).
	parked := store.StatusHuman
	if _, err := ts.UpdateTask(ctx, id, store.TaskPatch{Status: &parked}); err != nil {
		t.Fatalf("park: %v", err)
	}
	if err := ds.Resume(ctx, id, ""); err != nil {
		t.Fatalf("resume: %v", err)
	}
	tk, _ := ts.GetTask(ctx, id)
	if tk.Status != store.StatusWork {
		t.Fatalf("status: %s (want work)", tk.Status)
	}
	if tk.CurrentStepID != devID {
		t.Fatalf("current_step_id: %q (want dev=%q; no-transition resume must not move the pointer)", tk.CurrentStepID, devID)
	}
	// Counters must be byte-identical to what Enroll left behind.
	preSV, _ := pre.Metadata["step_visits"].(map[string]any)
	postSV, _ := tk.Metadata["step_visits"].(map[string]any)
	if v, _ := postSV[devID].(float64); int(v) != 1 {
		t.Fatalf("step_visits[dev]=%v (want 1; no-transition resume must not bump)", postSV[devID])
	}
	if len(preSV) != len(postSV) {
		t.Fatalf("step_visits len pre=%d post=%d (no-transition resume must not add keys)", len(preSV), len(postSV))
	}
}

// TestOfflineResume_NoTo_RefusesWhenNoCurrentStep mirrors the CLI's
// `task has no current_step_id; pass --to STEP` guard in
// cmd/autosk/resume.go. Without the guard the lazy path would trip
// the SQL CHECK invariant (status='work' ⇔ current_step_id IS NOT
// NULL) and surface a cryptic doltlite error in the flash bar
// instead of an actionable hint.
func TestOfflineResume_NoTo_RefusesWhenNoCurrentStep(t *testing.T) {
	ctx := context.Background()
	ds, ts, closeFn := newOfflineFx(t)
	defer closeFn()
	wf, _, _ := seedTwoStepWF(t, ts, 5, 5)

	id, err := ds.CreateTask(ctx, "r2", "", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := ds.Enroll(ctx, id, wf.Name, ""); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	// Hand-edit to mimic the pathological state: parked in
	// human with current_step_id NULL. The schema allows
	// human with a NULL step (it's only work that
	// requires it), so the corrupted intermediate state is reachable.
	parked := store.StatusHuman
	empty := ""
	if _, err := ts.UpdateTask(ctx, id, store.TaskPatch{Status: &parked, CurrentStepID: &empty}); err != nil {
		t.Fatalf("park: %v", err)
	}
	err = ds.Resume(ctx, id, "")
	if err == nil {
		t.Fatal("expected error; lazy resume without --to must refuse when current_step_id is empty")
	}
	if !strings.Contains(err.Error(), "pass --to STEP") {
		t.Fatalf("err %q should hint at `pass --to STEP`", err.Error())
	}
	// Status must NOT have flipped — the operator has to choose a
	// target step before the task moves anywhere.
	tk, _ := ts.GetTask(ctx, id)
	if tk.Status != store.StatusHuman {
		t.Fatalf("status flipped on refused resume: %s (want human)", tk.Status)
	}
}

// TestOfflineEnroll_CapHitOnFirstEntry verifies the cap-exceeded path
// on Enroll: with step_visits[dev]=1 already present (e.g. left over
// after a partial reset) and dev.max_visits=1, Enroll must surface a
// typed MaxVisitsExceededError-derived message and NOT mutate the
// task's workflow_id / current_step_id.
func TestOfflineEnroll_CapHitOnFirstEntry(t *testing.T) {
	ctx := context.Background()
	ds, ts, closeFn := newOfflineFx(t)
	defer closeFn()
	wf, devID, _ := seedTwoStepWF(t, ts, 1, 5) // dev capped at 1.

	id, err := ds.CreateTask(ctx, "x", "", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Prime the counter to 1 so the next bump would cross the cap.
	md := map[string]any{"step_visits": map[string]any{devID: 1}}
	if _, err := ts.UpdateTask(ctx, id, store.TaskPatch{Metadata: &md}); err != nil {
		t.Fatalf("prime metadata: %v", err)
	}
	err = ds.Enroll(ctx, id, wf.Name, "")
	if err == nil {
		t.Fatal("expected cap-exceeded error")
	}
	// mapEnterStepError wraps the typed error but keeps the chain intact.
	var mve workflow.MaxVisitsExceededError
	if !errors.As(err, &mve) {
		t.Fatalf("err %T: %v (want MaxVisitsExceededError in chain)", err, err)
	}
	if mve.StepID != devID {
		t.Fatalf("mve.StepID: %q (want %q)", mve.StepID, devID)
	}
	if !strings.Contains(err.Error(), "reset-visits") {
		t.Fatalf("err %q should hint at reset-visits", err.Error())
	}
	// Task pointer must not have moved.
	tk, _ := ts.GetTask(ctx, id)
	if tk.WorkflowID != "" {
		t.Fatalf("workflow_id leaked on cap-fire: %q", tk.WorkflowID)
	}
	if tk.CurrentStepID != "" {
		t.Fatalf("current_step_id leaked on cap-fire: %q", tk.CurrentStepID)
	}
	if tk.Status != store.StatusNew {
		t.Fatalf("status flipped on cap-fire: %s (want new)", tk.Status)
	}
}

// TestOfflineEnroll_FromHuman_OK pins the lazy parity with the CLI's
// `enroll from human` happy path: a parked task accepts enroll cleanly
// and lands at the workflow's first step with status flipped back to
// work. Mirrors cmd/autosk's TestEnroll_FromHuman_OK.
func TestOfflineEnroll_FromHuman_OK(t *testing.T) {
	ctx := context.Background()
	ds, ts, closeFn := newOfflineFx(t)
	defer closeFn()
	wf, devID, _ := seedTwoStepWF(t, ts, 5, 5)

	id, err := ds.CreateTask(ctx, "park me", "", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := ds.Enroll(ctx, id, wf.Name, ""); err != nil {
		t.Fatalf("first enroll: %v", err)
	}
	// Force into human without touching current_step_id (the SQL
	// CHECK allows that combination; only `status='work'` requires
	// the pointer to be non-null).
	parked := store.StatusHuman
	if _, err := ts.UpdateTask(ctx, id, store.TaskPatch{Status: &parked}); err != nil {
		t.Fatalf("park: %v", err)
	}

	if err := ds.Enroll(ctx, id, wf.Name, ""); err != nil {
		t.Fatalf("re-enroll from human: %v", err)
	}
	tk, _ := ts.GetTask(ctx, id)
	if tk.Status != store.StatusWork {
		t.Fatalf("status: %s (want work)", tk.Status)
	}
	if tk.CurrentStepID != devID {
		t.Fatalf("current_step_id: %q (want dev=%q)", tk.CurrentStepID, devID)
	}
	if tk.WorkflowID != wf.ID {
		t.Fatalf("workflow_id: %q (want %q)", tk.WorkflowID, wf.ID)
	}
}

// TestOfflineEnroll_FromDone_OK pins parity with the CLI's
// `enroll from done` happy path.
func TestOfflineEnroll_FromDone_OK(t *testing.T) {
	ctx := context.Background()
	ds, ts, closeFn := newOfflineFx(t)
	defer closeFn()
	wf, devID, _ := seedTwoStepWF(t, ts, 5, 5)

	id, err := ds.CreateTask(ctx, "done me", "", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Force a terminal source state directly — mirrors what
	// `autosk done` would leave behind (status flipped, no
	// current_step_id pointer).
	done := store.StatusDone
	empty := ""
	if _, err := ts.UpdateTask(ctx, id, store.TaskPatch{Status: &done, CurrentStepID: &empty}); err != nil {
		t.Fatalf("terminate: %v", err)
	}

	if err := ds.Enroll(ctx, id, wf.Name, ""); err != nil {
		t.Fatalf("enroll from done: %v", err)
	}
	tk, _ := ts.GetTask(ctx, id)
	if tk.Status != store.StatusWork {
		t.Fatalf("status: %s (want work)", tk.Status)
	}
	if tk.CurrentStepID != devID {
		t.Fatalf("current_step_id: %q (want dev=%q)", tk.CurrentStepID, devID)
	}
	if tk.WorkflowID != wf.ID {
		t.Fatalf("workflow_id: %q (want %q)", tk.WorkflowID, wf.ID)
	}
}

// TestOfflineEnroll_FromCancel_OK pins parity with the CLI's
// `enroll from cancel` happy path.
func TestOfflineEnroll_FromCancel_OK(t *testing.T) {
	ctx := context.Background()
	ds, ts, closeFn := newOfflineFx(t)
	defer closeFn()
	wf, devID, _ := seedTwoStepWF(t, ts, 5, 5)

	id, err := ds.CreateTask(ctx, "cancel me", "", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	cancelled := store.StatusCancel
	empty := ""
	if _, err := ts.UpdateTask(ctx, id, store.TaskPatch{Status: &cancelled, CurrentStepID: &empty}); err != nil {
		t.Fatalf("terminate: %v", err)
	}

	if err := ds.Enroll(ctx, id, wf.Name, ""); err != nil {
		t.Fatalf("enroll from cancel: %v", err)
	}
	tk, _ := ts.GetTask(ctx, id)
	if tk.Status != store.StatusWork {
		t.Fatalf("status: %s (want work)", tk.Status)
	}
	if tk.CurrentStepID != devID {
		t.Fatalf("current_step_id: %q (want dev=%q)", tk.CurrentStepID, devID)
	}
	if tk.WorkflowID != wf.ID {
		t.Fatalf("workflow_id: %q (want %q)", tk.WorkflowID, wf.ID)
	}
}

// TestOfflineEnroll_FromWork_Rejected fences the one remaining
// status gate. A task currently in work is owned by the engine;
// lazy must refuse to re-stamp workflow_id / current_step_id
// underneath it and the surfaced message must hint at the
// cancel + reopen + enroll detour.
func TestOfflineEnroll_FromWork_Rejected(t *testing.T) {
	ctx := context.Background()
	ds, ts, closeFn := newOfflineFx(t)
	defer closeFn()
	wf, devID, _ := seedTwoStepWF(t, ts, 5, 5)

	id, err := ds.CreateTask(ctx, "work me", "", 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := ds.Enroll(ctx, id, wf.Name, ""); err != nil {
		t.Fatalf("first enroll: %v", err)
	}
	pre, _ := ts.GetTask(ctx, id)
	if pre.Status != store.StatusWork {
		t.Fatalf("setup failed: status=%s (want work)", pre.Status)
	}

	err = ds.Enroll(ctx, id, wf.Name, "")
	if err == nil {
		t.Fatal("expected enroll on work task to fail")
	}
	if !strings.Contains(err.Error(), "work") {
		t.Errorf("err %q should mention status='work'", err.Error())
	}
	if !strings.Contains(err.Error(), "cancel") || !strings.Contains(err.Error(), "reopen") {
		t.Errorf("err %q should hint at cancel + reopen", err.Error())
	}
	post, _ := ts.GetTask(ctx, id)
	// The task pointer must be byte-identical to the pre-state.
	if post.WorkflowID != pre.WorkflowID || post.CurrentStepID != pre.CurrentStepID || post.Status != pre.Status {
		t.Fatalf("work task mutated on refused enroll: pre=%+v post=%+v", pre, post)
	}
	// And the entry counter must NOT have bumped a second time.
	sv, _ := post.Metadata["step_visits"].(map[string]any)
	if v, _ := sv[devID].(float64); int(v) != 1 {
		t.Errorf("step_visits[dev]=%v (want 1; refused enroll must not bump)", sv[devID])
	}
}

// versionTrackingNpm is a fake npm runner that records every install
// spec so tests can assert the exact version passed to npm.
type versionTrackingNpm struct {
	installs []string
}

func (n *versionTrackingNpm) Install(_ context.Context, prefix, spec string) error {
	n.installs = append(n.installs, spec)
	name := spec
	if i := strings.LastIndex(spec, "@"); i > 0 {
		name = spec[:i]
	}
	dir := filepath.Join(prefix, "node_modules", filepath.FromSlash(name))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	pj := map[string]any{
		"name":    name,
		"version": "1.0.0",
		"autosk":  map[string]any{"agent": map[string]any{"first_message": "hello"}},
	}
	b, _ := json.MarshalIndent(pj, "", "  ")
	return os.WriteFile(filepath.Join(dir, "package.json"), b, 0o600)
}

func (n *versionTrackingNpm) Uninstall(_ context.Context, prefix, name string) error {
	return os.RemoveAll(filepath.Join(prefix, "node_modules", filepath.FromSlash(name)))
}

// TestOffline_SyncWorkflows_PinsPackageVersion verifies that lazy
// SyncWorkflows passes the package-backed global's pinned version
// into agent auto-install.
func TestOffline_SyncWorkflows_PinsPackageVersion(t *testing.T) {
	ctx := context.Background()
	_, ts, closeFn := newOfflineFx(t)
	defer closeFn()

	npm := &versionTrackingNpm{}
	prefix := filepath.Join(t.TempDir(), "packages")
	reg, err := pkgregistry.Open(prefix, pkgregistry.WithNpm(npm))
	if err != nil {
		t.Fatal(err)
	}

	// Re-create datasource with the registry so SyncWorkflows can
	// auto-install agents.
	ds, err := datasource.NewOffline(ts, t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}

	// Seed global registry with a package-backed workflow referencing
	// a scoped agent.
	globalDir := t.TempDir()
	t.Setenv("AUTOSK_WORKFLOWS", globalDir)
	gwr, err := globalworkflow.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := gwr.EnsurePrefix(); err != nil {
		t.Fatal(err)
	}
	def := workflow.Definition{
		Name:      "pinned-sync-wf",
		FirstStep: "do",
		Steps: map[string]workflow.StepDef{
			"do": {
				AgentName: "@scope/pinned-agent",
				NextSteps: []workflow.TransitionDef{{TaskStatus: "done", PromptRule: "Done."}},
			},
		},
	}
	if _, err := gwr.StoreDefinition(def, globalworkflow.StoreOptions{
		SourceType:     "package",
		Source:         "@scope/pinned-agent",
		SourceMetadata: map[string]any{"version": "2.0.0"},
	}); err != nil {
		t.Fatal(err)
	}

	_, err = ds.SyncWorkflows(ctx, false, false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}

	found := false
	for _, spec := range npm.installs {
		if spec == "@scope/pinned-agent@2.0.0" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected npm install spec @scope/pinned-agent@2.0.0, got %v", npm.installs)
	}
}
