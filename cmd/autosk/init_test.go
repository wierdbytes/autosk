package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"autosk/internal/globalworkflow"
	"autosk/internal/worktree"
)

// TestInit_BootstrapsFeatureDevGeneric covers AC1-AC4: after a plain
// `autosk init` on a clean dir the user can immediately enroll work
// into the seeded `feature-dev-generic` workflow.
//
// Since feature-dev-generic now ships with `isolation: worktree` by
// default, the AC4 `create --workflow feature-dev-generic` block
// requires the project dir to be a real git repo (so worktree.Ensure
// can `git worktree add`) and a hermetic $HOME (so the allocated
// worktree lands under a sweepable temp dir instead of the
// developer's real ~/.autosk/worktrees/...).
func TestInit_BootstrapsFeatureDevGeneric(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed; feature-dev-generic now defaults to isolation=worktree which needs git")
	}
	withIsolatedPackagesPrefix(t)
	// Hermetic $HOME for the lifetime of the subtest: worktree.PathFor
	// derives its destination from $HOME, so without this the AC4
	// `create --workflow feature-dev-generic` would spray
	// ~/.autosk/worktrees/<slug>/<task-id> into the developer's real
	// home dir.
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	// Promote the project root to a git repo with one commit so HEAD
	// resolves — mirrors the fixture in internal/worktree/worktree_test.go.
	gitInitOrFail(t, dir)
	out, err := runRoot(t, dir, "init")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if !strings.Contains(out, "initialized") {
		t.Errorf("init output missing 'initialized':\n%s", out)
	}
	if !strings.Contains(out, "bootstrapped workflow feature-dev-generic") {
		t.Errorf("expected bootstrap line:\n%s", out)
	}
	// Plan §4.6: the bootstrap success line carries the installed
	// agent + version so a `--quiet` user grepping stdout still has
	// the version on the canonical token. The version string is
	// minted by the in-process fake-npm runner; we only assert on
	// the agent name + the `installed)` suffix to stay version-agnostic.
	if !strings.Contains(out, "agent @autogent/generic@") || !strings.Contains(out, " installed)") {
		t.Errorf("expected bootstrap line to include the agent + version suffix per plan §4.6:\n%s", out)
	}

	// AC1: `autosk workflow list` shows the seeded workflow.
	list, err := runRoot(t, dir, "workflow", "list")
	if err != nil {
		t.Fatalf("workflow list: %v\n%s", err, list)
	}
	if !strings.Contains(list, "feature-dev-generic") {
		t.Errorf("workflow list missing feature-dev-generic:\n%s", list)
	}

	// AC2: workflow content matches — step graph is
	// dev → review → docs → validator → human, including the
	// bounce-back edges `review → dev` and `validator → dev`.
	show, err := runRoot(t, dir, "workflow", "show", "feature-dev-generic", "--json")
	if err != nil {
		t.Fatalf("workflow show: %v\n%s", err, show)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(show)), &meta); err != nil {
		t.Fatalf("unmarshal workflow show: %v\nraw=%s", err, show)
	}
	if meta["is_synthetic"] != false {
		t.Errorf("workflow should be non-synthetic: %v", meta["is_synthetic"])
	}
	// AC2 (isolation): fresh init must seed the workflow with
	// isolation=worktree (the new bootstrap default). Pinned here at
	// the persisted-DB level; bootstrap_test.go pins it at the
	// embedded-JSON level.
	if got := meta["isolation"]; got != "worktree" {
		t.Errorf("workflow isolation = %v, want \"worktree\"", got)
	}
	steps, _ := meta["steps"].([]any)
	wantNames := []string{"dev", "review", "docs", "validator"}
	if len(steps) != len(wantNames) {
		t.Fatalf("steps len = %d, want %d (%v)", len(steps), len(wantNames), steps)
	}
	for i, want := range wantNames {
		sm, _ := steps[i].(map[string]any)
		if got, _ := sm["name"].(string); got != want {
			t.Errorf("step[%d].name = %q, want %q", i, got, want)
		}
		if got, _ := sm["agent_name"].(string); got != "@autogent/generic" {
			t.Errorf("step[%d].agent_name = %q, want @autogent/generic", i, got)
		}
	}

	// AC2 (transitions): assert the canonical edges by matching on
	// next_step_name + task_status. We don't byte-equal the
	// prompt_rule strings here — bootstrap_test.go already pins the
	// source JSON, this e2e check is about whether Create persists
	// the graph faithfully.
	assertEdges := func(stepIdx int, want []map[string]string) {
		t.Helper()
		sm, _ := steps[stepIdx].(map[string]any)
		name, _ := sm["name"].(string)
		trs, _ := sm["transitions"].([]any)
		if len(trs) != len(want) {
			t.Errorf("step %q: got %d transitions, want %d (%v)", name, len(trs), len(want), trs)
			return
		}
		got := make(map[string]string, len(trs))
		for _, tr := range trs {
			tm, _ := tr.(map[string]any)
			ns, _ := tm["next_step_name"].(string)
			ts, _ := tm["task_status"].(string)
			key := "step:" + ns
			if ns == "" {
				key = "task_status:" + ts
			}
			got[key] = ns + "|" + ts
		}
		for _, w := range want {
			key := "step:" + w["next_step_name"]
			if w["next_step_name"] == "" {
				key = "task_status:" + w["task_status"]
			}
			if _, ok := got[key]; !ok {
				t.Errorf("step %q: missing transition %v (got %v)", name, w, got)
			}
		}
	}
	assertEdges(0, []map[string]string{{"next_step_name": "review"}})
	assertEdges(1, []map[string]string{
		{"next_step_name": "docs"},
		{"next_step_name": "dev"},
	})
	assertEdges(2, []map[string]string{{"next_step_name": "validator"}})
	assertEdges(3, []map[string]string{
		{"next_step_name": "dev"},
		{"task_status": "human"},
	})

	// AC3: the agent is in the DB.
	agents, err := runRoot(t, dir, "agent", "list")
	if err != nil {
		t.Fatalf("agent list: %v\n%s", err, agents)
	}
	if !strings.Contains(agents, "@autogent/generic") {
		t.Errorf("agent list missing @autogent/generic:\n%s", agents)
	}

	// AC4: a fresh task lands at step `dev` with @autogent/generic.
	createOut, err := runRoot(t, dir, "create", "smoke", "--workflow", "feature-dev-generic")
	if err != nil {
		t.Fatalf("create --workflow: %v\n%s", err, createOut)
	}
	id := lastLine(createOut)
	if !strings.HasPrefix(id, "ask-") {
		t.Fatalf("did not get task id: %q", createOut)
	}
	got := statusOf(t, dir, id)
	if got["status"] != "work" {
		t.Errorf("task status = %v, want work", got["status"])
	}
	if got["current_step"] != "dev" {
		t.Errorf("task current_step = %v, want dev", got["current_step"])
	}
	if got["current_agent"] != "@autogent/generic" {
		t.Errorf("task current_agent = %v, want @autogent/generic", got["current_agent"])
	}
	// AC4 (worktree allocation): with isolation=worktree the create
	// path runs worktree.Ensure, which materialises
	// $HOME/.autosk/worktrees/<slug>/<task-id> and the branch
	// autosk/<task-id> on the project repo.
	wtPath, perr := worktree.PathFor(dir, id)
	if perr != nil {
		t.Fatalf("worktree.PathFor: %v", perr)
	}
	if _, statErr := os.Stat(wtPath); statErr != nil {
		t.Errorf("worktree dir should exist at %s: %v", wtPath, statErr)
	}
}

