package pkgregistry_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"autosk/internal/agent/pkgregistry"
)

// fakeNpm materialises packages on-disk directly, exactly like a real
// npm install would (the parts we care about: node_modules/<name>/
// directories with whatever the test wrote ahead).
type fakeNpm struct {
	// installs holds (name, spec) tuples in call order.
	installs   []string
	uninstalls []string

	// installFn lets a test write package.json on first install.
	installFn func(prefix, spec string) error
}

func (f *fakeNpm) Install(_ context.Context, prefix, spec string) error {
	f.installs = append(f.installs, spec)
	if f.installFn != nil {
		return f.installFn(prefix, spec)
	}
	return nil
}

func (f *fakeNpm) Uninstall(_ context.Context, prefix, name string) error {
	f.uninstalls = append(f.uninstalls, name)
	_ = os.RemoveAll(filepath.Join(prefix, "node_modules", filepath.FromSlash(name)))
	return nil
}

func writePackage(t *testing.T, prefix, name, version string, autoskAgent map[string]any, files map[string]string) {
	t.Helper()
	dir := filepath.Join(prefix, "node_modules", filepath.FromSlash(name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pj := map[string]any{
		"name":    name,
		"version": version,
	}
	if autoskAgent != nil {
		pj["autosk"] = map[string]any{"agent": autoskAgent}
	}
	body, _ := json.MarshalIndent(pj, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "package.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// ---- tests ---------------------------------------------------------------

func TestEnsurePrefix_CreatesSkeleton(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "packages")
	r, err := pkgregistry.Open(prefix)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.EnsurePrefix(); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"package.json", "registry.json"} {
		if _, err := os.Stat(filepath.Join(prefix, rel)); err != nil {
			t.Errorf("%s missing: %v", rel, err)
		}
	}
	// Idempotent.
	if err := r.EnsurePrefix(); err != nil {
		t.Fatalf("re-EnsurePrefix: %v", err)
	}
	entries, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("fresh registry should be empty, got %v", entries)
	}
}

func TestInstall_StandardAgent(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "packages")
	npm := &fakeNpm{}
	npm.installFn = func(prefix, spec string) error {
		writePackage(t, prefix, "@autosk/developer", "0.3.1",
			map[string]any{
				"model":         "sonnet:high",
				"thinking":      "high",
				"first_message": "You are the developer.",
				"extra_args":    []string{"--no-tool", "web_fetch"},
			}, nil)
		return nil
	}
	r, err := pkgregistry.Open(prefix, pkgregistry.WithNpm(npm))
	if err != nil {
		t.Fatal(err)
	}
	entry, err := r.Install(context.Background(), "@autosk/developer", "")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if entry.Version != "0.3.1" {
		t.Errorf("entry.Version=%q", entry.Version)
	}
	if !r.Has("@autosk/developer") {
		t.Fatal("Has=false after Install")
	}
	cfg, err := r.Resolve("@autosk/developer")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Runner != "" {
		t.Errorf("standard agent should have empty Runner, got %q", cfg.Runner)
	}
	if cfg.Model != "sonnet:high" || cfg.Thinking != "high" {
		t.Errorf("model/thinking: %+v", cfg)
	}
	if cfg.FirstMessage != "You are the developer." {
		t.Errorf("first_message: %q", cfg.FirstMessage)
	}
	if len(cfg.ExtraArgs) != 2 || cfg.ExtraArgs[0] != "--no-tool" {
		t.Errorf("extra_args: %v", cfg.ExtraArgs)
	}
}

func TestInstall_CustomRunnerAgent(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "packages")
	npm := &fakeNpm{}
	npm.installFn = func(prefix, spec string) error {
		writePackage(t, prefix, "@autosk/lint-fixer", "1.2.0",
			map[string]any{
				"runner": "./src/agent.ts",
			},
			map[string]string{"src/agent.ts": "export default async () => {};"})
		return nil
	}
	r, err := pkgregistry.Open(prefix, pkgregistry.WithNpm(npm))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Install(context.Background(), "@autosk/lint-fixer", "1.2.0"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	cfg, err := r.Resolve("@autosk/lint-fixer")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Runner == "" {
		t.Fatal("custom agent should have Runner set")
	}
	if !filepath.IsAbs(cfg.Runner) {
		t.Errorf("runner should be absolute: %s", cfg.Runner)
	}
}

func TestInstall_RollsBackRegistryOnValidationFailure(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "packages")
	npm := &fakeNpm{}
	npm.installFn = func(prefix, spec string) error {
		// Write a package without the autosk.agent block.
		writePackage(t, prefix, "@autosk/bad", "0.1.0", nil, nil)
		return nil
	}
	r, err := pkgregistry.Open(prefix, pkgregistry.WithNpm(npm))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Install(context.Background(), "@autosk/bad", "")
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !errors.Is(err, pkgregistry.ErrPackageMalformed) {
		t.Fatalf("want ErrPackageMalformed wrapped, got %v", err)
	}
	if r.Has("@autosk/bad") {
		t.Errorf("registry entry should have been rolled back")
	}
}

