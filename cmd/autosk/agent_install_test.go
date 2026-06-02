package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosk/internal/agent/pkgregistry"
)

// fakeNpmInProcess writes the on-disk shape that a real `npm install`
// would produce. Tests inject it via the AUTOSK_PACKAGES env so the CLI
// hits an isolated prefix.
//
// The fixtures it provides:
//
//	@autosk/dev-fixture          — a "standard" agent (no runner)
//	@autosk/custom-fixture       — declares a runner ./agent.ts
//	@autosk/agent-runtime        — runtime stub
//	@autogent/generic            — the bootstrap agent seeded by
//	                               `autosk init` (mirrors the
//	                               on-disk shape of agents/generic-agent)
//
// Anything else passed to Install is rejected so tests fail loudly on
// drift.
type fakeNpmInProcess struct{}

func (fakeNpmInProcess) Install(_ context.Context, prefix, spec string) error {
	if pkgName, ok := localPackageName(spec); ok {
		dir := filepath.Join(prefix, "node_modules", filepath.FromSlash(pkgName))
		_ = os.RemoveAll(dir)
		return copyDir(spec, dir)
	}
	// Strip @version if present.
	name := spec
	if i := strings.LastIndex(spec, "@"); i > 0 {
		name = spec[:i]
	}
	dir := filepath.Join(prefix, "node_modules", filepath.FromSlash(name))

	// If the package was previously installed (e.g. from a local path),
	// preserve its on-disk shape and bump the version so update tests
	// can detect a change.
	existingPJ := filepath.Join(dir, "package.json")
	if b, err := os.ReadFile(filepath.Clean(existingPJ)); err == nil {
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			m["version"] = "9.9.9"
			b, _ = json.MarshalIndent(m, "", "  ")
			return os.WriteFile(existingPJ, b, 0o600)
		}
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var pj map[string]any
	switch name {
	case pkgregistry.RuntimePackageName:
		pj = map[string]any{"name": name, "version": "0.1.0"}
	case "@autosk/dev-fixture":
		pj = map[string]any{
			"name":    name,
			"version": "0.2.5",
			"autosk": map[string]any{"agent": map[string]any{
				"first_message": "You are the dev fixture.",
				"model":         "sonnet:high",
				"thinking":      "high",
			}},
		}
	case "@autogent/generic":
		// Hermetic stand-in for the real agents/generic-agent package
		// so the bootstrap workflow seeded by `autosk init` can be
		// installed offline.
		pj = map[string]any{
			"name":    name,
			"version": "0.1.0",
			"autosk":  map[string]any{"agent": map[string]any{}},
		}
	case "@autosk/custom-fixture":
		pj = map[string]any{
			"name":    name,
			"version": "1.0.0",
			"autosk":  map[string]any{"agent": map[string]any{"runner": "./agent.ts"}},
		}
		if err := os.WriteFile(filepath.Join(dir, "agent.ts"), []byte("export default async () => {};"), 0o644); err != nil {
			return err
		}
	default:
		return os.ErrNotExist
	}
	body, _ := json.MarshalIndent(pj, "", "  ")
	return os.WriteFile(filepath.Join(dir, "package.json"), body, 0o644)
}

func (fakeNpmInProcess) Uninstall(_ context.Context, prefix, name string) error {
	return os.RemoveAll(filepath.Join(prefix, "node_modules", filepath.FromSlash(name)))
}

func localPackageName(spec string) (string, bool) {
	if spec == "" || !filepath.IsAbs(spec) {
		return "", false
	}
	name, err := pkgregistry.ReadPackageNameFromPath(spec)
	if err != nil {
		return "", false
	}
	return name, true
}

func copyDir(src, dst string) error {
	srcRoot, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	return filepath.Walk(srcRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o750)
		}
		if strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
			return os.ErrPermission
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		body, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		return os.WriteFile(target, body, info.Mode())
	})
}

// withIsolatedPackagesPrefix creates a fresh prefix and points
// pkgregistry.Default() at it via $AUTOSK_PACKAGES. The fake npm runner
// is wired into the file-level pkgregistryNpmFactory hook (see below).
// Returns the prefix path.
func withIsolatedPackagesPrefix(t *testing.T) string {
	t.Helper()
	prefix := filepath.Join(t.TempDir(), "packages")
	t.Setenv("AUTOSK_PACKAGES", prefix)
	prev := pkgregistryNpmFactory
	pkgregistryNpmFactory = func() pkgregistry.NpmRunner { return fakeNpmInProcess{} }
	t.Cleanup(func() { pkgregistryNpmFactory = prev })
	return prefix
}