// gitInitOrFail initialises `dir` as a git repo with one empty commit
// on `main`, configuring a local user identity so subsequent
// `git worktree add` commands have a valid HEAD to fork from. Mirrors
// the helper in internal/worktree/worktree_test.go but kept package-local
// here to avoid a test-only dependency cycle.
func gitInitOrFail(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "--initial-branch=main")
	run("config", "user.email", "test@autosk.local")
	run("config", "user.name", "autosk test")
	run("commit", "--allow-empty", "-m", "initial")
}

// TestInit_Idempotent covers AC5 + AC9: a second init is a no-op and a
// deleted workflow can be re-seeded by re-running init.
func TestInit_Idempotent(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init"); err != nil {
		t.Fatalf("init: %v", err)
	}

	// AC5: re-running init exits 0, does not duplicate the row, and
	// surfaces the "already present" line.
	out, err := runRoot(t, dir, "init")
	if err != nil {
		t.Fatalf("init re-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "already present") {
		t.Errorf("expected 'already present' line:\n%s", out)
	}
	if strings.Contains(out, "bootstrapped workflow") {
		t.Errorf("re-run should NOT emit 'bootstrapped workflow':\n%s", out)
	}

	list, _ := runRoot(t, dir, "workflow", "list")
	if strings.Count(list, "feature-dev-generic") != 1 {
		t.Errorf("workflow list should contain feature-dev-generic exactly once:\n%s", list)
	}

	// AC9: delete the workflow, init again, it comes back.
	if _, err := runRoot(t, dir, "workflow", "delete", "feature-dev-generic"); err != nil {
		t.Fatalf("workflow delete: %v", err)
	}
	out, err = runRoot(t, dir, "init")
	if err != nil {
		t.Fatalf("init after delete: %v\n%s", err, out)
	}
	if !strings.Contains(out, "bootstrapped workflow feature-dev-generic") {
		t.Errorf("expected re-bootstrap after delete:\n%s", out)
	}
	list, _ = runRoot(t, dir, "workflow", "list")
	if !strings.Contains(list, "feature-dev-generic") {
		t.Errorf("re-bootstrap did not produce a workflow row:\n%s", list)
	}
}