func TestResolve_UnknownAgentIsErrNotInstalled(t *testing.T) {
	dir := t.TempDir()
	r, err := pkgregistry.Open(filepath.Join(dir, "packages"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.EnsurePrefix(); err != nil {
		t.Fatal(err)
	}
	_, err = r.Resolve("@noone/here")
	if !errors.Is(err, pkgregistry.ErrNotInstalled) {
		t.Fatalf("want ErrNotInstalled, got %v", err)
	}
}

func TestResolve_HumanIsRejected(t *testing.T) {
	dir := t.TempDir()
	r, err := pkgregistry.Open(filepath.Join(dir, "packages"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.EnsurePrefix(); err != nil {
		t.Fatal(err)
	}
	_, err = r.Resolve("human")
	if !errors.Is(err, pkgregistry.ErrNotInstalled) {
		t.Fatalf("Resolve(human): want ErrNotInstalled, got %v", err)
	}
}

func TestResolve_FirstMessageFile(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "packages")
	npm := &fakeNpm{}
	npm.installFn = func(prefix, spec string) error {
		writePackage(t, prefix, "@autosk/with-prompt", "0.1.0",
			map[string]any{
				"first_message_file": "./prompts/first.md",
			},
			map[string]string{"prompts/first.md": "FILE PROMPT BODY\n"})
		return nil
	}
	r, _ := pkgregistry.Open(prefix, pkgregistry.WithNpm(npm))
	if _, err := r.Install(context.Background(), "@autosk/with-prompt", ""); err != nil {
		t.Fatalf("Install: %v", err)
	}
	cfg, err := r.Resolve("@autosk/with-prompt")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.FirstMessage != "FILE PROMPT BODY\n" {
		t.Errorf("first_message: %q", cfg.FirstMessage)
	}
}

func TestResolve_BothFirstMessageAndFileIsError(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "packages")
	npm := &fakeNpm{}
	npm.installFn = func(prefix, spec string) error {
		writePackage(t, prefix, "@autosk/conflict", "0.1.0",
			map[string]any{
				"first_message":      "inline",
				"first_message_file": "./prompts/first.md",
			},
			map[string]string{"prompts/first.md": "FILE"})
		return nil
	}
	r, _ := pkgregistry.Open(prefix, pkgregistry.WithNpm(npm))
	_, err := r.Install(context.Background(), "@autosk/conflict", "")
	if err == nil || !errors.Is(err, pkgregistry.ErrPackageMalformed) {
		t.Fatalf("want ErrPackageMalformed, got %v", err)
	}
}

func TestResolve_BadThinkingRejected(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "packages")
	npm := &fakeNpm{}
	npm.installFn = func(prefix, spec string) error {
		writePackage(t, prefix, "@autosk/badthink", "0.1.0",
			map[string]any{"thinking": "extreme"}, nil)
		return nil
	}
	r, _ := pkgregistry.Open(prefix, pkgregistry.WithNpm(npm))
	_, err := r.Install(context.Background(), "@autosk/badthink", "")
	if err == nil || !errors.Is(err, pkgregistry.ErrPackageMalformed) {
		t.Fatalf("want ErrPackageMalformed, got %v", err)
	}
}

func TestResolve_RunnerEscapeRejected(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "packages")
	npm := &fakeNpm{}
	npm.installFn = func(prefix, spec string) error {
		writePackage(t, prefix, "@autosk/escape", "0.1.0",
			map[string]any{"runner": "../../../etc/passwd"}, nil)
		return nil
	}
	r, _ := pkgregistry.Open(prefix, pkgregistry.WithNpm(npm))
	_, err := r.Install(context.Background(), "@autosk/escape", "")
	if err == nil || !errors.Is(err, pkgregistry.ErrPackageMalformed) {
		t.Fatalf("want ErrPackageMalformed, got %v", err)
	}
}

func TestInstallWorkflowSpec_WorkflowOnlyPackage(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "packages")
	npm := &fakeNpm{}
	npm.installFn = func(prefix, spec string) error {
		dir := filepath.Join(prefix, "node_modules", "@autosk", "workflow-only")
		if err := os.MkdirAll(filepath.Join(dir, "workflows"), 0o750); err != nil {
			return err
		}
		pkg := map[string]any{
			"name":    "@autosk/workflow-only",
			"version": "0.4.0",
			"autosk": map[string]any{"workflows": []any{map[string]any{
				"name": "feature-dev-extra",
				"file": "./workflows/feature-dev-extra.json",
			}}},
		}
		body, _ := json.MarshalIndent(pkg, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, "package.json"), body, 0o600); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "workflows", "feature-dev-extra.json"), []byte(`{"name":"feature-dev-extra"}`), 0o600)
	}
	r, _ := pkgregistry.Open(prefix, pkgregistry.WithNpm(npm))
	entry, err := r.InstallWorkflowSpec(context.Background(), "@autosk/workflow-only", "")
	if err != nil {
		t.Fatalf("InstallWorkflowSpec: %v", err)
	}
	if entry.Version != "0.4.0" {
		t.Fatalf("entry.Version=%q", entry.Version)
	}
	if r.Has("@autosk/workflow-only") {
		t.Fatal("workflow-only package must not satisfy agent resolver Has")
	}
	workflows, err := r.ResolveWorkflows("@autosk/workflow-only")
	if err != nil {
		t.Fatalf("ResolveWorkflows: %v", err)
	}
	if len(workflows) != 1 || workflows[0].Name != "feature-dev-extra" || !filepath.IsAbs(workflows[0].File) {
		t.Fatalf("unexpected workflows: %+v", workflows)
	}
}

