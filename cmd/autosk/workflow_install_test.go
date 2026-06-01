package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosk/internal/agent/pkgregistry"
)

func writeWorkflowPackage(t *testing.T, root, name, version string, autosk map[string]any, workflows map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	pj := map[string]any{"name": name, "version": version}
	if autosk != nil {
		pj["autosk"] = autosk
	}
	body, _ := json.MarshalIndent(pj, "", "  ")
	if err := os.WriteFile(filepath.Join(root, "package.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	for rel, content := range workflows {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func humanWorkflowJSON(name string) string {
	return `{
  "name": "` + name + `",
  "first_step": "do",
  "steps": {
    "do": {
      "agent": { "name": "human" },
      "next_steps": [
        { "task_status": "done", "prompt_rule": "Done." }
      ]
    }
  }
}`
}

func workflowPackageAutosk(entries ...map[string]string) map[string]any {
	workflows := make([]any, 0, len(entries))
	for _, e := range entries {
		workflows = append(workflows, map[string]any{"name": e["name"], "file": e["file"]})
	}
	return map[string]any{"workflows": workflows}
}

type trackingWorkflowNpm struct {
	fakeNpmInProcess
	installs []string
}

func (n *trackingWorkflowNpm) Install(ctx context.Context, prefix, spec string) error {
	n.installs = append(n.installs, spec)
	return n.fakeNpmInProcess.Install(ctx, prefix, spec)
}

type loudWorkflowNpm struct {
	fakeNpmInProcess
}

func (n loudWorkflowNpm) Install(ctx context.Context, prefix, spec string) error {
	_, _ = fmt.Fprintf(os.Stdout, "npm progress for %s\n", spec)
	return n.fakeNpmInProcess.Install(ctx, prefix, spec)
}

func withWorkflowNpm(t *testing.T, runner pkgregistry.NpmRunner) string {
	t.Helper()
	prefix := filepath.Join(t.TempDir(), "packages")
	t.Setenv("AUTOSK_PACKAGES", prefix)
	prev := pkgregistryNpmFactory
	pkgregistryNpmFactory = func() pkgregistry.NpmRunner { return runner }
	t.Cleanup(func() { pkgregistryNpmFactory = prev })
	return prefix
}

func containsInstall(installs []string, spec string) bool {
	for _, got := range installs {
		if got == spec {
			return true
		}
	}
	return false
}

func TestWorkflowInstall_LocalPackage(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/wf-fixture", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "pkg-workflow", "file": "./workflows/pkg-workflow.json"}),
		map[string]string{"workflows/pkg-workflow.json": humanWorkflowJSON("pkg-workflow")})

	out, err := runRoot(t, dir, "workflow", "install", pkg)
	if err != nil {
		t.Fatalf("workflow install: %v\n%s", err, out)
	}
	if !strings.Contains(out, "installed workflow pkg-workflow from @autosk/wf-fixture@0.1.0") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	list, err := runRoot(t, dir, "workflow", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, "pkg-workflow") {
		t.Fatalf("workflow list missing installed workflow:\n%s", list)
	}
}

func TestWorkflowInstall_AgentRowCreation(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfJSON := `{
  "name": "agent-workflow",
  "first_step": "do",
  "steps": {
    "do": {
      "agent": { "name": "@autosk/wf-agent" },
      "next_steps": [ { "task_status": "done", "prompt_rule": "Done." } ]
    }
  }
}`
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/wf-agent", "0.2.0",
		map[string]any{
			"agent":     map[string]any{"first_message": "I am bundled."},
			"workflows": []any{map[string]any{"name": "agent-workflow", "file": "./workflow.json"}},
		},
		map[string]string{"workflow.json": wfJSON})

	out, err := runRoot(t, dir, "workflow", "install", pkg)
	if err != nil {
		t.Fatalf("workflow install: %v\n%s", err, out)
	}
	agents, err := runRoot(t, dir, "agent", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(agents, "@autosk/wf-agent") || !strings.Contains(agents, "package") {
		t.Fatalf("agent row/package source missing after workflow install:\n%s", agents)
	}
}

func TestWorkflowInstall_MultipleWorkflowSelection(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/multi-wf", "0.1.0",
		workflowPackageAutosk(
			map[string]string{"name": "one", "file": "./one.json"},
			map[string]string{"name": "two", "file": "./two.json"},
		),
		map[string]string{"one.json": humanWorkflowJSON("one"), "two.json": humanWorkflowJSON("two")})

	out, err := runRoot(t, dir, "workflow", "install", pkg)
	if err == nil {
		t.Fatalf("expected multiple-workflows error, got success:\n%s", out)
	}
	if !strings.Contains(err.Error(), "multiple workflows") || !strings.Contains(err.Error(), "--workflow") {
		t.Fatalf("wrong error: %v", err)
	}
	out, err = runRoot(t, dir, "workflow", "install", pkg, "--workflow", "two")
	if err != nil {
		t.Fatalf("workflow install --workflow: %v\n%s", err, out)
	}
	if !strings.Contains(out, "installed workflow two") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestWorkflowInstall_FirstMessageFileResolution(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfJSON := `{
  "name": "prompt-workflow",
  "first_step": "do",
  "steps": {
    "do": {
      "agent": { "name": "human", "params": { "first_message_file": "./prompts/first.md" } },
      "next_steps": [ { "task_status": "done", "prompt_rule": "Done." } ]
    }
  }
}`
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/prompt-wf", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "prompt-workflow", "file": "./workflows/prompt.json"}),
		map[string]string{
			"workflows/prompt.json":      wfJSON,
			"workflows/prompts/first.md": "PACKAGED PROMPT\n",
		})

	if out, err := runRoot(t, dir, "workflow", "install", pkg); err != nil {
		t.Fatalf("workflow install: %v\n%s", err, out)
	}
	show, err := runRoot(t, dir, "workflow", "show", "prompt-workflow", "--json")
	if err != nil {
		t.Fatalf("workflow show: %v\n%s", err, show)
	}
	if !strings.Contains(show, "PACKAGED PROMPT\\n") {
		t.Fatalf("first_message_file was not resolved relative to packaged workflow file:\n%s", show)
	}
}