// TestInit_SkipBootstrap covers AC6 + AC7: --skip-bootstrap on a clean
// dir leaves the workflow list empty, and running --skip-bootstrap
// after a previous bootstrap does not undo the seed.
func TestInit_UnmanagedBootstrapNameSkipsBeforeAgentInstall(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatalf("init --skip-bootstrap: %v", err)
	}

	wf := map[string]any{
		"name":       "feature-dev-generic",
		"first_step": "hold",
		"steps": map[string]any{
			"hold": map[string]any{
				"agent": map[string]any{"name": "human"},
				"next_steps": []any{
					map[string]any{"task_status": "human", "prompt_rule": "pause"},
				},
			},
		},
	}
	body, _ := json.MarshalIndent(wf, "", "  ")
	wfPath := filepath.Join(dir, "unmanaged-bootstrap-name.json")
	if err := os.WriteFile(wfPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runRoot(t, dir, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatalf("workflow create unmanaged collision: %v\n%s", err, out)
	}

	out, err := runRoot(t, dir, "init")
	if err != nil {
		t.Fatalf("init over unmanaged bootstrap-name workflow: %v\n%s", err, out)
	}
	if !strings.Contains(out, "already present") {
		t.Errorf("expected already-present skip line:\n%s", out)
	}
	if strings.Contains(out, "installing") || strings.Contains(out, "agent install @autogent/generic") {
		t.Errorf("init should not auto-install before unmanaged no-op detection:\n%s", out)
	}
	agents, err := runRoot(t, dir, "agent", "list")
	if err != nil {
		t.Fatalf("agent list: %v\n%s", err, agents)
	}
	if strings.Contains(agents, "@autogent/generic") {
		t.Errorf("init installed bootstrap agent despite unmanaged workflow collision:\n%s", agents)
	}
}

