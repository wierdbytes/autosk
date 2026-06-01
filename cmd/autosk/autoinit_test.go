package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosk/internal/globalworkflow"
)

// withInteractiveStdin swaps the package-level TTY check + stdin
// reader for the duration of the test so a write verb running in a
// fresh dir takes the prompting code path. The supplied `answer` is
// piped verbatim into the confirmation prompt; multiple answers may
// be supplied by joining them with "\n" (used for the re-prompt
// regression below).
func withInteractiveStdin(t *testing.T, answer string) {
	t.Helper()
	prevIsTTY := isInteractiveFn
	prevReader := confirmReader
	isInteractiveFn = func() bool { return true }
	confirmReader = strings.NewReader(answer)
	t.Cleanup(func() {
		isInteractiveFn = prevIsTTY
		confirmReader = prevReader
	})
}

// TestAutoInit_NonTTYBootstrapsByDefault covers the requirement that
// an implicit auto-init from a write verb (non-TTY path) seeds the
// default workflow exactly like `autosk init` does. The runRoot
// helper sets AUTOSK_AUTOINIT_SKIP_BOOTSTRAP=1 by default; this test
// clears it to verify the bootstrap-on-auto-init contract end-to-end.
func TestAutoInit_NonTTYBootstrapsByDefault(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	t.Setenv("AUTOSK_AUTOINIT_SKIP_BOOTSTRAP", "")
	dir := t.TempDir()

	// A fresh dir + a write verb should auto-create the DB AND seed
	// the workflow without an explicit `autosk init`. We use
	// `workflow list` to verify the seed because `autosk create`
	// against a missing workflow would also work but couples the
	// assertion to create-cmd output formatting.
	out, err := runRoot(t, dir, "create", "smoke")
	if err != nil {
		t.Fatalf("create on fresh dir: %v\n%s", err, out)
	}
	if !strings.Contains(out, "autosk: created") {
		t.Errorf("expected 'autosk: created' notice on stderr:\n%s", out)
	}
	if !strings.Contains(out, "bootstrapped workflow feature-dev-generic") {
		t.Errorf("expected auto-init to bootstrap feature-dev-generic, got:\n%s", out)
	}

	list, err := runRoot(t, dir, "workflow", "list")
	if err != nil {
		t.Fatalf("workflow list: %v\n%s", err, list)
	}
	if !strings.Contains(list, "feature-dev-generic") {
		t.Errorf("workflow list missing feature-dev-generic after auto-init:\n%s", list)
	}

	// A second write verb is a normal hit — no second "autosk: created"
	// and no duplicate bootstrap line.
	out2, err := runRoot(t, dir, "create", "smoke-2")
	if err != nil {
		t.Fatalf("second create: %v\n%s", err, out2)
	}
	if strings.Contains(out2, "autosk: created") {
		t.Errorf("second create should not re-create the DB:\n%s", out2)
	}
	if strings.Contains(out2, "bootstrapped workflow") {
		t.Errorf("second create should not re-bootstrap:\n%s", out2)
	}
}