func TestInstallWorkflowSpec_RejectsMissingWorkflowFile(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "packages")
	npm := &fakeNpm{}
	npm.installFn = func(prefix, spec string) error {
		dir := filepath.Join(prefix, "node_modules", "@autosk", "missing-workflow")
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
		pkg := map[string]any{
			"name":    "@autosk/missing-workflow",
			"version": "0.1.0",
			"autosk":  map[string]any{"workflows": []any{map[string]any{"name": "missing", "file": "./missing.json"}}},
		}
		body, _ := json.MarshalIndent(pkg, "", "  ")
		return os.WriteFile(filepath.Join(dir, "package.json"), body, 0o600)
	}
	r, _ := pkgregistry.Open(prefix, pkgregistry.WithNpm(npm))
	_, err := r.InstallWorkflowSpec(context.Background(), "@autosk/missing-workflow", "")
	if err == nil || !errors.Is(err, pkgregistry.ErrPackageMalformed) {
		t.Fatalf("want ErrPackageMalformed, got %v", err)
	}
	if _, gerr := r.Get("@autosk/missing-workflow"); !errors.Is(gerr, pkgregistry.ErrNotInstalled) {
		t.Fatalf("registry entry should be rolled back, got %v", gerr)
	}
}

func TestUninstall_RemovesRegistryEntry(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "packages")
	npm := &fakeNpm{}
	npm.installFn = func(prefix, spec string) error {
		writePackage(t, prefix, "@autosk/x", "0.1.0",
			map[string]any{"first_message": "x"}, nil)
		return nil
	}
	r, _ := pkgregistry.Open(prefix, pkgregistry.WithNpm(npm))
	if _, err := r.Install(context.Background(), "@autosk/x", ""); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := r.Uninstall(context.Background(), "@autosk/x"); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if r.Has("@autosk/x") {
		t.Errorf("entry should be gone")
	}
	// Idempotent.
	if err := r.Uninstall(context.Background(), "@autosk/x"); err != nil {
		t.Errorf("second uninstall should be noop, got %v", err)
	}
}

func TestEnsureRuntime_InstallsIfMissing(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "packages")
	npm := &fakeNpm{}
	npm.installFn = func(prefix, spec string) error {
		// Write a stub runtime so the second EnsureRuntime call is a no-op.
		runtimeDir := filepath.Join(prefix, "node_modules", "@autosk", "agent-runtime")
		if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(runtimeDir, "package.json"),
			[]byte(`{"name":"@autosk/agent-runtime","version":"0.1.0"}`), 0o644)
	}
	r, _ := pkgregistry.Open(prefix, pkgregistry.WithNpm(npm))
	if err := r.EnsureRuntime(context.Background(), ""); err != nil {
		t.Fatalf("EnsureRuntime: %v", err)
	}
	if err := r.EnsureRuntime(context.Background(), ""); err != nil {
		t.Fatalf("second EnsureRuntime: %v", err)
	}
	if len(npm.installs) != 1 {
		t.Errorf("expected exactly one install call, got %v", npm.installs)
	}
}

func TestList_SortedByName(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "packages")
	npm := &fakeNpm{}
	r, _ := pkgregistry.Open(prefix, pkgregistry.WithNpm(npm))
	npm.installFn = func(prefix, spec string) error {
		name := spec
		// Strip an optional @version suffix (after the last '@').
		// We can't naïvely split on '@' because the name itself may start with '@'.
		if i := lastAt(spec); i > 0 {
			name = spec[:i]
		}
		writePackage(t, prefix, name, "0.0.1",
			map[string]any{"first_message": "x"}, nil)
		return nil
	}
	for _, n := range []string{"@b/x", "@a/x", "z"} {
		if _, err := r.Install(context.Background(), n, ""); err != nil {
			t.Fatalf("install %s: %v", n, err)
		}
	}
	entries, err := r.List()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"@a/x", "@b/x", "z"}
	if len(entries) != len(want) {
		t.Fatalf("len=%d want %d", len(entries), len(want))
	}
	for i, w := range want {
		if entries[i].Name != w {
			t.Errorf("idx %d: got %s, want %s", i, entries[i].Name, w)
		}
	}
}

func lastAt(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '@' {
			return i
		}
	}
	return -1
}