func TestInit_SkipBootstrap(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()

	// AC6: clean dir + --skip-bootstrap → no workflow rows.
	out, err := runRoot(t, dir, "init", "--skip-bootstrap")
	if err != nil {
		t.Fatalf("init --skip-bootstrap: %v\n%s", err, out)
	}
	if strings.Contains(out, "bootstrapped workflow") || strings.Contains(out, "already present") {
		t.Errorf("--skip-bootstrap should not print bootstrap lines:\n%s", out)
	}
	list, _ := runRoot(t, dir, "workflow", "list")
	if strings.Contains(list, "feature-dev-generic") {
		t.Errorf("--skip-bootstrap should leave workflow list empty:\n%s", list)
	}

	// AC7: a previously-seeded workflow stays put under --skip-bootstrap.
	dir2 := t.TempDir()
	if _, err := runRoot(t, dir2, "init"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := runRoot(t, dir2, "init", "--skip-bootstrap"); err != nil {
		t.Fatalf("init --skip-bootstrap (2nd): %v", err)
	}
	list2, _ := runRoot(t, dir2, "workflow", "list")
	if !strings.Contains(list2, "feature-dev-generic") {
		t.Errorf("--skip-bootstrap should not remove an existing seed:\n%s", list2)
	}
}

// TestInit_BootstrapFailureNonFatal covers AC8: pointing the packages
// prefix at a read-only directory causes the auto-install to fail; init
// must still exit 0 with a warning on stderr and a usable .autosk/db on
// disk.
func TestInit_BootstrapFailureNonFatal(t *testing.T) {
	// Build a hermetic packages prefix the same way other tests do,
	// THEN flip it to a read-only directory so EnsurePrefix /
	// auto-install can't write into it.
	withIsolatedPackagesPrefix(t)
	roPrefix := filepath.Join(t.TempDir(), "ro")
	if err := os.MkdirAll(roPrefix, 0o500); err != nil {
		t.Fatalf("mkdir ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(roPrefix, 0o755) })
	t.Setenv("AUTOSK_PACKAGES", roPrefix)

	dir := t.TempDir()
	out, err := runRoot(t, dir, "init")
	if err != nil {
		t.Fatalf("init should still exit 0 on bootstrap failure: %v\n%s", err, out)
	}
	if !strings.Contains(out, "warning") {
		t.Errorf("expected a 'warning' line on stderr:\n%s", out)
	}
	// Pin specifically the bootstrap-flavour warning. EnsurePrefix
	// also warns under the same ro-prefix setup; without this check
	// a regression that deleted the bootstrap call site entirely
	// would still satisfy the generic Contains("warning") above.
	if !strings.Contains(out, "bootstrap default workflow") {
		t.Errorf("expected the bootstrap-flavour warning, got:\n%s", out)
	}
	// The init bootstrap warning must advertise the right opt-out
	// flag (--skip-bootstrap, which `autosk init` actually has) and
	// must NOT advertise --no-install (which only `workflow create`
	// has). Guards the regression flagged in review.
	if strings.Contains(out, "--no-install") {
		t.Errorf("init bootstrap warning must not advertise --no-install (init has no such flag); got:\n%s", out)
	}
	if !strings.Contains(out, "--skip-bootstrap") {
		t.Errorf("init bootstrap warning should advertise --skip-bootstrap; got:\n%s", out)
	}
	if !strings.Contains(out, "initialized") {
		t.Errorf("init should still report 'initialized':\n%s", out)
	}

	// The DB file must exist and be a valid migrated project root.
	if _, err := os.Stat(filepath.Join(dir, ".autosk", "db")); err != nil {
		t.Errorf(".autosk/db missing after failed bootstrap: %v", err)
	}
	// A subsequent `migrate` succeeds → schema is current.
	if _, err := runRoot(t, dir, "migrate"); err != nil {
		t.Errorf("migrate after failed bootstrap: %v", err)
	}
}

// TestInit_QuietSuppressesBootstrapLines covers AC10: --quiet emits no
// stdout on a successful init.
func TestInit_QuietSuppressesBootstrapLines(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	out, err := runRoot(t, dir, "--quiet", "init")
	if err != nil {
		t.Fatalf("init --quiet: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("expected no stdout under --quiet, got:\n%q", out)
	}

	// Re-run under --quiet: still silent, no "already present" line.
	out2, err := runRoot(t, dir, "--quiet", "init")
	if err != nil {
		t.Fatalf("init --quiet (2nd): %v\n%s", err, out2)
	}
	if strings.TrimSpace(out2) != "" {
		t.Errorf("expected no stdout under --quiet on re-run, got:\n%q", out2)
	}
}

// TestInit_QuietDoesNotSwallowBootstrapWarning is the regression test
// for the review remark on plan §4.6: --quiet must not suppress the
// bootstrap failure warning on stderr. Mirrors
// TestInit_BootstrapFailureNonFatal but with --quiet first; we still
// expect the bootstrap-flavour warning text.
func TestInit_QuietDoesNotSwallowBootstrapWarning(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	roPrefix := filepath.Join(t.TempDir(), "ro")
	if err := os.MkdirAll(roPrefix, 0o500); err != nil {
		t.Fatalf("mkdir ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(roPrefix, 0o755) })
	t.Setenv("AUTOSK_PACKAGES", roPrefix)

	dir := t.TempDir()
	out, err := runRoot(t, dir, "--quiet", "init")
	if err != nil {
		t.Fatalf("init --quiet should still exit 0 on bootstrap failure: %v\n%s", err, out)
	}
	if !strings.Contains(out, "warning") {
		t.Errorf("expected a 'warning' line on stderr even under --quiet:\n%q", out)
	}
	if !strings.Contains(out, "bootstrap default workflow") {
		t.Errorf("expected the bootstrap-flavour warning under --quiet, got:\n%q", out)
	}
	// Same flag-name regression guard as TestInit_BootstrapFailureNonFatal.
	if strings.Contains(out, "--no-install") {
		t.Errorf("init bootstrap warning must not advertise --no-install under --quiet; got:\n%q", out)
	}
	if !strings.Contains(out, "--skip-bootstrap") {
		t.Errorf("init bootstrap warning should advertise --skip-bootstrap under --quiet; got:\n%q", out)
	}
}

// TestInit_SyncsGlobalWorkflows covers that explicit init syncs enabled
// global workflows after bootstrap.
func TestInit_SyncsGlobalWorkflows(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	r := withIsolatedGlobalWorkflows(t)
	def := cliSyncDefinition("global-wf", "@autosk/dev-fixture", "global workflow")
	if _, err := r.StoreDefinition(def, globalworkflow.StoreOptions{Revision: "rev-1"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out, err := runRoot(t, dir, "init")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if !strings.Contains(out, "bootstrapped workflow feature-dev-generic") {
		t.Errorf("expected bootstrap line:\n%s", out)
	}
	if !strings.Contains(out, "workflow global-wf: added") {
		t.Errorf("expected global workflow sync line:\n%s", out)
	}
	list, err := runRoot(t, dir, "workflow", "list")
	if err != nil {
		t.Fatalf("workflow list: %v\n%s", err, list)
	}
	if !strings.Contains(list, "feature-dev-generic") {
		t.Errorf("workflow list missing bootstrap workflow:\n%s", list)
	}
	if !strings.Contains(list, "global-wf") {
		t.Errorf("workflow list missing synced global workflow:\n%s", list)
	}
}

// TestInit_SkipGlobalWorkflows covers that --skip-global-workflows
// suppresses the global workflow sync step while still running bootstrap.
func TestInit_SkipGlobalWorkflows(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	r := withIsolatedGlobalWorkflows(t)
	def := cliSyncDefinition("global-wf", "@autosk/dev-fixture", "global workflow")
	if _, err := r.StoreDefinition(def, globalworkflow.StoreOptions{Revision: "rev-1"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out, err := runRoot(t, dir, "init", "--skip-global-workflows")
	if err != nil {
		t.Fatalf("init --skip-global-workflows: %v\n%s", err, out)
	}
	if !strings.Contains(out, "bootstrapped workflow feature-dev-generic") {
		t.Errorf("expected bootstrap line:\n%s", out)
	}
	if strings.Contains(out, "workflow global-wf: added") {
		t.Errorf("--skip-global-workflows should not sync global workflows:\n%s", out)
	}
	list, err := runRoot(t, dir, "workflow", "list")
	if err != nil {
		t.Fatalf("workflow list: %v\n%s", err, list)
	}
	if !strings.Contains(list, "feature-dev-generic") {
		t.Errorf("workflow list missing bootstrap workflow:\n%s", list)
	}
	if strings.Contains(list, "global-wf") {
		t.Errorf("--skip-global-workflows should leave global workflow absent:\n%s", list)
	}
}

// TestInit_SkipBootstrapStillSyncsGlobalWorkflows ensures that
// --skip-bootstrap only suppresses the built-in bootstrap, not the
// global workflow sync.
func TestInit_SkipBootstrapStillSyncsGlobalWorkflows(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	r := withIsolatedGlobalWorkflows(t)
	def := cliSyncDefinition("global-wf", "human", "global workflow")
	if _, err := r.StoreDefinition(def, globalworkflow.StoreOptions{Revision: "rev-1"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out, err := runRoot(t, dir, "init", "--skip-bootstrap")
	if err != nil {
		t.Fatalf("init --skip-bootstrap: %v\n%s", err, out)
	}
	if strings.Contains(out, "bootstrapped workflow feature-dev-generic") {
		t.Errorf("--skip-bootstrap should not bootstrap:\n%s", out)
	}
	if !strings.Contains(out, "workflow global-wf: added") {
		t.Errorf("expected global workflow sync line:\n%s", out)
	}
	list, err := runRoot(t, dir, "workflow", "list")
	if err != nil {
		t.Fatalf("workflow list: %v\n%s", err, list)
	}
	if strings.Contains(list, "feature-dev-generic") {
		t.Errorf("--skip-bootstrap should leave bootstrap workflow absent:\n%s", list)
	}
	if !strings.Contains(list, "global-wf") {
		t.Errorf("workflow list missing synced global workflow:\n%s", list)
	}
}

// TestInit_GlobalWorkflowSyncFailureNonFatal covers that a global
// workflow sync failure (e.g., uninstalled agent that can't be
// auto-installed) is a warning, not a fatal error.
func TestInit_GlobalWorkflowSyncFailureNonFatal(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	r := withIsolatedGlobalWorkflows(t)
	def := cliSyncDefinition("bad-global", "@noone/here", "bad global")
	if _, err := r.StoreDefinition(def, globalworkflow.StoreOptions{Revision: "rev-1"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out, err := runRoot(t, dir, "init")
	if err != nil {
		t.Fatalf("init should exit 0 on global workflow sync failure: %v\n%s", err, out)
	}
	if !strings.Contains(out, "warning") {
		t.Errorf("expected a 'warning' line on stderr:\n%s", out)
	}
	if !strings.Contains(out, "sync global workflows") {
		t.Errorf("expected the global-workflow-sync-flavour warning, got:\n%s", out)
	}
	if !strings.Contains(out, "--skip-global-workflows") {
		t.Errorf("global workflow sync warning should advertise --skip-global-workflows; got:\n%s", out)
	}
	if !strings.Contains(out, "initialized") {
		t.Errorf("init should still report 'initialized':\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".autosk", "db")); err != nil {
		t.Errorf(".autosk/db missing after failed sync: %v", err)
	}
}

// TestInit_SyncsGlobalWorkflows_PinsPackageVersion verifies that init
// passes the package-backed global's pinned version into agent
// auto-install during the best-effort global workflow sync.
func TestInit_SyncsGlobalWorkflows_PinsPackageVersion(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	withWorkflowNpm(t, npm)
	withIsolatedGlobalWorkflows(t)

	// Create a local package whose workflow references the package itself
	// as an agent, with a pinned version.
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/dev-fixture", "0.2.5",
		map[string]any{
			"workflows": []map[string]any{{"name": "versioned-init-wf", "file": "./workflow.json"}},
			"agent": map[string]any{
				"first_message": "You are the dev fixture.",
				"model":         "sonnet:high",
				"thinking":      "high",
			},
		},
		map[string]string{"workflow.json": `{
  "name": "versioned-init-wf",
  "first_step": "do",
  "steps": {
    "do": {
      "agent": { "name": "@autosk/dev-fixture" },
      "next_steps": [{ "task_status": "done", "prompt_rule": "Done." }]
    }
  }
}`})

	if _, err := runRoot(t, t.TempDir(), "workflow", "global", "install", pkg, "--workflow", "versioned-init-wf"); err != nil {
		t.Fatal(err)
	}

	// Reset install tracking so we only see init-time installs.
	npm.installs = nil

	// Run init in a fresh directory. It should sync the global workflow
	// and auto-install the agent using the pinned version.
	dir := t.TempDir()
	out, err := runRoot(t, dir, "init")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if !strings.Contains(out, "workflow versioned-init-wf: added") {
		t.Errorf("expected global workflow sync line:\n%s", out)
	}

	found := false
	for _, spec := range npm.installs {
		if spec == "@autosk/dev-fixture@0.2.5" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected npm install spec @autosk/dev-fixture@0.2.5, got %v", npm.installs)
	}
}

// TestInit_Quiet_SilencesNpm verifies that explicit `autosk init --quiet`
// does not leak npm stdout/stderr when bootstrapping the default workflow.
func TestInit_Quiet_SilencesNpm(t *testing.T) {
	withWorkflowNpm(t, loudWorkflowNpm{})
	dir := t.TempDir()
	out, err := runRoot(t, dir, "--quiet", "init")
	if err != nil {
		t.Fatalf("init --quiet: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("expected no stdout under --quiet, got:\n%q", out)
	}
}

// TestInit_GlobalWorkflowSyncFailureNonFatal_Quiet covers the quiet
// path: even though emitWorkflowSyncReport is suppressed, the warning
// on stderr must still carry actionable per-workflow failure details
// produced by renderWorkflowSyncError so operators can diagnose the
// issue without re-running.
func TestInit_GlobalWorkflowSyncFailureNonFatal_Quiet(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	r := withIsolatedGlobalWorkflows(t)
	def := cliSyncDefinition("bad-global", "@noone/here", "bad global")
	if _, err := r.StoreDefinition(def, globalworkflow.StoreOptions{Revision: "rev-1"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out, err := runRoot(t, dir, "--quiet", "init")
	if err != nil {
		t.Fatalf("init --quiet should exit 0 on global workflow sync failure: %v\n%s", err, out)
	}
	if !strings.Contains(out, "warning") {
		t.Errorf("expected a 'warning' line on stderr under --quiet:\n%q", out)
	}
	if !strings.Contains(out, "sync global workflows") {
		t.Errorf("expected the global-workflow-sync-flavour warning under --quiet, got:\n%q", out)
	}
	// The rendered error must include the workflow name and the failure
	// reason so the warning is actionable even without the report table.
	if !strings.Contains(out, "bad-global") {
		t.Errorf("quiet warning should name the failing workflow; got:\n%q", out)
	}
	if !strings.Contains(out, "--skip-global-workflows") {
		t.Errorf("quiet warning should advertise --skip-global-workflows; got:\n%q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".autosk", "db")); err != nil {
		t.Errorf(".autosk/db missing after failed sync under --quiet: %v", err)
	}
}

// lastLine returns the last non-empty trimmed line of s. It is the
// canonical id-extraction helper for tests that parse `autosk create`
// output (which may carry prefatory warning lines from
// auto-install / pkgregistry / bootstrap). Reused by `createBareTask`
// in enroll_test.go and by `createIDFromOutput` in
// worktree_e2e_test.go so the parsing rule lives in exactly one
// place across the cmd/autosk test suite.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		if t != "" {
			return t
		}
	}
	return ""
}
