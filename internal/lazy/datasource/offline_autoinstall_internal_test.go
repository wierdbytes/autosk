package datasource

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosk/internal/agent"
	"autosk/internal/agent/pkgregistry"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
)

// loudNpmRunner writes to stdout/stderr on every install, simulating
// npm progress output that would corrupt a gocui TUI.
type loudNpmRunner struct{}

func (loudNpmRunner) Install(_ context.Context, prefix, spec string) error {
	_, _ = fmt.Fprintf(os.Stdout, "npm progress for %s\n", spec)
	_, _ = fmt.Fprintf(os.Stderr, "npm stderr for %s\n", spec)

	// Strip @version if present.
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
		"autosk": map[string]any{
			"agent": map[string]any{"first_message": "hello"},
		},
	}
	b, _ := json.MarshalIndent(pj, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "package.json"), b, 0o600); err != nil {
		return err
	}
	return nil
}

func (loudNpmRunner) Uninstall(_ context.Context, prefix, name string) error {
	return os.RemoveAll(filepath.Join(prefix, "node_modules", filepath.FromSlash(name)))
}

// failingLoudNpmRunner writes diagnostics to stderr and returns an error,
// simulating an npm install failure where stderr carries actionable info.
type failingLoudNpmRunner struct{}

func (failingLoudNpmRunner) Install(_ context.Context, prefix, spec string) error {
	_, _ = fmt.Fprintf(os.Stderr, "E404 registry not found for %s\n", spec)
	_, _ = fmt.Fprintf(os.Stderr, "npm ERR! code E404\n")
	return fmt.Errorf("npm install failed for %s", spec)
}

func (failingLoudNpmRunner) Uninstall(_ context.Context, prefix, name string) error {
	return nil
}

func TestAutoInstallMissingAgents_SilencesNpmOutput(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ts := doltlite.New()
	if err := ts.Open(ctx, filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := ts.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer func() { _ = ts.Close() }()

	// Seed human agent.
	ag := agent.New(ts.DB())
	if _, err := ag.EnsureByName(ctx, "human"); err != nil {
		t.Fatalf("ensure human: %v", err)
	}

	// Create a registry with the loud runner.
	prefix := filepath.Join(dir, "packages")
	reg, err := pkgregistry.Open(prefix, pkgregistry.WithNpm(loudNpmRunner{}))
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}

	// Workflow that references a scoped agent not yet in the DB.
	def := workflow.Definition{
		Name:      "wf-with-agent",
		FirstStep: "do",
		Steps: map[string]workflow.StepDef{
			"do": {
				AgentName: "@scope/agent",
				NextSteps: []workflow.TransitionDef{{TaskStatus: "done", PromptRule: "Done."}},
			},
		},
	}

	// Capture stdout/stderr.
	origOut, origErr := os.Stdout, os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout = wOut
	os.Stderr = wErr

	_, aerr := autoInstallMissingAgents(ctx, def, ag, ts, reg, nil)

	_ = wOut.Close()
	_ = wErr.Close()
	os.Stdout = origOut
	os.Stderr = origErr

	outBytes, _ := io.ReadAll(rOut)
	errBytes, _ := io.ReadAll(rErr)

	if aerr != nil {
		t.Fatalf("autoInstallMissingAgents: %v", aerr)
	}

	if len(outBytes) != 0 {
		t.Fatalf("stdout leaked npm output: %q", string(outBytes))
	}
	if len(errBytes) != 0 {
		t.Fatalf("stderr leaked npm output: %q", string(errBytes))
	}
}

func TestAutoInstallMissingAgents_CapturesDiagnostics(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ts := doltlite.New()
	if err := ts.Open(ctx, filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := ts.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer func() { _ = ts.Close() }()

	ag := agent.New(ts.DB())
	if _, err := ag.EnsureByName(ctx, "human"); err != nil {
		t.Fatalf("ensure human: %v", err)
	}

	prefix := filepath.Join(dir, "packages")
	reg, err := pkgregistry.Open(prefix, pkgregistry.WithNpm(failingLoudNpmRunner{}))
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}

	def := workflow.Definition{
		Name:      "wf-with-agent",
		FirstStep: "do",
		Steps: map[string]workflow.StepDef{
			"do": {
				AgentName: "@scope/failing",
				NextSteps: []workflow.TransitionDef{{TaskStatus: "done", PromptRule: "Done."}},
			},
		},
	}

	// Capture actual terminal stdout/stderr.
	origOut, origErr := os.Stdout, os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout = wOut
	os.Stderr = wErr

	_, aerr := autoInstallMissingAgents(ctx, def, ag, ts, reg, nil)

	_ = wOut.Close()
	_ = wErr.Close()
	os.Stdout = origOut
	os.Stderr = origErr

	outBytes, _ := io.ReadAll(rOut)
	errBytes, _ := io.ReadAll(rErr)

	if aerr == nil {
		t.Fatal("expected autoInstallMissingAgents to fail")
	}

	// The error should include the captured stderr diagnostics.
	errStr := aerr.Error()
	if !strings.Contains(errStr, "E404 registry not found") {
		t.Fatalf("error missing captured stderr diagnostics: %q", errStr)
	}
	if !strings.Contains(errStr, "npm ERR! code E404") {
		t.Fatalf("error missing npm error code: %q", errStr)
	}

	// Terminal stdout/stderr must remain clean (no TUI corruption).
	if len(outBytes) != 0 {
		t.Fatalf("stdout leaked npm output: %q", string(outBytes))
	}
	if len(errBytes) != 0 {
		t.Fatalf("stderr leaked npm output: %q", string(errBytes))
	}
}