// TestAutoInit_SkipBootstrapEnv covers the AUTOSK_AUTOINIT_SKIP_BOOTSTRAP
// opt-out: a write verb on a fresh dir still creates the DB but leaves
// the workflow table empty. This is the contract the runRoot test
// helper relies on (it sets the env by default).
func TestAutoInit_SkipBootstrapEnv(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	t.Setenv("AUTOSK_AUTOINIT_SKIP_BOOTSTRAP", "1")
	dir := t.TempDir()

	if _, err := runRoot(t, dir, "create", "smoke"); err != nil {
		t.Fatalf("create on fresh dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".autosk", "db")); err != nil {
		t.Errorf(".autosk/db not created: %v", err)
	}
	list, _ := runRoot(t, dir, "workflow", "list")
	if strings.Contains(list, "feature-dev-generic") {
		t.Errorf("AUTOSK_AUTOINIT_SKIP_BOOTSTRAP should leave workflow empty:\n%s", list)
	}
}

// TestAutoInit_TTYPromptYesBootstraps simulates the interactive
// happy-path: stdin/stderr report as TTY, user answers "y", the DB
// gets created and feature-dev-generic is seeded.
func TestAutoInit_TTYPromptYesBootstraps(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	t.Setenv("AUTOSK_AUTOINIT_SKIP_BOOTSTRAP", "")
	withInteractiveStdin(t, "y\n")
	dir := t.TempDir()

	out, err := runRoot(t, dir, "create", "smoke")
	if err != nil {
		t.Fatalf("create after y: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Create a new autosk database") {
		t.Errorf("expected interactive prompt on stderr:\n%s", out)
	}
	if !strings.Contains(out, "bootstrapped workflow feature-dev-generic") {
		t.Errorf("expected bootstrap after 'y':\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".autosk", "db")); err != nil {
		t.Errorf(".autosk/db not created after 'y': %v", err)
	}
}

// TestAutoInit_TTYPromptDefaultYes covers the documented default: an
// empty answer (user pressed Enter) is treated as "y".
func TestAutoInit_TTYPromptDefaultYes(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	t.Setenv("AUTOSK_AUTOINIT_SKIP_BOOTSTRAP", "")
	withInteractiveStdin(t, "\n")
	dir := t.TempDir()

	out, err := runRoot(t, dir, "create", "smoke")
	if err != nil {
		t.Fatalf("create after empty answer: %v\n%s", err, out)
	}
	if !strings.Contains(out, "bootstrapped workflow feature-dev-generic") {
		t.Errorf("empty answer should default to yes and bootstrap:\n%s", out)
	}
}

// TestAutoInit_TTYPromptNoRefuses covers the decline path: user
// answers "n", no DB is created, the command exits with a clear
// error pointing at --db / AUTOSK_DB / `autosk init`.
func TestAutoInit_TTYPromptNoRefuses(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	t.Setenv("AUTOSK_AUTOINIT_SKIP_BOOTSTRAP", "")
	withInteractiveStdin(t, "n\n")
	dir := t.TempDir()

	out, err := runRoot(t, dir, "create", "smoke")
	if err == nil {
		t.Fatalf("expected error after declining prompt, got nil; output:\n%s", out)
	}
	combined := out + err.Error()
	if !strings.Contains(combined, "declined to create one") {
		t.Errorf("expected 'declined to create one' error, got: err=%v\nout=%s", err, out)
	}
	if !strings.Contains(combined, "autosk init") {
		t.Errorf("expected error to mention `autosk init`:\nerr=%v\nout=%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".autosk")); !os.IsNotExist(err) {
		t.Errorf(".autosk directory should NOT exist after decline; stat err=%v", err)
	}
}

// TestAutoInit_TTYPromptReprompt covers the "answer not recognised"
// loop: a junk first answer triggers a "please answer y or n" hint,
// then the real "y" goes through.
func TestAutoInit_TTYPromptReprompt(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	t.Setenv("AUTOSK_AUTOINIT_SKIP_BOOTSTRAP", "")
	withInteractiveStdin(t, "huh\ny\n")
	dir := t.TempDir()

	out, err := runRoot(t, dir, "create", "smoke")
	if err != nil {
		t.Fatalf("create after re-prompt: %v\n%s", err, out)
	}
	if !strings.Contains(out, "please answer 'y' or 'n'") {
		t.Errorf("expected re-prompt hint on stderr:\n%s", out)
	}
	if !strings.Contains(out, "bootstrapped workflow feature-dev-generic") {
		t.Errorf("re-prompt then 'y' should still bootstrap:\n%s", out)
	}
}

// TestAutoInit_JSONSuppressesPrompt covers the --json / --quiet rule:
// even with a TTY attached, machine-readable / terse modes skip the
// prompt and fall through to silent auto-init+bootstrap.
func TestAutoInit_JSONSuppressesPrompt(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	t.Setenv("AUTOSK_AUTOINIT_SKIP_BOOTSTRAP", "")
	// Pretend stdin/stderr are real TTYs but supply NO answer: if
	// the prompt fired we'd block on the empty reader; instead the
	// --json branch should bypass the prompt entirely.
	prevIsTTY := isInteractiveFn
	isInteractiveFn = func() bool { return true }
	t.Cleanup(func() { isInteractiveFn = prevIsTTY })
	prevReader := confirmReader
	confirmReader = blockingReader{}
	t.Cleanup(func() { confirmReader = prevReader })

	dir := t.TempDir()
	out, err := runRoot(t, dir, "--json", "create", "smoke")
	if err != nil {
		t.Fatalf("create --json on fresh dir: %v\n%s", err, out)
	}
	if strings.Contains(out, "Create a new autosk database") {
		t.Errorf("--json should not surface the interactive prompt:\n%s", out)
	}
	if !strings.Contains(out, "bootstrapped workflow feature-dev-generic") {
		t.Errorf("--json auto-init should still bootstrap:\n%s", out)
	}
}

// TestAutoInit_AssumeYesEnv covers AUTOSK_AUTOINIT_ASSUME_YES: it
// suppresses the prompt under a TTY and proceeds as if the user said
// "y".
func TestAutoInit_AssumeYesEnv(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	t.Setenv("AUTOSK_AUTOINIT_SKIP_BOOTSTRAP", "")
	t.Setenv("AUTOSK_AUTOINIT_ASSUME_YES", "1")
	prevIsTTY := isInteractiveFn
	isInteractiveFn = func() bool { return true }
	t.Cleanup(func() { isInteractiveFn = prevIsTTY })
	prevReader := confirmReader
	confirmReader = blockingReader{}
	t.Cleanup(func() { confirmReader = prevReader })

	dir := t.TempDir()
	out, err := runRoot(t, dir, "create", "smoke")
	if err != nil {
		t.Fatalf("create with assume-yes: %v\n%s", err, out)
	}
	if strings.Contains(out, "Create a new autosk database") {
		t.Errorf("AUTOSK_AUTOINIT_ASSUME_YES should skip the prompt:\n%s", out)
	}
	if !strings.Contains(out, "bootstrapped workflow feature-dev-generic") {
		t.Errorf("assume-yes path should bootstrap:\n%s", out)
	}
}

// TestAutoInit_ExistingDBNoPrompt covers the trivial case: when a DB
// is already discoverable on disk, openStore must not prompt even if
// the TTY hooks are stubbed to interactive.
func TestAutoInit_ExistingDBNoPrompt(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatalf("init: %v", err)
	}
	prevIsTTY := isInteractiveFn
	isInteractiveFn = func() bool { return true }
	t.Cleanup(func() { isInteractiveFn = prevIsTTY })
	prevReader := confirmReader
	confirmReader = blockingReader{}
	t.Cleanup(func() { confirmReader = prevReader })

	out, err := runRoot(t, dir, "create", "smoke")
	if err != nil {
		t.Fatalf("create against existing DB: %v\n%s", err, out)
	}
	if strings.Contains(out, "Create a new autosk database") {
		t.Errorf("existing DB must not trigger the prompt:\n%s", out)
	}
}

// TestAutoInit_SyncsGlobalWorkflows covers that auto-init from a write
// verb syncs enabled global workflows after bootstrap.
func TestAutoInit_SyncsGlobalWorkflows(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	r := withIsolatedGlobalWorkflows(t)
	def := cliSyncDefinition("global-wf", "@autosk/dev-fixture", "global workflow")
	if _, err := r.StoreDefinition(def, globalworkflow.StoreOptions{Revision: "rev-1"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSK_AUTOINIT_SKIP_BOOTSTRAP", "")
	dir := t.TempDir()

	out, err := runRoot(t, dir, "create", "smoke")
	if err != nil {
		t.Fatalf("create on fresh dir: %v\n%s", err, out)
	}
	if !strings.Contains(out, "bootstrapped workflow feature-dev-generic") {
		t.Errorf("expected auto-init to bootstrap feature-dev-generic, got:\n%s", out)
	}
	if !strings.Contains(out, "workflow global-wf: added") {
		t.Errorf("expected global workflow sync line:\n%s", out)
	}
	list, err := runRoot(t, dir, "workflow", "list")
	if err != nil {
		t.Fatalf("workflow list: %v\n%s", err, list)
	}
	if !strings.Contains(list, "global-wf") {
		t.Errorf("workflow list missing synced global workflow:\n%s", list)
	}
}

// TestAutoInit_SkipGlobalWorkflowsEnv covers the
// AUTOSK_AUTOINIT_SKIP_GLOBAL_WORKFLOWS opt-out: a write verb on a
// fresh dir still creates the DB and bootstraps, but leaves global
// workflows unsynced.
func TestAutoInit_SkipGlobalWorkflowsEnv(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	r := withIsolatedGlobalWorkflows(t)
	def := cliSyncDefinition("global-wf", "@autosk/dev-fixture", "global workflow")
	if _, err := r.StoreDefinition(def, globalworkflow.StoreOptions{Revision: "rev-1"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSK_AUTOINIT_SKIP_BOOTSTRAP", "")
	t.Setenv("AUTOSK_AUTOINIT_SKIP_GLOBAL_WORKFLOWS", "1")
	dir := t.TempDir()

	out, err := runRoot(t, dir, "create", "smoke")
	if err != nil {
		t.Fatalf("create on fresh dir: %v\n%s", err, out)
	}
	if !strings.Contains(out, "bootstrapped workflow feature-dev-generic") {
		t.Errorf("expected auto-init to bootstrap feature-dev-generic, got:\n%s", out)
	}
	if strings.Contains(out, "workflow global-wf: added") {
		t.Errorf("AUTOSK_AUTOINIT_SKIP_GLOBAL_WORKFLOWS should not sync global workflows:\n%s", out)
	}
	list, err := runRoot(t, dir, "workflow", "list")
	if err != nil {
		t.Fatalf("workflow list: %v\n%s", err, list)
	}
	if strings.Contains(list, "global-wf") {
		t.Errorf("AUTOSK_AUTOINIT_SKIP_GLOBAL_WORKFLOWS should leave global workflow absent:\n%s", list)
	}
}

// TestAutoInit_GlobalWorkflowSyncFailureNonFatal covers that a global
// workflow sync failure during auto-init is a warning, not a fatal
// error: the write command still succeeds, the DB is created, and the
// warning mentions the AUTOSK_AUTOINIT_SKIP_GLOBAL_WORKFLOWS opt-out.
func TestAutoInit_GlobalWorkflowSyncFailureNonFatal(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	r := withIsolatedGlobalWorkflows(t)
	def := cliSyncDefinition("bad-global", "@noone/here", "bad global")
	if _, err := r.StoreDefinition(def, globalworkflow.StoreOptions{Revision: "rev-1"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSK_AUTOINIT_SKIP_BOOTSTRAP", "")
	dir := t.TempDir()

	out, err := runRoot(t, dir, "create", "smoke")
	if err != nil {
		t.Fatalf("create on fresh dir should succeed despite sync failure: %v\n%s", err, out)
	}
	if !strings.Contains(out, "warning") {
		t.Errorf("expected a 'warning' line on stderr:\n%s", out)
	}
	if !strings.Contains(out, "sync global workflows") {
		t.Errorf("expected the global-workflow-sync-flavour warning, got:\n%s", out)
	}
	if !strings.Contains(out, "AUTOSK_AUTOINIT_SKIP_GLOBAL_WORKFLOWS") {
		t.Errorf("warning should advertise AUTOSK_AUTOINIT_SKIP_GLOBAL_WORKFLOWS opt-out; got:\n%s", out)
	}
	if !strings.Contains(out, "bootstrapped workflow feature-dev-generic") {
		t.Errorf("expected auto-init to still bootstrap feature-dev-generic, got:\n%s", out)
	}
	if _, serr := os.Stat(filepath.Join(dir, ".autosk", "db")); serr != nil {
		t.Errorf(".autosk/db not created after auto-init with sync failure: %v", serr)
	}
	// Verify the task was actually created.
	list, err := runRoot(t, dir, "list")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, list)
	}
	if !strings.Contains(list, "smoke") {
		t.Errorf("task 'smoke' should exist after auto-init:\n%s", list)
	}
}

// TestAutoInit_JSONCreatesSingleJSONDocument is a regression test for
// the auto-init path when the outer command runs with --json.
// syncGlobalWorkflows must not emit its own JSON report to stdout,
// because that would produce two concatenated JSON documents and break
// consumers expecting a single task JSON object.
func TestAutoInit_JSONCreatesSingleJSONDocument(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	r := withIsolatedGlobalWorkflows(t)
	def := cliSyncDefinition("global-wf", "@autosk/dev-fixture", "global workflow")
	if _, err := r.StoreDefinition(def, globalworkflow.StoreOptions{Revision: "rev-1"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSK_AUTOINIT_SKIP_BOOTSTRAP", "")
	dir := t.TempDir()

	out, err := runRoot(t, dir, "--json", "create", "smoke")
	if err != nil {
		t.Fatalf("--json create on fresh dir: %v\n%s", err, out)
	}
	// Count JSON-object-looking lines. There must be exactly one
	// (the task JSON from create --json). A regression where
	// emitWorkflowSyncReport also wrote to stdout would add a second.
	var jsonLines []string
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
			jsonLines = append(jsonLines, trimmed)
		}
	}
	if len(jsonLines) != 1 {
		t.Fatalf("expected exactly one JSON object in output, got %d:\n%s", len(jsonLines), out)
	}
	var task map[string]any
	if err := json.Unmarshal([]byte(jsonLines[0]), &task); err != nil {
		t.Fatalf("task JSON is invalid: %v\nraw:\n%s", err, jsonLines[0])
	}
	if task["title"] != "smoke" {
		t.Errorf("expected task title 'smoke', got %v", task["title"])
	}
}

// blockingReader is used by tests that want to prove no read happens.
// Calling Read would block forever on a real terminal; here we panic
// so a regression is loud rather than mysterious.
type blockingReader struct{}

func (blockingReader) Read(_ []byte) (int, error) {
	panic("autoinit prompt read should not have happened in this test")
}

// Compile-time assertion: blockingReader implements io.Reader.
var _ io.Reader = blockingReader{}