// runRoot executes the CLI's root cobra command in-process and captures
// stdout + stderr. Run inside the supplied directory.
//
// Env-isolation: this helper unsets AUTOSK_DB and AUTOSK_NO_AUTOINIT for
// the duration of the test. Without this, tests running inside a
// worktree-isolated agent's subprocess (which the executor spawns with
// AUTOSK_DB=<projectRoot>/.autosk/db so the agent's `autosk` CLI calls
// find the canonical DB) would inherit that env via os.Environ() and
// every `runRoot` invocation would write tasks/workflows into the
// project's DB instead of the per-test t.TempDir(). Same hazard if a
// developer's shell has AUTOSK_DB set. t.Setenv restores the prior
// state on test cleanup.
//
// Bootstrap isolation: openStore now also seeds feature-dev-generic on
// the auto-init path (mirroring `autosk init`). Tests that exercise a
// write verb in a fresh dir without first calling `autosk init` would
// otherwise have that bootstrap fire and either pollute their assertions
// or try to reach the real npm registry. We default the runRoot env to
// AUTOSK_AUTOINIT_SKIP_BOOTSTRAP=1; tests that explicitly want to verify
// the auto-init bootstrap path opt back in by clearing the env via
// t.Setenv before calling runRoot. The `autosk init` verb itself ignores
// this env (it has its own --skip-bootstrap flag), so init-driven
// bootstrap tests stay unaffected.
func runRoot(t *testing.T, dir string, argv ...string) (string, error) {
	t.Helper()
	t.Setenv("AUTOSK_DB", "")
	t.Setenv("AUTOSK_NO_AUTOINIT", "")
	if _, set := os.LookupEnv("AUTOSK_AUTOINIT_SKIP_BOOTSTRAP"); !set {
		t.Setenv("AUTOSK_AUTOINIT_SKIP_BOOTSTRAP", "1")
	}
	if _, set := os.LookupEnv("AUTOSK_WORKFLOWS"); !set {
		t.Setenv("AUTOSK_WORKFLOWS", filepath.Join(t.TempDir(), "workflows"))
	}
	root := newRootCmd()
	root.SetArgs(argv)
	// emit* helpers write to os.Stdout directly; capture via pipe.
	origStdout := os.Stdout
	origStderr := os.Stderr
	rPipe, wPipe, _ := os.Pipe()
	os.Stdout = wPipe
	os.Stderr = wPipe
	root.SetOut(wPipe)
	root.SetErr(wPipe)

	cwd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		os.Stdout = origStdout
		os.Stderr = origStderr
		t.Fatalf("chdir %s: %v", dir, err)
	}
	defer func() {
		_ = os.Chdir(cwd)
		os.Stdout = origStdout
		os.Stderr = origStderr
	}()

	var out bytes.Buffer
	doneCh := make(chan struct{})
	go func() {
		_, _ = out.ReadFrom(rPipe)
		close(doneCh)
	}()
	err := root.Execute()
	_ = wPipe.Close()
	<-doneCh
	return out.String(), err
}

func TestAgentInstall_StandardCreatesDBRow(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatalf("init: %v", err)
	}
	stdout, err := runRoot(t, dir, "agent", "install", "@autosk/dev-fixture")
	if err != nil {
		t.Fatalf("install: %v\noutput=%s", err, stdout)
	}
	if !strings.Contains(stdout, "installed @autosk/dev-fixture@0.2.5") {
		t.Errorf("unexpected output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "kind:") || !strings.Contains(stdout, "standard") {
		t.Errorf("missing 'kind: standard' in output:\n%s", stdout)
	}
	// list should show the agent in DB + package source.
	list, err := runRoot(t, dir, "agent", "list")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(list, "@autosk/dev-fixture") || !strings.Contains(list, "package") || !strings.Contains(list, "0.2.5") {
		t.Errorf("list output missing fixture:\n%s", list)
	}
	if !strings.Contains(list, "human") || !strings.Contains(list, "builtin") {
		t.Errorf("list output missing human/builtin row:\n%s", list)
	}
}

func TestAgentInstall_CustomRunnerSurfaceKind(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	stdout, err := runRoot(t, dir, "agent", "install", "@autosk/custom-fixture")
	if err != nil {
		t.Fatalf("install: %v\n%s", err, stdout)
	}
	if !strings.Contains(stdout, "custom runner") {
		t.Errorf("expected 'custom runner' in output:\n%s", stdout)
	}
}

func TestAgentShow_UnionView(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}
	stdout, err := runRoot(t, dir, "agent", "show", "@autosk/dev-fixture")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, stdout)
	}
	for _, want := range []string{"name:", "agent_id:", "version:", "0.2.5", "install:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("show output missing %q:\n%s", want, stdout)
		}
	}
}

func TestAgentUninstall_RefusesWhenReferenced(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "agent", "install", "@autosk/dev-fixture"); err != nil {
		t.Fatal(err)
	}
	// Create a task in the synthetic single workflow → adds a steps row
	// that references the agent.
	if _, err := runRoot(t, dir, "create", "Do the thing", "--agent", "@autosk/dev-fixture"); err != nil {
		t.Fatalf("create --agent: %v", err)
	}

	out, err := runRoot(t, dir, "agent", "uninstall", "@autosk/dev-fixture")
	if err == nil {
		t.Fatalf("expected refusal, got success:\n%s", out)
	}
	if !strings.Contains(err.Error(), "referenced by") {
		t.Errorf("error should explain the refusal: %v", err)
	}

	// --force should succeed.
	out, err = runRoot(t, dir, "agent", "uninstall", "@autosk/dev-fixture", "--force")
	if err != nil {
		t.Fatalf("--force should succeed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "uninstalled") {
		t.Errorf("missing 'uninstalled' in output:\n%s", out)
	}
}

func TestAgentInstall_RejectsBadPackageName(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	_, err := runRoot(t, dir, "agent", "install", "human")
	if err == nil {
		t.Fatal("install human should be rejected")
	}
}

func TestCreate_RejectsUninstalledAgent(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	_, err := runRoot(t, dir, "create", "bad", "--agent", "@noone/here")
	if err == nil {
		t.Fatal("expected agent_not_installed rejection")
	}
	if !strings.Contains(err.Error(), "agent_not_installed") {
		t.Errorf("wrong error: %v", err)
	}
}