// verboseNpmRunner writes a large amount of data to stdout and stderr
// to verify that withStdioSilenced does not deadlock when the pipe
// buffer fills.
type verboseNpmRunner struct{}

func (verboseNpmRunner) Install(_ context.Context, prefix, spec string) error {
	// Write 128 KiB to stdout and stderr — well above typical pipe
	// buffer limits (64 KiB on Linux, 16 KiB on macOS).
	big := strings.Repeat("npm progress line \n", 4096)
	_, _ = fmt.Fprint(os.Stdout, big)
	_, _ = fmt.Fprint(os.Stderr, big)

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
		"autosk": map[string]any{
			"agent": map[string]any{"first_message": "hello"},
		},
	}
	b, _ := json.MarshalIndent(pj, "", "  ")
	return os.WriteFile(filepath.Join(dir, "package.json"), b, 0o600)
}

func (verboseNpmRunner) Uninstall(_ context.Context, prefix, name string) error {
	return os.RemoveAll(filepath.Join(prefix, "node_modules", filepath.FromSlash(name)))
}

func TestAutoInstallMissingAgents_PipeDoesNotDeadlock(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ts := doltlite.New()
	if err := ts.Open(ctx, filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := ts.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer func() { _ = ts.Close() }()

	ag := agent.New(ts.DB())
	if _, err := ag.EnsureByName(ctx, "human"); err != nil {
		t.Fatalf("ensure human: %v", err)
	}

	prefix := filepath.Join(dir, "packages")
	reg, err := pkgregistry.Open(prefix, pkgregistry.WithNpm(verboseNpmRunner{}))
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}

	def := workflow.Definition{
		Name:      "wf-with-agent",
		FirstStep: "do",
		Steps: map[string]workflow.StepDef{
			"do": {
				AgentName: "@scope/agent",
				NextSteps: []workflow.TransitionDef{{TaskStatus: "done", PromptRule: "Done."}},
			},
		},
	}

	done := make(chan struct{})
	var resultErr error
	go func() {
		_, resultErr = autoInstallMissingAgents(ctx, def, ag, ts, reg, nil)
		close(done)
	}()

	select {
	case <-done:
		if resultErr != nil {
			t.Fatalf("autoInstallMissingAgents: %v", resultErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("autoInstallMissingAgents deadlocked — pipes not drained concurrently")
	}
}

// failingRuntimeNpmRunner installs agents successfully but fails when
// EnsureRuntime tries to install the runtime package, writing diagnostics
// to stderr.
type failingRuntimeNpmRunner struct{}

func (failingRuntimeNpmRunner) Install(_ context.Context, prefix, spec string) error {
	name := spec
	if i := strings.LastIndex(spec, "@"); i > 0 {
		name = spec[:i]
	}
	if name == pkgregistry.RuntimePackageName {
		_, _ = fmt.Fprintf(os.Stderr, "E404 registry not found for %s\n", spec)
		_, _ = fmt.Fprint(os.Stderr, "npm ERR! code E404\n")
		return fmt.Errorf("npm install failed for %s", spec)
	}

	dir := filepath.Join(prefix, "node_modules", filepath.FromSlash(name))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	pj := map[string]any{
		"name":    name,
		"version": "1.0.0",
		"autosk": map[string]any{
			"agent": map[string]any{"runner": "./agent.ts"},
		},
	}
	b, _ := json.MarshalIndent(pj, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "package.json"), b, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "agent.ts"), []byte("export default async () => {};"), 0o600)
}

func (failingRuntimeNpmRunner) Uninstall(_ context.Context, prefix, name string) error {
	return os.RemoveAll(filepath.Join(prefix, "node_modules", filepath.FromSlash(name)))
}

func TestAutoInstallMissingAgents_RuntimeCapturesDiagnostics(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	ts := doltlite.New()
	if err := ts.Open(ctx, filepath.Join(dir, "test.db")); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := ts.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer func() { _ = ts.Close() }()

	ag := agent.New(ts.DB())
	if _, err := ag.EnsureByName(ctx, "human"); err != nil {
		t.Fatalf("ensure human: %v", err)
	}

	prefix := filepath.Join(dir, "packages")
	reg, err := pkgregistry.Open(prefix, pkgregistry.WithNpm(failingRuntimeNpmRunner{}))
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}

	def := workflow.Definition{
		Name:      "wf-with-agent",
		FirstStep: "do",
		Steps: map[string]workflow.StepDef{
			"do": {
				AgentName: "@scope/custom-runner",
				NextSteps: []workflow.TransitionDef{{TaskStatus: "done", PromptRule: "Done."}},
			},
		},
	}

	// Capture actual terminal stdout/stderr.
	origOut, origErr := os.Stdout, os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout = wOut
	os.Stderr = wErr

	_, aerr := autoInstallMissingAgents(ctx, def, ag, ts, reg, nil)

	_ = wOut.Close()
	_ = wErr.Close()
	os.Stdout = origOut
	os.Stderr = origErr

	outBytes, _ := io.ReadAll(rOut)
	errBytes, _ := io.ReadAll(rErr)

	if aerr == nil {
		t.Fatal("expected autoInstallMissingAgents to fail")
	}

	errStr := aerr.Error()
	if !strings.Contains(errStr, "E404 registry not found") {
		t.Fatalf("error missing captured stderr diagnostics: %q", errStr)
	}
	if !strings.Contains(errStr, "npm ERR! code E404") {
		t.Fatalf("error missing npm error code: %q", errStr)
	}

	// Terminal stdout/stderr must remain clean (no TUI corruption).
	if len(outBytes) != 0 {
		t.Fatalf("stdout leaked npm output: %q", string(outBytes))
	}
	if len(errBytes) != 0 {
		t.Fatalf("stderr leaked npm output: %q", string(errBytes))
	}
}