func TestWorkflowInstall_RejectsEscapingFirstMessageFile(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfJSON := `{
  "name": "escape-prompt",
  "first_step": "do",
  "steps": {
    "do": {
      "agent": { "name": "human", "params": { "first_message_file": "../secret.md" } },
      "next_steps": [ { "task_status": "done", "prompt_rule": "Done." } ]
    }
  }
}`
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/escape-prompt", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "escape-prompt", "file": "./workflows/prompt.json"}),
		map[string]string{"workflows/prompt.json": wfJSON, "secret.md": "HOST SECRET"})
	_, err := runRoot(t, dir, "workflow", "install", pkg)
	if err == nil || !strings.Contains(err.Error(), "escapes workflow file") {
		t.Fatalf("want escaping first_message_file error, got %v", err)
	}
}

func TestWorkflowInstall_MissingFile(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/missing-wf", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "missing", "file": "./missing.json"}), nil)

	_, err := runRoot(t, dir, "workflow", "install", pkg)
	if err == nil {
		t.Fatal("expected missing workflow file error")
	}
	if !strings.Contains(err.Error(), "file missing") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestWorkflowInstall_ExistingWorkflow(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/existing-wf", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "existing", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("existing")})
	if out, err := runRoot(t, dir, "workflow", "install", pkg); err != nil {
		t.Fatalf("first install: %v\n%s", err, out)
	}
	_, err := runRoot(t, dir, "workflow", "install", pkg)
	if err == nil {
		t.Fatal("expected existing workflow error")
	}
	if !strings.Contains(err.Error(), "workflow already exists: existing") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestWorkflowInstall_NoInstallBehavior(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/noinstall-wf", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "noinstall", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("noinstall")})

	dirMissing := t.TempDir()
	if _, err := runRoot(t, dirMissing, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	_, err := runRoot(t, dirMissing, "workflow", "install", pkg, "--no-install")
	if err == nil {
		t.Fatal("expected --no-install absent package error")
	}
	if !strings.Contains(err.Error(), "package not installed") {
		t.Fatalf("wrong error: %v", err)
	}

	dirInstall := t.TempDir()
	if _, err := runRoot(t, dirInstall, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	if out, err := runRoot(t, dirInstall, "workflow", "install", pkg); err != nil {
		t.Fatalf("seed package install: %v\n%s", err, out)
	}
	dirNoInstall := t.TempDir()
	if _, err := runRoot(t, dirNoInstall, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	out, err := runRoot(t, dirNoInstall, "workflow", "install", pkg, "--no-install")
	if err != nil {
		t.Fatalf("--no-install should use registered package: %v\n%s", err, out)
	}
	if !strings.Contains(out, "installed workflow noinstall") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestWorkflowInstall_NoWorkflowsManifest(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/no-workflows", "0.1.0",
		map[string]any{"agent": map[string]any{}}, nil)

	_, err := runRoot(t, dir, "workflow", "install", pkg)
	if err == nil {
		t.Fatal("expected no workflows manifest error")
	}
	if !strings.Contains(err.Error(), "autosk.workflows") {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestWorkflowInstall_NoInstallDoesNotInstallMissingAgents(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	prefix := withWorkflowNpm(t, npm)
	wfJSON := `{
  "name": "noinstall-missing-agent",
  "first_step": "do",
  "steps": {
    "do": {
      "agent": { "name": "@autosk/dev-fixture" },
      "next_steps": [ { "task_status": "done", "prompt_rule": "Done." } ]
    }
  }
}`
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/noinstall-missing", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "noinstall-missing-agent", "file": "./workflow.json"}),
		map[string]string{"workflow.json": wfJSON})
	reg, err := pkgregistry.Open(prefix, pkgregistry.WithNpm(npm))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.InstallWorkflowSpec(context.Background(), "@autosk/noinstall-missing", pkg); err != nil {
		t.Fatalf("seed package install: %v", err)
	}
	before := len(npm.installs)
	dirNoInstall := t.TempDir()
	if _, err := runRoot(t, dirNoInstall, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	_, err = runRoot(t, dirNoInstall, "workflow", "install", pkg, "--no-install")
	if err == nil {
		t.Fatal("expected missing agent error")
	}
	if !strings.Contains(err.Error(), "--no-install") || !strings.Contains(err.Error(), "@autosk/dev-fixture") {
		t.Fatalf("wrong error: %v", err)
	}
	if got := len(npm.installs); got != before {
		t.Fatalf("--no-install ran npm: before=%d after=%d installs=%v", before, got, npm.installs)
	}
}

func TestWorkflowInstall_CustomRunnerRuntime(t *testing.T) {
	t.Run("bundled agent", func(t *testing.T) {
		npm := &trackingWorkflowNpm{}
		withWorkflowNpm(t, npm)
		dir := t.TempDir()
		if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
			t.Fatal(err)
		}
		wfJSON := `{
  "name": "custom-runtime",
  "first_step": "do",
  "steps": {
    "do": {
      "agent": { "name": "@autosk/custom-wf" },
      "next_steps": [ { "task_status": "done", "prompt_rule": "Done." } ]
    }
  }
}`
		pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/custom-wf", "0.1.0",
			map[string]any{
				"agent":     map[string]any{"runner": "./agent.ts"},
				"workflows": []any{map[string]any{"name": "custom-runtime", "file": "./workflow.json"}},
			},
			map[string]string{"workflow.json": wfJSON, "agent.ts": "export default async () => {};"})
		if out, err := runRoot(t, dir, "workflow", "install", pkg); err != nil {
			t.Fatalf("workflow install: %v\n%s", err, out)
		}
		if !containsInstall(npm.installs, pkgregistry.RuntimePackageName) {
			t.Fatalf("runtime was not installed for bundled custom runner: %v", npm.installs)
		}
	})
	t.Run("referenced agent", func(t *testing.T) {
		npm := &trackingWorkflowNpm{}
		withWorkflowNpm(t, npm)
		dir := t.TempDir()
		if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
			t.Fatal(err)
		}
		wfJSON := `{
  "name": "referenced-custom-runtime",
  "first_step": "do",
  "steps": {
    "do": {
      "agent": { "name": "@autosk/custom-fixture" },
      "next_steps": [ { "task_status": "done", "prompt_rule": "Done." } ]
    }
  }
}`
		pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/custom-ref-wf", "0.1.0",
			workflowPackageAutosk(map[string]string{"name": "referenced-custom-runtime", "file": "./workflow.json"}),
			map[string]string{"workflow.json": wfJSON})
		if out, err := runRoot(t, dir, "workflow", "install", pkg); err != nil {
			t.Fatalf("workflow install: %v\n%s", err, out)
		}
		if !containsInstall(npm.installs, pkgregistry.RuntimePackageName) {
			t.Fatalf("runtime was not installed for referenced custom runner: %v", npm.installs)
		}
	})
}

func TestWorkflowInstall_WorkflowOnlyPackageNotShownAsAgent(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/workflow-only", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "workflow-only", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("workflow-only")})
	if out, err := runRoot(t, dir, "workflow", "install", pkg); err != nil {
		t.Fatalf("workflow install: %v\n%s", err, out)
	}
	list, err := runRoot(t, dir, "agent", "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(list, "@autosk/workflow-only") {
		t.Fatalf("workflow-only package shown as agent:\n%s", list)
	}
	_, err = runRoot(t, dir, "agent", "show", "@autosk/workflow-only")
	if err == nil || !strings.Contains(err.Error(), "agent not found") {
		t.Fatalf("expected agent not found for workflow-only package, got %v", err)
	}
}

func TestWorkflowInstall_JSONOutput(t *testing.T) {
	withWorkflowNpm(t, loudWorkflowNpm{})
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfJSON := `{
  "name": "json-workflow",
  "first_step": "do",
  "steps": {
    "do": {
      "agent": { "name": "@autosk/dev-fixture" },
      "next_steps": [ { "task_status": "done", "prompt_rule": "Done." } ]
    }
  }
}`
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/json-wf", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "json-workflow", "file": "./workflow.json"}),
		map[string]string{"workflow.json": wfJSON})

	out, err := runRoot(t, dir, "workflow", "install", pkg, "--json")
	if err != nil {
		t.Fatalf("workflow install --json: %v\n%s", err, out)
	}
	var got struct {
		Package struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"package"`
		Workflow struct {
			Name string `json:"name"`
		} `json:"workflow"`
		AutoInstalledAgents []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"auto_installed_agents"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal json output: %v\n%s", err, out)
	}
	if got.Package.Name != "@autosk/json-wf" || got.Package.Version != "0.1.0" || got.Workflow.Name != "json-workflow" {
		t.Fatalf("unexpected json output: %+v\nraw=%s", got, out)
	}
	if len(got.AutoInstalledAgents) != 1 || got.AutoInstalledAgents[0].Name != "@autosk/dev-fixture" {
		t.Fatalf("missing auto-installed agent in json output: %+v\nraw=%s", got.AutoInstalledAgents, out)
	}
}
