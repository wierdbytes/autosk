package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosk/internal/workflow"
)

func writeWorkflowFile(t *testing.T, path string, def workflow.Definition) {
	t.Helper()
	body, err := workflow.CanonicalDefinitionJSON(def)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func simpleDefinition(name string) workflow.Definition {
	return workflow.Definition{
		Name:        name,
		Description: "test workflow",
		FirstStep:   "dev",
		Steps: map[string]workflow.StepDef{
			"dev": {
				AgentName: "human",
				NextSteps: []workflow.TransitionDef{{TaskStatus: "done", PromptRule: "Done."}},
			},
		},
	}
}

// ----------------------------------------------------------------------
// add
// ----------------------------------------------------------------------

func TestWorkflowGlobalAdd(t *testing.T) {
	r := withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "my-wf.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("my-wf"))

	out, err := runRoot(t, dir, "workflow", "global", "add", wfPath)
	if err != nil {
		t.Fatalf("workflow global add: %v\n%s", err, out)
	}
	if !strings.Contains(out, "my-wf") {
		t.Fatalf("output missing workflow name:\n%s", out)
	}

	list, err := runRoot(t, dir, "workflow", "global", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, "my-wf") {
		t.Fatalf("list missing added workflow:\n%s", list)
	}

	// Verify definitions dir was created inside the isolated prefix.
	if _, err := os.Stat(filepath.Join(r.Prefix(), "definitions")); err != nil {
		t.Fatalf("definitions dir not created: %v", err)
	}
}

func TestWorkflowGlobalAdd_DryRun(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "dry-wf.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("dry-wf"))

	out, err := runRoot(t, dir, "workflow", "global", "add", wfPath, "--dry-run")
	if err != nil {
		t.Fatalf("workflow global add --dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "dry-wf") {
		t.Fatalf("output missing workflow name:\n%s", out)
	}

	list, err := runRoot(t, dir, "workflow", "global", "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(list, "dry-wf") {
		t.Fatalf("dry-run should not have written workflow:\n%s", list)
	}
}

func TestWorkflowGlobalAdd_DryRun_NoPrefixCreate(t *testing.T) {
	r := withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "dry-wf.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("dry-wf"))

	if _, err := runRoot(t, dir, "workflow", "global", "add", wfPath, "--dry-run"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.Prefix(), "registry.json")); !os.IsNotExist(err) {
		t.Fatalf("dry-run should not create registry.json: %v", err)
	}
}

func TestWorkflowGlobalAdd_DryRun_JSON(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "dry-wf.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("dry-wf"))

	out, err := runRoot(t, dir, "workflow", "global", "add", wfPath, "--dry-run", "--json")
	if err != nil {
		t.Fatalf("workflow global add --dry-run --json: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if result["dry_run"] != true || result["action"] != "add" || result["name"] != "dry-wf" {
		t.Fatalf("unexpected json: %s", out)
	}
	if result["would_mutate"] != true {
		t.Fatalf("expected would_mutate=true: %s", out)
	}
}

// ----------------------------------------------------------------------
// list
// ----------------------------------------------------------------------

func TestWorkflowGlobalList(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "list-wf.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("list-wf"))
	if _, err := runRoot(t, dir, "workflow", "global", "add", wfPath); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "list")
	if err != nil {
		t.Fatalf("workflow global list: %v\n%s", err, out)
	}
	if !strings.Contains(out, "list-wf") {
		t.Fatalf("list missing workflow:\n%s", out)
	}
}

func TestWorkflowGlobalList_All(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "disabled-wf.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("disabled-wf"))
	if _, err := runRoot(t, dir, "workflow", "global", "add", wfPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "workflow", "global", "disable", "disabled-wf"); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "list")
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out)
	}
	if strings.Contains(out, "disabled-wf") {
		t.Fatalf("list without --all should hide disabled:\n%s", out)
	}
	out, err = runRoot(t, dir, "workflow", "global", "list", "--all")
	if err != nil {
		t.Fatalf("list --all: %v\n%s", err, out)
	}
	if !strings.Contains(out, "disabled-wf") {
		t.Fatalf("list --all should show disabled:\n%s", out)
	}
}

// ----------------------------------------------------------------------
// show
// ----------------------------------------------------------------------

func TestWorkflowGlobalShow(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "show-wf.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("show-wf"))
	if _, err := runRoot(t, dir, "workflow", "global", "add", wfPath); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "show", "show-wf")
	if err != nil {
		t.Fatalf("workflow global show: %v\n%s", err, out)
	}
	if !strings.Contains(out, "show-wf") || !strings.Contains(out, "definition:") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestWorkflowGlobalShow_JSON(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "show-wf.json")
	def := simpleDefinition("show-wf")
	writeWorkflowFile(t, wfPath, def)
	if _, err := runRoot(t, dir, "workflow", "global", "add", wfPath); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "show", "show-wf", "--json")
	if err != nil {
		t.Fatalf("workflow global show --json: %v\n%s", err, out)
	}
	var result struct {
		Entry      map[string]any `json:"entry"`
		Definition struct {
			Name      string `json:"name"`
			FirstStep string `json:"first_step"`
			Steps     []struct {
				Name      string `json:"name"`
				NextSteps []struct {
					TaskStatus string `json:"task_status"`
				} `json:"next_steps"`
			} `json:"steps"`
		} `json:"definition"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if result.Definition.Name != "show-wf" {
		t.Fatalf("definition name mismatch: %s", result.Definition.Name)
	}
	if result.Definition.FirstStep != "dev" {
		t.Fatalf("definition first_step mismatch: %s", result.Definition.FirstStep)
	}
	if len(result.Definition.Steps) != 1 || result.Definition.Steps[0].Name != "dev" {
		t.Fatalf("definition steps mismatch: %+v", result.Definition.Steps)
	}
	if len(result.Definition.Steps[0].NextSteps) != 1 || result.Definition.Steps[0].NextSteps[0].TaskStatus != "done" {
		t.Fatalf("definition transitions mismatch: %+v", result.Definition.Steps[0].NextSteps)
	}
}

func TestWorkflowGlobalShow_Quiet(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "quiet-wf.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("quiet-wf"))
	if _, err := runRoot(t, dir, "workflow", "global", "add", wfPath); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "show", "quiet-wf", "--quiet")
	if err != nil {
		t.Fatalf("workflow global show --quiet: %v\n%s", err, out)
	}
	if strings.Contains(out, "definition:") || strings.Contains(out, "quiet-wf") {
		t.Fatalf("--quiet should suppress output:\n%s", out)
	}
}

// ----------------------------------------------------------------------
// remove
// ----------------------------------------------------------------------

func TestWorkflowGlobalRemove(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "rm-wf.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("rm-wf"))
	if _, err := runRoot(t, dir, "workflow", "global", "add", wfPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "workflow", "global", "disable", "rm-wf"); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "remove", "rm-wf")
	if err != nil {
		t.Fatalf("workflow global remove: %v\n%s", err, out)
	}
	if !strings.Contains(out, "removed global workflow: rm-wf") {
		t.Fatalf("unexpected output:\n%s", out)
	}

	list, err := runRoot(t, dir, "workflow", "global", "list", "--all")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(list, "rm-wf") {
		t.Fatalf("remove did not remove workflow:\n%s", list)
	}
}

func TestWorkflowGlobalRemove_RefusesEnabled(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "rm-enabled.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("rm-enabled"))
	if _, err := runRoot(t, dir, "workflow", "global", "add", wfPath); err != nil {
		t.Fatal(err)
	}

	_, err := runRoot(t, dir, "workflow", "global", "remove", "rm-enabled")
	if err == nil || !strings.Contains(err.Error(), "enabled") {
		t.Fatalf("expected enabled refusal, got %v", err)
	}
}

func TestWorkflowGlobalRemove_Force(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "rm-force.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("rm-force"))
	if _, err := runRoot(t, dir, "workflow", "global", "add", wfPath); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "remove", "rm-force", "--force")
	if err != nil {
		t.Fatalf("workflow global remove --force: %v\n%s", err, out)
	}
	if !strings.Contains(out, "removed global workflow: rm-force") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestWorkflowGlobalRemove_JSON(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "rm-json.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("rm-json"))
	if _, err := runRoot(t, dir, "workflow", "global", "add", wfPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "workflow", "global", "disable", "rm-json"); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "remove", "rm-json", "--json")
	if err != nil {
		t.Fatalf("workflow global remove --json: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if result["name"] != "rm-json" || result["removed"] != true {
		t.Fatalf("unexpected json: %s", out)
	}
}

// ----------------------------------------------------------------------
// enable / disable
// ----------------------------------------------------------------------

func TestWorkflowGlobalEnableDisable(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "toggle-wf.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("toggle-wf"))
	if _, err := runRoot(t, dir, "workflow", "global", "add", wfPath); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "disable", "toggle-wf")
	if err != nil {
		t.Fatalf("disable: %v\n%s", err, out)
	}
	if !strings.Contains(out, "enabled=false") {
		t.Fatalf("expected disabled:\n%s", out)
	}
	out, err = runRoot(t, dir, "workflow", "global", "enable", "toggle-wf")
	if err != nil {
		t.Fatalf("enable: %v\n%s", err, out)
	}
	if !strings.Contains(out, "enabled=true") {
		t.Fatalf("expected enabled:\n%s", out)
	}
}

// ----------------------------------------------------------------------
// sync
// ----------------------------------------------------------------------

func TestWorkflowGlobalSync(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "sync-wf.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("sync-wf"))
	if _, err := runRoot(t, dir, "workflow", "global", "add", wfPath); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "sync")
	if err != nil {
		t.Fatalf("workflow global sync: %v\n%s", err, out)
	}
	if !strings.Contains(out, "sync-wf") {
		t.Fatalf("sync output missing workflow:\n%s", out)
	}

	list, err := runRoot(t, dir, "workflow", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, "sync-wf") {
		t.Fatalf("local workflow list missing synced workflow:\n%s", list)
	}
}

// ----------------------------------------------------------------------
// adopt
// ----------------------------------------------------------------------

func TestWorkflowGlobalAdopt(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "adopt-wf.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("adopt-wf"))
	if _, err := runRoot(t, dir, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "adopt", "adopt-wf")
	if err != nil {
		t.Fatalf("workflow global adopt: %v\n%s", err, out)
	}
	if !strings.Contains(out, "adopt-wf") {
		t.Fatalf("output missing workflow:\n%s", out)
	}

	list, err := runRoot(t, dir, "workflow", "global", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, "adopt-wf") {
		t.Fatalf("global list missing adopted workflow:\n%s", list)
	}
}

func TestWorkflowGlobalAdopt_DryRun(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "adopt-dry.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("adopt-dry"))
	if _, err := runRoot(t, dir, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "adopt", "adopt-dry", "--dry-run")
	if err != nil {
		t.Fatalf("workflow global adopt --dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "adopt-dry") {
		t.Fatalf("output missing workflow:\n%s", out)
	}

	list, err := runRoot(t, dir, "workflow", "global", "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(list, "adopt-dry") {
		t.Fatalf("dry-run should not have written workflow:\n%s", list)
	}
}

func TestWorkflowGlobalAdopt_DryRun_NoPrefixCreate(t *testing.T) {
	r := withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "adopt-dry.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("adopt-dry"))
	if _, err := runRoot(t, dir, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatal(err)
	}

	if _, err := runRoot(t, dir, "workflow", "global", "adopt", "adopt-dry", "--dry-run"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.Prefix(), "registry.json")); !os.IsNotExist(err) {
		t.Fatalf("dry-run should not create registry.json: %v", err)
	}
}

// ----------------------------------------------------------------------
// install
// ----------------------------------------------------------------------

func TestWorkflowGlobalInstall_LocalPackage(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/global-wf", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "global-wf", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("global-wf")})

	out, err := runRoot(t, dir, "workflow", "global", "install", pkg)
	if err != nil {
		t.Fatalf("workflow global install: %v\n%s", err, out)
	}
	if !strings.Contains(out, "global-wf") {
		t.Fatalf("output missing workflow:\n%s", out)
	}

	list, err := runRoot(t, dir, "workflow", "global", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, "global-wf") {
		t.Fatalf("global list missing installed workflow:\n%s", list)
	}
}

func TestWorkflowGlobalInstall_DryRun_NoNpm(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	withWorkflowNpm(t, npm)
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/dry-npm", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "dry-npm", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("dry-npm")})

	out, err := runRoot(t, dir, "workflow", "global", "install", pkg, "--dry-run")
	if err != nil {
		t.Fatalf("workflow global install --dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "dry-npm") {
		t.Fatalf("output missing workflow:\n%s", out)
	}
	if len(npm.installs) != 0 {
		t.Fatalf("dry-run should not call npm: %v", npm.installs)
	}
}

func TestWorkflowGlobalInstall_DryRun_NoPrefixCreate(t *testing.T) {
	r := withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/dry-prefix", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "dry-prefix", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("dry-prefix")})

	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg, "--dry-run"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.Prefix(), "registry.json")); !os.IsNotExist(err) {
		t.Fatalf("dry-run should not create workflows registry.json: %v", err)
	}
}

func TestWorkflowGlobalInstall_DryRun_VersionMismatch(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	withWorkflowNpm(t, npm)
	withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/dry-ver", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "dry-ver", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("dry-ver")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "install", "@autosk/dry-ver", "--dry-run", "--version", "2.0.0", "--json")
	if err != nil {
		t.Fatalf("install --dry-run --version: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if result["note"] == "" {
		t.Fatalf("expected note for version mismatch: %s", out)
	}
	if result["workflow_name"] != nil {
		t.Fatalf("version mismatch dry-run should not include workflow_name: %s", out)
	}
}

func TestWorkflowGlobalInstall_DryRun_StaleInstalled(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	pkgPrefix := withWorkflowNpm(t, npm)
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/dry-stale", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "dry-stale", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("dry-stale")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}
	// Mutate the installed workflow file in the same prefix the command uses.
	instPath := filepath.Join(pkgPrefix, "node_modules", "@autosk", "dry-stale", "workflow.json")
	if err := os.WriteFile(instPath, []byte(`{"name":"dry-stale","first_step":"do","steps":{"do":{"agent":{"name":"human"},"next_steps":[{"task_status":"done","prompt_rule":" mutated"}]}}}`), 0o600); err != nil {
		t.Fatalf("mutate installed workflow: %v", err)
	}

	before := len(npm.installs)
	out, err := runRoot(t, dir, "workflow", "global", "install", "@autosk/dry-stale", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("install --dry-run --json: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if result["note"] == "" {
		t.Fatalf("expected note for stale installed: %s", out)
	}
	if result["workflow_name"] != nil || result["hash"] != nil {
		t.Fatalf("stale dry-run should not include workflow_name or hash: %s", out)
	}
	if len(npm.installs) != before {
		t.Fatalf("dry-run should not call npm: before=%d after=%d installs=%v", before, len(npm.installs), npm.installs)
	}
}

func TestWorkflowGlobalInstall_DryRun_JSON(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/dry-json", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "dry-json", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("dry-json")})

	out, err := runRoot(t, dir, "workflow", "global", "install", pkg, "--dry-run", "--json")
	if err != nil {
		t.Fatalf("install --dry-run --json: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if result["dry_run"] != true || result["action"] != "install" || result["package_name"] != "@autosk/dry-json" {
		t.Fatalf("unexpected json: %s", out)
	}
	if result["would_mutate"] != true {
		t.Fatalf("expected would_mutate=true: %s", out)
	}
}

func TestWorkflowGlobalInstall_NoInstall_VersionRejected(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/no-ver", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "no-ver", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("no-ver")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}

	_, err := runRoot(t, dir, "workflow", "global", "install", "@autosk/no-ver", "--no-install", "--version", "2.0.0")
	if err == nil || !strings.Contains(err.Error(), "cannot be used with --no-install") {
		t.Fatalf("expected --version + --no-install rejection, got %v", err)
	}
}

func TestWorkflowGlobalInstall_LocalPackage_DryRun_Escape(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/escape", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "escape", "file": "../outside.json"}),
		map[string]string{"../outside.json": humanWorkflowJSON("escape")})

	_, err := runRoot(t, dir, "workflow", "global", "install", pkg, "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("expected escape error, got %v", err)
	}
}

func TestWorkflowGlobalInstall_LocalPackage_DryRun_MissingVersion(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(t.TempDir(), "pkg")
	_ = os.MkdirAll(pkgDir, 0o750)
	pj := map[string]any{"name": "@autosk/miss-ver", "autosk": workflowPackageAutosk(map[string]string{"name": "miss-ver", "file": "./wf.json"})}
	b, _ := json.Marshal(pj)
	_ = os.WriteFile(filepath.Join(pkgDir, "package.json"), b, 0o600)
	_ = os.WriteFile(filepath.Join(pkgDir, "wf.json"), []byte(humanWorkflowJSON("miss-ver")), 0o600)

	_, err := runRoot(t, dir, "workflow", "global", "install", pkgDir, "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("expected missing version error, got %v", err)
	}
}

func TestWorkflowGlobalInstall_LocalPackage_DryRun_MalformedAgent(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(t.TempDir(), "pkg")
	_ = os.MkdirAll(pkgDir, 0o750)
	pj := map[string]any{
		"name":    "@autosk/bad-agent",
		"version": "0.1.0",
		"autosk": map[string]any{
			"agent":     map[string]any{"thinking": "invalid"},
			"workflows": []any{map[string]any{"name": "bad-agent", "file": "./wf.json"}},
		},
	}
	b, _ := json.Marshal(pj)
	_ = os.WriteFile(filepath.Join(pkgDir, "package.json"), b, 0o600)
	_ = os.WriteFile(filepath.Join(pkgDir, "wf.json"), []byte(humanWorkflowJSON("bad-agent")), 0o600)

	_, err := runRoot(t, dir, "workflow", "global", "install", pkgDir, "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "thinking") {
		t.Fatalf("expected malformed agent error, got %v", err)
	}
}

// ----------------------------------------------------------------------
// update
// ----------------------------------------------------------------------

func TestWorkflowGlobalUpdate(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	withWorkflowNpm(t, npm)
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/update-wf", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "update-wf", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("update-wf")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "update", "update-wf")
	if err != nil {
		t.Fatalf("workflow global update: %v\n%s", err, out)
	}
	if !strings.Contains(out, "update-wf") {
		t.Fatalf("output missing workflow:\n%s", out)
	}
	if !containsInstall(npm.installs, "@autosk/update-wf") {
		t.Fatalf("update should have reinstalled package: %v", npm.installs)
	}
}

func TestWorkflowGlobalUpdate_DryRun_NoNpm(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	pkgPrefix := withWorkflowNpm(t, npm)
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/update-dry", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "update-dry", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("update-dry")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}
	before := len(npm.installs)

	out, err := runRoot(t, dir, "workflow", "global", "update", "update-dry", "--dry-run", "--no-install", "--json")
	if err != nil {
		t.Fatalf("workflow global update --dry-run: %v\n%s", err, out)
	}
	if len(npm.installs) != before {
		t.Fatalf("dry-run should not call npm: before=%d after=%d", before, len(npm.installs))
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if result["note"] != nil {
		t.Fatalf("unexpected note for --no-install dry-run: %s", out)
	}
	// Check that prefix wasn't created/modified by dry-run.
	_ = pkgPrefix
}

func TestWorkflowGlobalUpdate_DryRun_NoPrefixCreate(t *testing.T) {
	pkgPrefix := withWorkflowNpm(t, &trackingWorkflowNpm{})
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/update-prefix", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "update-prefix", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("update-prefix")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}
	// Remove the package.json inside the packages prefix so we can detect
	// EnsurePrefix creating it again.
	_ = os.Remove(filepath.Join(pkgPrefix, "package.json"))

	if _, err := runRoot(t, dir, "workflow", "global", "update", "update-prefix", "--dry-run"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(pkgPrefix, "package.json")); !os.IsNotExist(err) {
		t.Fatalf("dry-run should not recreate packages prefix: %v", err)
	}
}

func TestWorkflowGlobalUpdate_DryRun_VersionMismatch(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	withWorkflowNpm(t, npm)
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/update-ver", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "update-ver", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("update-ver")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "update", "update-ver", "--dry-run", "--version", "2.0.0", "--json")
	if err != nil {
		t.Fatalf("update --dry-run --version: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if result["note"] == "" {
		t.Fatalf("expected note for version mismatch: %s", out)
	}
}

func TestWorkflowGlobalUpdate_DryRun_StaleInstalled(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	pkgPrefix := withWorkflowNpm(t, npm)
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/update-stale", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "update-stale", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("update-stale")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}
	// Mutate installed file.
	instPath := filepath.Join(pkgPrefix, "node_modules", "@autosk", "update-stale", "workflow.json")
	_ = os.WriteFile(instPath, []byte(`{"name":"update-stale","first_step":"do","steps":{"do":{"agent":{"name":"human"},"next_steps":[{"task_status":"done","prompt_rule":" mutated"}]}}}`), 0o600)

	out, err := runRoot(t, dir, "workflow", "global", "update", "update-stale", "--dry-run", "--json")
	if err != nil {
		t.Fatalf("update --dry-run --json: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if result["note"] == "" {
		t.Fatalf("expected note for stale installed: %s", out)
	}
	if result["new_hash"] != nil {
		t.Fatalf("stale dry-run should not include new_hash: %s", out)
	}
}

func TestWorkflowGlobalUpdate_DryRun_JSON(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	pkgPrefix := withWorkflowNpm(t, npm)
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/update-djson", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "update-djson", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("update-djson")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}
	// Mutate installed file so hashes differ.
	instPath := filepath.Join(pkgPrefix, "node_modules", "@autosk", "update-djson", "workflow.json")
	_ = os.WriteFile(instPath, []byte(`{"name":"update-djson","first_step":"do","steps":{"do":{"agent":{"name":"human"},"next_steps":[{"task_status":"done","prompt_rule":" changed"}]}}}`), 0o600)

	out, err := runRoot(t, dir, "workflow", "global", "update", "update-djson", "--dry-run", "--no-install", "--json")
	if err != nil {
		t.Fatalf("update --dry-run --json: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if result["dry_run"] != true || result["action"] != "update" || result["name"] != "update-djson" {
		t.Fatalf("unexpected json: %s", out)
	}
	if result["previous_hash"] == "" || result["new_hash"] == "" {
		t.Fatalf("expected hash fields: %s", out)
	}
	if result["would_mutate"] != true {
		t.Fatalf("expected would_mutate=true: %s", out)
	}
}

func TestWorkflowGlobalUpdate_DryRun_JSON_NoOp(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	withWorkflowNpm(t, npm)
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/update-noop", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "update-noop", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("update-noop")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "update", "update-noop", "--dry-run", "--no-install", "--json")
	if err != nil {
		t.Fatalf("update --dry-run --json: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if result["dry_run"] != true || result["action"] != "update" || result["name"] != "update-noop" {
		t.Fatalf("unexpected json: %s", out)
	}
	if result["previous_hash"] != result["new_hash"] {
		t.Fatalf("expected matching hashes for no-op: %s", out)
	}
	if result["would_mutate"] != false {
		t.Fatalf("expected would_mutate=false: %s", out)
	}
}

func TestWorkflowGlobalUpdate_DryRun_Force_NoOp(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	withWorkflowNpm(t, npm)
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/update-force", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "update-force", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("update-force")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "update", "update-force", "--dry-run", "--force", "--no-install", "--json")
	if err != nil {
		t.Fatalf("update --dry-run --force --json: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if result["dry_run"] != true || result["action"] != "update" || result["name"] != "update-force" {
		t.Fatalf("unexpected json: %s", out)
	}
	if result["previous_hash"] != result["new_hash"] {
		t.Fatalf("expected matching hashes for no-op: %s", out)
	}
	if result["would_mutate"] != true {
		t.Fatalf("expected would_mutate=true when --force is set: %s", out)
	}
}

func TestWorkflowGlobalUpdate_NoOp_JSON(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	withWorkflowNpm(t, npm)
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/update-nop", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "update-nop", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("update-nop")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "update", "update-nop", "--no-install", "--json")
	if err != nil {
		t.Fatalf("update --no-install --json: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if result["action"] != "update" || result["name"] != "update-nop" {
		t.Fatalf("unexpected json: %s", out)
	}
	if result["updated"] != false {
		t.Fatalf("expected updated=false: %s", out)
	}
}

func TestWorkflowGlobalUpdate_NoInstall_VersionRejected(t *testing.T) {
	withWorkflowNpm(t, &trackingWorkflowNpm{})
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/update-nover", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "update-nover", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("update-nover")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}

	_, err := runRoot(t, dir, "workflow", "global", "update", "update-nover", "--no-install", "--version", "2.0.0")
	if err == nil || !strings.Contains(err.Error(), "cannot be used with --no-install") {
		t.Fatalf("expected --version + --no-install rejection, got %v", err)
	}
}

func TestWorkflowGlobalUpdate_PreservesEnabledState(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	withWorkflowNpm(t, npm)
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/update-pres", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "update-pres", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("update-pres")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "workflow", "global", "disable", "update-pres"); err != nil {
		t.Fatal(err)
	}
	// Re-install with a changed file.
	_ = writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg2"), "@autosk/update-pres", "0.2.0",
		workflowPackageAutosk(map[string]string{"name": "update-pres", "file": "./workflow.json"}),
		map[string]string{"workflow.json": `{"name":"update-pres","first_step":"do","steps":{"do":{"agent":{"name":"human"},"next_steps":[{"task_status":"done","prompt_rule":"Updated"}]}}}`})

	out, err := runRoot(t, dir, "workflow", "global", "update", "update-pres")
	if err != nil {
		t.Fatalf("update: %v\n%s", err, out)
	}
	if !strings.Contains(out, "update-pres") {
		t.Fatalf("output missing workflow:\n%s", out)
	}

	show, err := runRoot(t, dir, "workflow", "global", "show", "update-pres", "--json")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, show)
	}
	var result struct {
		Entry struct {
			Enabled bool `json:"enabled"`
		} `json:"entry"`
	}
	if err := json.Unmarshal([]byte(show), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, show)
	}
	if result.Entry.Enabled {
		t.Fatalf("update should preserve disabled state")
	}
}

func TestWorkflowGlobalUpdate_MetadataRefreshOnSameHash(t *testing.T) {
	withWorkflowNpm(t, &trackingWorkflowNpm{})
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/update-meta", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "update-meta", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("update-meta")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}

	// Run update without --no-install. The fake npm runner sees the package is
	// already installed and bumps package.json version to 9.9.9 while leaving
	// the workflow JSON unchanged, so the definition hash stays the same but
	// the package version metadata changes.
	out, err := runRoot(t, dir, "workflow", "global", "update", "update-meta", "--json")
	if err != nil {
		t.Fatalf("update: %v\n%s", err, out)
	}
	var result struct {
		SourceMetadata map[string]any `json:"source_metadata"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if result.SourceMetadata == nil || result.SourceMetadata["version"] != "9.9.9" {
		t.Fatalf("expected metadata version 9.9.9 in update response, got %v", result.SourceMetadata)
	}
}

// ----------------------------------------------------------------------
// Regression tests for seventh review round
// ----------------------------------------------------------------------

func TestWorkflowGlobalInstall_LocalPackage_MultiWorkflow_DryRun(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(t.TempDir(), "pkg")
	_ = os.MkdirAll(pkgDir, 0o750)
	pj := map[string]any{
		"name":    "@autosk/multi-wf",
		"version": "0.1.0",
		"autosk": map[string]any{
			"workflows": []any{
				map[string]any{"name": "wf-a", "file": "./a.json"},
				map[string]any{"name": "wf-b", "file": "./b.json"},
			},
		},
	}
	b, _ := json.Marshal(pj)
	_ = os.WriteFile(filepath.Join(pkgDir, "package.json"), b, 0o600)
	_ = os.WriteFile(filepath.Join(pkgDir, "a.json"), []byte(humanWorkflowJSON("wf-a")), 0o600)
	_ = os.WriteFile(filepath.Join(pkgDir, "b.json"), []byte(humanWorkflowJSON("wf-b")), 0o600)

	_, err := runRoot(t, dir, "workflow", "global", "install", pkgDir, "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "multiple workflows") {
		t.Fatalf("expected multi-workflow error without --workflow, got %v", err)
	}

	// With --workflow it should succeed.
	out, err := runRoot(t, dir, "workflow", "global", "install", pkgDir, "--dry-run", "--workflow", "wf-b")
	if err != nil {
		t.Fatalf("install --workflow wf-b --dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "wf-b") {
		t.Fatalf("output missing wf-b:\n%s", out)
	}
}

func TestWorkflowGlobalInstall_LocalPackage_DryRun_BadPkgName(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(t.TempDir(), "pkg")
	_ = os.MkdirAll(pkgDir, 0o750)
	// Directory is named "pkg" but package.json name is "human" (reserved).
	pj := map[string]any{
		"name":    "human",
		"version": "0.1.0",
		"autosk": map[string]any{
			"workflows": []any{map[string]any{"name": "bad-name", "file": "./wf.json"}},
		},
	}
	b, _ := json.Marshal(pj)
	_ = os.WriteFile(filepath.Join(pkgDir, "package.json"), b, 0o600)
	_ = os.WriteFile(filepath.Join(pkgDir, "wf.json"), []byte(humanWorkflowJSON("bad-name")), 0o600)

	_, err := runRoot(t, dir, "workflow", "global", "install", pkgDir, "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected reserved name error, got %v", err)
	}
}

func TestWorkflowGlobalInstall_LocalPackage_DryRun_AgentPathEscape(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(t.TempDir(), "pkg")
	_ = os.MkdirAll(pkgDir, 0o750)
	pj := map[string]any{
		"name":    "@autosk/bad-agent-path",
		"version": "0.1.0",
		"autosk": map[string]any{
			"agent":     map[string]any{"runner": "../runner.js"},
			"workflows": []any{map[string]any{"name": "bad-agent-path", "file": "./wf.json"}},
		},
	}
	b, _ := json.Marshal(pj)
	_ = os.WriteFile(filepath.Join(pkgDir, "package.json"), b, 0o600)
	_ = os.WriteFile(filepath.Join(pkgDir, "wf.json"), []byte(humanWorkflowJSON("bad-agent-path")), 0o600)

	_, err := runRoot(t, dir, "workflow", "global", "install", pkgDir, "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("expected runner path escape error, got %v", err)
	}
}

func TestWorkflowGlobalInstall_LocalPackage_DryRun_MissingAgentFile(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(t.TempDir(), "pkg")
	_ = os.MkdirAll(pkgDir, 0o750)
	pj := map[string]any{
		"name":    "@autosk/miss-agent-file",
		"version": "0.1.0",
		"autosk": map[string]any{
			"agent":     map[string]any{"first_message_file": "./missing.txt"},
			"workflows": []any{map[string]any{"name": "miss-agent-file", "file": "./wf.json"}},
		},
	}
	b, _ := json.Marshal(pj)
	_ = os.WriteFile(filepath.Join(pkgDir, "package.json"), b, 0o600)
	_ = os.WriteFile(filepath.Join(pkgDir, "wf.json"), []byte(humanWorkflowJSON("miss-agent-file")), 0o600)

	_, err := runRoot(t, dir, "workflow", "global", "install", pkgDir, "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "first_message_file") {
		t.Fatalf("expected missing first_message_file error, got %v", err)
	}
}

func TestWorkflowGlobalInstall_JSON_NpmSilenced(t *testing.T) {
	// Install a package with --json; npm progress must not break
	// the JSON document.
	withWorkflowNpm(t, loudWorkflowNpm{})
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/silent-json", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "silent-json", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("silent-json")})

	out, err := runRoot(t, dir, "workflow", "global", "install", pkg, "--json")
	if err != nil {
		t.Fatalf("install --json: %v\n%s", err, out)
	}
	// Verify the output is a single valid JSON object.
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if result["name"] != "silent-json" {
		t.Fatalf("unexpected json: %s", out)
	}
	if result["source"] != "@autosk/silent-json" {
		t.Fatalf("unexpected source: %s", out)
	}
}

func TestWorkflowGlobalUpdate_MultiWorkflow(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	pkgPrefix := withWorkflowNpm(t, npm)
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/multi-update", "0.1.0",
		workflowPackageAutosk(
			map[string]string{"name": "wf-a", "file": "./a.json"},
			map[string]string{"name": "wf-b", "file": "./b.json"},
		),
		map[string]string{
			"a.json": humanWorkflowJSON("wf-a"),
			"b.json": humanWorkflowJSON("wf-b"),
		})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg, "--workflow", "wf-b"); err != nil {
		t.Fatal(err)
	}

	// Update the installed package.
	_ = writeWorkflowPackage(t, filepath.Join(pkgPrefix, "node_modules", "@autosk", "multi-update"), "@autosk/multi-update", "0.2.0",
		workflowPackageAutosk(
			map[string]string{"name": "wf-a", "file": "./a.json"},
			map[string]string{"name": "wf-b", "file": "./b.json"},
		),
		map[string]string{
			"a.json": humanWorkflowJSON("wf-a"),
			"b.json": `{"name":"wf-b","first_step":"do","steps":{"do":{"agent":{"name":"human"},"next_steps":[{"task_status":"done","prompt_rule":"Updated"}]}}}`,
		})

	out, err := runRoot(t, dir, "workflow", "global", "update", "wf-b", "--no-install")
	if err != nil {
		t.Fatalf("update: %v\n%s", err, out)
	}
	if !strings.Contains(out, "wf-b") {
		t.Fatalf("output missing workflow:\n%s", out)
	}
}

func TestWorkflowGlobalUpdate_NameMismatch(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	pkgPrefix := withWorkflowNpm(t, npm)
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/mismatch", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "mismatch", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("mismatch")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}

	// Mutate the installed workflow file so its definition name no longer
	// matches the global workflow name being updated.
	instPath := filepath.Join(pkgPrefix, "node_modules", "@autosk", "mismatch", "workflow.json")
	_ = os.WriteFile(instPath, []byte(`{"name":"other-wf","first_step":"do","steps":{"do":{"agent":{"name":"human"},"next_steps":[{"task_status":"done","prompt_rule":"Done"}]}}}`), 0o600)

	_, err := runRoot(t, dir, "workflow", "global", "update", "mismatch", "--no-install")
	if err == nil || !strings.Contains(err.Error(), "does not match workflow file name") {
		t.Fatalf("expected manifest/definition name mismatch error, got %v", err)
	}
}

func TestWorkflowGlobalAdopt_TwoStepTransition(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "adopt-two.json")
	writeWorkflowFile(t, wfPath, workflow.Definition{
		Name:        "adopt-two",
		Description: "two-step",
		FirstStep:   "first",
		Steps: map[string]workflow.StepDef{
			"first": {
				AgentName: "human",
				NextSteps: []workflow.TransitionDef{{Step: "second", PromptRule: "Go to second."}},
			},
			"second": {
				AgentName: "human",
				NextSteps: []workflow.TransitionDef{{TaskStatus: "done", PromptRule: "Done."}},
			},
		},
	})
	if _, err := runRoot(t, dir, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "adopt", "adopt-two")
	if err != nil {
		t.Fatalf("adopt: %v\n%s", err, out)
	}
	if !strings.Contains(out, "adopt-two") {
		t.Fatalf("output missing workflow:\n%s", out)
	}

	show, err := runRoot(t, dir, "workflow", "global", "show", "adopt-two", "--json")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, show)
	}
	var result struct {
		Definition struct {
			Steps []struct {
				Name      string `json:"name"`
				NextSteps []struct {
					Step       string `json:"step,omitempty"`
					TaskStatus string `json:"task_status,omitempty"`
				} `json:"next_steps"`
			} `json:"steps"`
		} `json:"definition"`
	}
	if err := json.Unmarshal([]byte(show), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, show)
	}
	if len(result.Definition.Steps) != 2 {
		t.Fatalf("expected 2 steps, got %d", len(result.Definition.Steps))
	}
	// first step should transition to "second" by step name, not empty.
	found := false
	for _, st := range result.Definition.Steps {
		if st.Name == "first" {
			for _, tr := range st.NextSteps {
				if tr.Step == "second" {
					found = true
					break
				}
			}
		}
	}
	if !found {
		t.Fatalf("first step should transition to 'second', got %+v", result.Definition.Steps)
	}
}

// ----------------------------------------------------------------------
// Regression tests for eighth review round
// ----------------------------------------------------------------------

func TestWorkflowGlobalInstall_LocalPackage_DryRun_AbsoluteWorkflowPath(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(t.TempDir(), "pkg")
	_ = os.MkdirAll(pkgDir, 0o750)
	pj := map[string]any{
		"name":    "@autosk/abs-wf",
		"version": "0.1.0",
		"autosk": map[string]any{
			"workflows": []any{map[string]any{"name": "abs-wf", "file": "/etc/passwd"}},
		},
	}
	b, _ := json.Marshal(pj)
	_ = os.WriteFile(filepath.Join(pkgDir, "package.json"), b, 0o600)

	_, err := runRoot(t, dir, "workflow", "global", "install", pkgDir, "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "absolute paths are not allowed") {
		t.Fatalf("expected absolute path error, got %v", err)
	}
}

func TestWorkflowGlobalInstall_LocalPackage_DryRun_AbsoluteAgentPath(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(t.TempDir(), "pkg")
	_ = os.MkdirAll(pkgDir, 0o750)
	pj := map[string]any{
		"name":    "@autosk/abs-agent",
		"version": "0.1.0",
		"autosk": map[string]any{
			"agent":     map[string]any{"runner": "/etc/passwd"},
			"workflows": []any{map[string]any{"name": "abs-agent", "file": "./wf.json"}},
		},
	}
	b, _ := json.Marshal(pj)
	_ = os.WriteFile(filepath.Join(pkgDir, "package.json"), b, 0o600)
	_ = os.WriteFile(filepath.Join(pkgDir, "wf.json"), []byte(humanWorkflowJSON("abs-agent")), 0o600)

	_, err := runRoot(t, dir, "workflow", "global", "install", pkgDir, "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "absolute paths are not allowed") {
		t.Fatalf("expected absolute path error, got %v", err)
	}
}

func TestWorkflowGlobalInstall_DryRun_RemoteNameMismatch(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	pkgPrefix := withWorkflowNpm(t, npm)
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/mismatch-dry", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "mismatch-dry", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("mismatch-dry")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}
	// Mutate the installed workflow file so its definition name differs.
	instPath := filepath.Join(pkgPrefix, "node_modules", "@autosk", "mismatch-dry", "workflow.json")
	_ = os.WriteFile(instPath, []byte(`{"name":"other-wf","first_step":"do","steps":{"do":{"agent":{"name":"human"},"next_steps":[{"task_status":"done","prompt_rule":"Done"}]}}}`), 0o600)

	_, err := runRoot(t, dir, "workflow", "global", "install", "@autosk/mismatch-dry", "--dry-run", "--version", "0.1.0")
	if err == nil || !strings.Contains(err.Error(), "does not match workflow file name") {
		t.Fatalf("expected name mismatch error, got %v", err)
	}
}

// ----------------------------------------------------------------------
// Regression tests for tenth review round
// ----------------------------------------------------------------------

func TestWorkflowGlobalInstall_DryRun_NoInstall_Remote_Installed(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	withWorkflowNpm(t, npm)
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/dry-noinst", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "dry-noinst", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("dry-noinst")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}
	before := len(npm.installs)

	out, err := runRoot(t, dir, "workflow", "global", "install", "@autosk/dry-noinst", "--dry-run", "--no-install", "--json")
	if err != nil {
		t.Fatalf("install --dry-run --no-install: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if result["dry_run"] != true {
		t.Fatalf("expected dry_run=true: %s", out)
	}
	if result["workflow_name"] != "dry-noinst" {
		t.Fatalf("expected workflow_name=dry-noinst: %s", out)
	}
	if result["hash"] == "" {
		t.Fatalf("expected hash: %s", out)
	}
	if len(npm.installs) != before {
		t.Fatalf("dry-run --no-install should not call npm: before=%d after=%d", before, len(npm.installs))
	}
}

func TestWorkflowGlobalInstall_DryRun_NoInstall_Remote_Absent(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	withWorkflowNpm(t, npm)
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}

	_, err := runRoot(t, dir, "workflow", "global", "install", "@autosk/absent-pkg", "--dry-run", "--no-install")
	if err == nil || !strings.Contains(err.Error(), "package not installed") {
		t.Fatalf("expected package not installed error, got %v", err)
	}
}

func TestWorkflowGlobalUpdate_DryRun_MetadataChanged(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	pkgPrefix := withWorkflowNpm(t, npm)
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/update-meta-dry", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "update-meta-dry", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("update-meta-dry")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}

	// Bump the installed package version without changing the workflow JSON,
	// so the definition hash stays the same but the metadata version changes.
	instPkgJSON := filepath.Join(pkgPrefix, "node_modules", "@autosk", "update-meta-dry", "package.json")
	b, _ := os.ReadFile(filepath.Clean(instPkgJSON))
	var pj map[string]any
	_ = json.Unmarshal(b, &pj)
	pj["version"] = "9.9.9"
	newBody, _ := json.Marshal(pj)
	_ = os.WriteFile(instPkgJSON, newBody, 0o600)

	// Also update registry.json so pkgReg.Get sees the new version.
	regPath := filepath.Join(pkgPrefix, "registry.json")
	rb, _ := os.ReadFile(filepath.Clean(regPath))
	var reg map[string]any
	_ = json.Unmarshal(rb, &reg)
	if agents, ok := reg["agents"].(map[string]any); ok {
		if entry, ok := agents["@autosk/update-meta-dry"].(map[string]any); ok {
			entry["version"] = "9.9.9"
		}
	}
	rb, _ = json.Marshal(reg)
	_ = os.WriteFile(regPath, rb, 0o600)

	out, err := runRoot(t, dir, "workflow", "global", "update", "update-meta-dry", "--dry-run", "--no-install", "--json")
	if err != nil {
		t.Fatalf("update --dry-run --no-install --json: %v\n%s", err, out)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if result["dry_run"] != true {
		t.Fatalf("expected dry_run=true: %s", out)
	}
	if result["previous_hash"] != result["new_hash"] {
		t.Fatalf("expected matching hashes when workflow unchanged: %s", out)
	}
	if result["would_mutate"] != true {
		t.Fatalf("expected would_mutate=true for metadata-only change: %s", out)
	}
}

// ----------------------------------------------------------------------
// Regression tests for eleventh review round
// ----------------------------------------------------------------------

func TestWorkflowGlobalInstall_Local_NoInstallRejected(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(t.TempDir(), "pkg")
	_ = os.MkdirAll(pkgDir, 0o750)
	_ = os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(`{"name":"@autosk/local-ni","version":"0.1.0","autosk":{"workflows":[{"name":"local-ni","file":"./wf.json"}]}}`), 0o600)
	_ = os.WriteFile(filepath.Join(pkgDir, "wf.json"), []byte(humanWorkflowJSON("local-ni")), 0o600)

	_, err := runRoot(t, dir, "workflow", "global", "install", pkgDir, "--no-install")
	if err == nil || !strings.Contains(err.Error(), "cannot be used with local paths") {
		t.Fatalf("expected local + --no-install rejection, got %v", err)
	}
}

func TestWorkflowGlobalInstall_Local_NoInstall_DryRunRejected(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(t.TempDir(), "pkg")
	_ = os.MkdirAll(pkgDir, 0o750)
	_ = os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(`{"name":"@autosk/local-dry","version":"0.1.0","autosk":{"workflows":[{"name":"local-dry","file":"./wf.json"}]}}`), 0o600)
	_ = os.WriteFile(filepath.Join(pkgDir, "wf.json"), []byte(humanWorkflowJSON("local-dry")), 0o600)

	_, err := runRoot(t, dir, "workflow", "global", "install", pkgDir, "--dry-run", "--no-install")
	if err == nil || !strings.Contains(err.Error(), "cannot be used with local paths") {
		t.Fatalf("expected local + --no-install dry-run rejection, got %v", err)
	}
}

func TestWorkflowGlobalSync_NoAutoInitSync(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "sync-wf.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("sync-wf"))
	if _, err := runRoot(t, dir, "workflow", "global", "add", wfPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "workflow", "global", "enable", "sync-wf"); err != nil {
		t.Fatal(err)
	}

	// Fresh directory with no .autosk/db
	freshDir := t.TempDir()
	out, err := runRoot(t, freshDir, "workflow", "global", "sync", "--json")
	if err != nil {
		t.Fatalf("sync: %v\n%s", err, out)
	}
	// Extract the JSON line from mixed stdout+stderr output.
	var jsonLine string
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
			jsonLine = trimmed
			break
		}
	}
	if jsonLine == "" {
		t.Fatalf("no JSON line found in output:\n%s", out)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(jsonLine), &result); err != nil {
		t.Fatalf("unmarshal: %v\nraw:%s", err, jsonLine)
	}
	workflows, ok := result["workflows"].([]any)
	if !ok || len(workflows) == 0 {
		t.Fatalf("expected at least one workflow in sync report: %s", jsonLine)
	}
	first := workflows[0].(map[string]any)
	if first["status"] != "added" {
		t.Fatalf("expected status=added, got %s (auto-init may have consumed the sync): %s", first["status"], jsonLine)
	}
}

// ----------------------------------------------------------------------
// Regression tests for fourteenth review round — workflow validation
// ----------------------------------------------------------------------

func TestWorkflowGlobalAdd_InvalidDefinition(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "bad-wf.json")
	// first_step references a missing step — validation should reject this.
	writeWorkflowFile(t, wfPath, workflow.Definition{
		Name:      "bad-wf",
		FirstStep: "missing",
		Steps: map[string]workflow.StepDef{
			"dev": {
				AgentName: "human",
				NextSteps: []workflow.TransitionDef{{TaskStatus: "done", PromptRule: "Done."}},
			},
		},
	})

	_, err := runRoot(t, dir, "workflow", "global", "add", wfPath)
	if err == nil || !strings.Contains(err.Error(), "first_step") {
		t.Fatalf("expected validation error for missing first_step, got %v", err)
	}

	// Also reject in dry-run.
	_, err = runRoot(t, dir, "workflow", "global", "add", wfPath, "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "first_step") {
		t.Fatalf("expected dry-run validation error for missing first_step, got %v", err)
	}
}

func TestWorkflowGlobalInstall_InvalidDefinition(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(t.TempDir(), "pkg")
	_ = os.MkdirAll(pkgDir, 0o750)
	pj := map[string]any{
		"name":    "@autosk/bad-wf",
		"version": "0.1.0",
		"autosk": map[string]any{
			"workflows": []any{map[string]any{"name": "bad-wf", "file": "./wf.json"}},
		},
	}
	b, _ := json.Marshal(pj)
	_ = os.WriteFile(filepath.Join(pkgDir, "package.json"), b, 0o600)
	// Transition targets a missing step.
	_ = os.WriteFile(filepath.Join(pkgDir, "wf.json"), []byte(`{"name":"bad-wf","first_step":"dev","steps":{"dev":{"agent":{"name":"human"},"next_steps":[{"step":"missing","prompt_rule":"Go"}]}}}`), 0o600)

	_, err := runRoot(t, dir, "workflow", "global", "install", pkgDir)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected validation error for missing step target, got %v", err)
	}

	// Dry-run should also reject.
	_, err = runRoot(t, dir, "workflow", "global", "install", pkgDir, "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected dry-run validation error for missing step target, got %v", err)
	}
}

func TestWorkflowGlobalUpdate_InvalidDefinition(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	pkgPrefix := withWorkflowNpm(t, npm)
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/update-bad", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "update-bad", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("update-bad")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}

	// Mutate the installed workflow to have an invalid transition target.
	instPath := filepath.Join(pkgPrefix, "node_modules", "@autosk", "update-bad", "workflow.json")
	_ = os.WriteFile(instPath, []byte(`{"name":"update-bad","first_step":"dev","steps":{"dev":{"agent":{"name":"human"},"next_steps":[{"step":"missing","prompt_rule":"Go"}]}}}`), 0o600)

	_, err := runRoot(t, dir, "workflow", "global", "update", "update-bad", "--no-install")
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected validation error for missing step target, got %v", err)
	}

	// Dry-run should also reject.
	_, err = runRoot(t, dir, "workflow", "global", "update", "update-bad", "--dry-run", "--no-install")
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected dry-run validation error for missing step target, got %v", err)
	}
}

// ----------------------------------------------------------------------
// --quiet takes precedence over --json (regression for review round 15)
// ----------------------------------------------------------------------

func TestWorkflowGlobalAdd_QuietSuppressesJSON(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "quiet-add.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("quiet-add"))

	// Non-dry-run
	out, err := runRoot(t, dir, "workflow", "global", "add", wfPath, "--json", "--quiet")
	if err != nil {
		t.Fatalf("add --json --quiet: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("--quiet should suppress JSON output: %q", out)
	}

	// Dry-run
	out, err = runRoot(t, dir, "workflow", "global", "add", wfPath, "--dry-run", "--json", "--quiet")
	if err != nil {
		t.Fatalf("add --dry-run --json --quiet: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("--quiet should suppress dry-run JSON output: %q", out)
	}
}

func TestWorkflowGlobalList_QuietSuppressesJSON(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "quiet-list.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("quiet-list"))
	if _, err := runRoot(t, dir, "workflow", "global", "add", wfPath); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "list", "--json", "--quiet")
	if err != nil {
		t.Fatalf("list --json --quiet: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("--quiet should suppress JSON output: %q", out)
	}
}

func TestWorkflowGlobalShow_QuietSuppressesJSON(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "quiet-show.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("quiet-show"))
	if _, err := runRoot(t, dir, "workflow", "global", "add", wfPath); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "show", "quiet-show", "--json", "--quiet")
	if err != nil {
		t.Fatalf("show --json --quiet: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("--quiet should suppress JSON output: %q", out)
	}
}

func TestWorkflowGlobalRemove_QuietSuppressesJSON(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "quiet-rm.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("quiet-rm"))
	if _, err := runRoot(t, dir, "workflow", "global", "add", wfPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "workflow", "global", "disable", "quiet-rm"); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "remove", "quiet-rm", "--json", "--quiet")
	if err != nil {
		t.Fatalf("remove --json --quiet: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("--quiet should suppress JSON output: %q", out)
	}
}

func TestWorkflowGlobalSync_QuietSuppressesJSON(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "quiet-sync.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("quiet-sync"))
	if _, err := runRoot(t, dir, "workflow", "global", "add", wfPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "workflow", "global", "enable", "quiet-sync"); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "sync", "--json", "--quiet")
	if err != nil {
		t.Fatalf("sync --json --quiet: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("--quiet should suppress JSON output: %q", out)
	}
}

func TestWorkflowGlobalInstall_QuietSuppressesJSON(t *testing.T) {
	withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/quiet-inst", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "quiet-inst", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("quiet-inst")})

	// Non-dry-run
	out, err := runRoot(t, dir, "workflow", "global", "install", pkg, "--json", "--quiet")
	if err != nil {
		t.Fatalf("install --json --quiet: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("--quiet should suppress JSON output: %q", out)
	}

	// Dry-run
	out, err = runRoot(t, dir, "workflow", "global", "install", pkg, "--dry-run", "--json", "--quiet")
	if err != nil {
		t.Fatalf("install --dry-run --json --quiet: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("--quiet should suppress dry-run JSON output: %q", out)
	}
}

func TestWorkflowGlobalUpdate_QuietSuppressesJSON(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	withWorkflowNpm(t, npm)
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/quiet-up", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "quiet-up", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("quiet-up")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}

	// Non-dry-run no-op
	out, err := runRoot(t, dir, "workflow", "global", "update", "quiet-up", "--no-install", "--json", "--quiet")
	if err != nil {
		t.Fatalf("update --no-install --json --quiet: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("--quiet should suppress JSON output: %q", out)
	}

	// Dry-run
	out, err = runRoot(t, dir, "workflow", "global", "update", "quiet-up", "--dry-run", "--no-install", "--json", "--quiet")
	if err != nil {
		t.Fatalf("update --dry-run --json --quiet: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("--quiet should suppress dry-run JSON output: %q", out)
	}
}

func TestWorkflowGlobalInstall_DryRun_BadPkgName(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "install", "human", "--dry-run", "--json")
	if err == nil {
		t.Fatalf("expected error for reserved package name, got: %s", out)
	}
	if !strings.Contains(err.Error(), "human") {
		t.Fatalf("error should mention reserved name: %v", err)
	}
}

func TestWorkflowGlobalAdd_PreservesDisabledState(t *testing.T) {
	r := withIsolatedGlobalWorkflows(t)
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "pres-add.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("pres-add"))
	if _, err := runRoot(t, dir, "workflow", "global", "add", wfPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "workflow", "global", "disable", "pres-add"); err != nil {
		t.Fatal(err)
	}

	// Modify the file and re-add.
	writeWorkflowFile(t, wfPath, workflow.Definition{
		Name:        "pres-add",
		Description: "updated",
		FirstStep:   "dev",
		Steps: map[string]workflow.StepDef{
			"dev": {
				AgentName: "human",
				NextSteps: []workflow.TransitionDef{{TaskStatus: "done", PromptRule: "Updated."}},
			},
		},
	})
	out, err := runRoot(t, dir, "workflow", "global", "add", wfPath)
	if err != nil {
		t.Fatalf("re-add: %v\n%s", err, out)
	}

	show, err := runRoot(t, dir, "workflow", "global", "show", "pres-add", "--json")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, show)
	}
	var result struct {
		Entry struct {
			Enabled bool `json:"enabled"`
		} `json:"entry"`
	}
	if err := json.Unmarshal([]byte(show), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, show)
	}
	if result.Entry.Enabled {
		t.Fatalf("re-add should preserve disabled state")
	}

	// Also verify the new definition was stored.
	entry, err := r.Get("pres-add")
	if err != nil {
		t.Fatal(err)
	}
	if entry.DefinitionHash == "" {
		t.Fatal("definition hash should be set")
	}
}

func TestWorkflowGlobalAdopt_PreservesDisabledState(t *testing.T) {
	r := withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "pres-adopt.json")
	writeWorkflowFile(t, wfPath, simpleDefinition("pres-adopt"))
	if _, err := runRoot(t, dir, "workflow", "create", "--file", wfPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "workflow", "global", "adopt", "pres-adopt"); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "workflow", "global", "disable", "pres-adopt"); err != nil {
		t.Fatal(err)
	}

	// Re-adopt.
	out, err := runRoot(t, dir, "workflow", "global", "adopt", "pres-adopt")
	if err != nil {
		t.Fatalf("re-adopt: %v\n%s", err, out)
	}

	show, err := runRoot(t, dir, "workflow", "global", "show", "pres-adopt", "--json")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, show)
	}
	var result struct {
		Entry struct {
			Enabled bool `json:"enabled"`
		} `json:"entry"`
	}
	if err := json.Unmarshal([]byte(show), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, show)
	}
	if result.Entry.Enabled {
		t.Fatalf("re-adopt should preserve disabled state")
	}

	entry, err := r.Get("pres-adopt")
	if err != nil {
		t.Fatal(err)
	}
	if entry.DefinitionHash == "" {
		t.Fatal("definition hash should be set")
	}
}

func TestWorkflowGlobalInstall_PreservesDisabledState(t *testing.T) {
	withWorkflowNpm(t, &trackingWorkflowNpm{})
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/pres-inst", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "pres-inst", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("pres-inst")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}
	if _, err := runRoot(t, dir, "workflow", "global", "disable", "pres-inst"); err != nil {
		t.Fatal(err)
	}

	// Re-install with --no-install.
	out, err := runRoot(t, dir, "workflow", "global", "install", "@autosk/pres-inst", "--no-install")
	if err != nil {
		t.Fatalf("re-install: %v\n%s", err, out)
	}

	show, err := runRoot(t, dir, "workflow", "global", "show", "pres-inst", "--json")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, show)
	}
	var result struct {
		Entry struct {
			Enabled bool `json:"enabled"`
		} `json:"entry"`
	}
	if err := json.Unmarshal([]byte(show), &result); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, show)
	}
	if result.Entry.Enabled {
		t.Fatalf("re-install should preserve disabled state")
	}
}

// ----------------------------------------------------------------------
// Regression tests for seventeenth review round
// ----------------------------------------------------------------------

func TestWorkflowGlobalInstall_Quiet_NpmSilenced(t *testing.T) {
	withWorkflowNpm(t, loudWorkflowNpm{})
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/quiet-npm", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "quiet-npm", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("quiet-npm")})

	out, err := runRoot(t, dir, "workflow", "global", "install", pkg, "--quiet")
	if err != nil {
		t.Fatalf("install --quiet: %v\n%s", err, out)
	}
	if strings.Contains(out, "npm progress") {
		t.Fatalf("npm progress leaked through --quiet: %q", out)
	}
}

func TestWorkflowGlobalUpdate_Quiet_NpmSilenced(t *testing.T) {
	withWorkflowNpm(t, loudWorkflowNpm{})
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/quiet-up-npm", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "quiet-up-npm", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("quiet-up-npm")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "update", "quiet-up-npm", "--quiet")
	if err != nil {
		t.Fatalf("update --quiet: %v\n%s", err, out)
	}
	if strings.Contains(out, "npm progress") {
		t.Fatalf("npm progress leaked through --quiet: %q", out)
	}
}

func TestWorkflowGlobalSync_Quiet_NpmSilenced(t *testing.T) {
	withWorkflowNpm(t, loudWorkflowNpm{})
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	// Create a global workflow that references a scoped agent so sync
	// will trigger auto-install.
	wfPath := filepath.Join(dir, "sync-agent-wf.json")
	writeWorkflowFile(t, wfPath, workflow.Definition{
		Name:      "sync-agent-wf",
		FirstStep: "do",
		Steps: map[string]workflow.StepDef{
			"do": {
				AgentName: "@autosk/dev-fixture",
				NextSteps: []workflow.TransitionDef{{TaskStatus: "done", PromptRule: "Done."}},
			},
		},
	})
	if _, err := runRoot(t, dir, "workflow", "global", "add", wfPath); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "sync", "--quiet")
	if err != nil {
		t.Fatalf("sync --quiet: %v\n%s", err, out)
	}
	if strings.Contains(out, "npm progress") {
		t.Fatalf("npm progress leaked through --quiet: %q", out)
	}
}

// ----------------------------------------------------------------------
// version pinning
// ----------------------------------------------------------------------

func TestWorkflowGlobalSync_PinsPackageVersion(t *testing.T) {
	npm := &trackingWorkflowNpm{}
	withWorkflowNpm(t, npm)
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}

	// Create a local package whose workflow references the package itself
	// as an agent. The package name matches a built-in fake-npm fixture so
	// the sync auto-install succeeds offline.
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/dev-fixture", "0.2.5",
		map[string]any{
			"workflows": []map[string]any{{"name": "versioned-wf", "file": "./workflow.json"}},
			"agent": map[string]any{
				"first_message": "You are the dev fixture.",
				"model":         "sonnet:high",
				"thinking":      "high",
			},
		},
		map[string]string{"workflow.json": `{
  "name": "versioned-wf",
  "first_step": "do",
  "steps": {
    "do": {
      "agent": { "name": "@autosk/dev-fixture" },
      "next_steps": [{ "task_status": "done", "prompt_rule": "Done." }]
    }
  }
}`})

	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg, "--workflow", "versioned-wf"); err != nil {
		t.Fatal(err)
	}

	// Sync into a fresh project. Use --skip-global-workflows on init so
	// the explicit sync command below is the one that performs the work.
	freshDir := t.TempDir()
	if _, err := runRoot(t, freshDir, "init", "--skip-bootstrap", "--skip-global-workflows"); err != nil {
		t.Fatal(err)
	}
	// Reset install tracking so we only see sync-time installs.
	npm.installs = nil

	out, err := runRoot(t, freshDir, "workflow", "global", "sync")
	if err != nil {
		t.Fatalf("sync failed: %v\n%s", err, out)
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

// ----------------------------------------------------------------------
// quiet stderr silencing / capture
// ----------------------------------------------------------------------

type loudStderrWorkflowNpm struct {
	fakeNpmInProcess
}

func (n loudStderrWorkflowNpm) Install(ctx context.Context, prefix, spec string) error {
	_, _ = fmt.Fprintf(os.Stderr, "npm stderr progress for %s\n", spec)
	return n.fakeNpmInProcess.Install(ctx, prefix, spec)
}

type failingLoudStderrWorkflowNpm struct {
	fakeNpmInProcess
}

func (n failingLoudStderrWorkflowNpm) Install(ctx context.Context, prefix, spec string) error {
	_, _ = fmt.Fprintf(os.Stderr, "npm ERR! code E404\nnpm ERR! registry not found\n")
	return errors.New("npm install failed")
}

// TestWorkflowGlobalInstall_DryRun_CorruptRegistry verifies that a
// corrupt registry.json is surfaced as an error instead of being
// treated as "not installed" and returning a success preview.
func TestWorkflowGlobalInstall_DryRun_CorruptRegistry(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}

	// Corrupt registry.json.
	prefix := os.Getenv("AUTOSK_PACKAGES")
	if err := os.WriteFile(filepath.Join(prefix, "registry.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "install", "@autosk/dev-fixture", "--dry-run", "--json")
	if err == nil {
		t.Fatalf("expected error for corrupt registry, got:\n%s", out)
	}
	if strings.Contains(out, "would_mutate") || strings.Contains(out, "note") {
		t.Fatalf("dry-run should not return success preview for corrupt registry:\n%s", out)
	}
}

// TestWorkflowGlobalUpdate_DryRun_CorruptRegistry verifies that a
// corrupt registry.json is surfaced as an error on update dry-run.
func TestWorkflowGlobalUpdate_DryRun_CorruptRegistry(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}

	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/corrupt-up", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "corrupt-up", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("corrupt-up")})
	if _, err := runRoot(t, dir, "workflow", "global", "install", pkg); err != nil {
		t.Fatal(err)
	}

	// Corrupt registry.json.
	prefix := os.Getenv("AUTOSK_PACKAGES")
	if err := os.WriteFile(filepath.Join(prefix, "registry.json"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, dir, "workflow", "global", "update", "corrupt-up", "--dry-run", "--json")
	if err == nil {
		t.Fatalf("expected error for corrupt registry, got:\n%s", out)
	}
	if strings.Contains(out, "would_mutate") || strings.Contains(out, "note") {
		t.Fatalf("dry-run should not return success preview for corrupt registry:\n%s", out)
	}
}

func TestWorkflowGlobalInstall_Quiet_NpmStderrSilenced(t *testing.T) {
	withWorkflowNpm(t, loudStderrWorkflowNpm{})
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/quiet-err", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "quiet-err", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("quiet-err")})

	out, err := runRoot(t, dir, "workflow", "global", "install", pkg, "--quiet")
	if err != nil {
		t.Fatalf("install --quiet: %v\n%s", err, out)
	}
	if strings.Contains(out, "npm stderr progress") {
		t.Fatalf("npm stderr leaked through --quiet: %q", out)
	}
}

func TestWorkflowGlobalSync_Quiet_NpmStderrSilenced(t *testing.T) {
	withWorkflowNpm(t, loudStderrWorkflowNpm{})
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(dir, "sync-err-wf.json")
	writeWorkflowFile(t, wfPath, workflow.Definition{
		Name:      "sync-err-wf",
		FirstStep: "do",
		Steps: map[string]workflow.StepDef{
			"do": {
				AgentName: "@autosk/dev-fixture",
				NextSteps: []workflow.TransitionDef{{TaskStatus: "done", PromptRule: "Done."}},
			},
		},
	})
	if _, err := runRoot(t, dir, "workflow", "global", "add", wfPath); err != nil {
		t.Fatal(err)
	}

	freshDir := t.TempDir()
	if _, err := runRoot(t, freshDir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}

	out, err := runRoot(t, freshDir, "workflow", "global", "sync", "--quiet")
	if err != nil {
		t.Fatalf("sync --quiet: %v\n%s", err, out)
	}
	if strings.Contains(out, "npm stderr progress") {
		t.Fatalf("npm stderr leaked through --quiet: %q", out)
	}
}

func TestWorkflowGlobalInstall_Quiet_NpmStderrCapturedOnFailure(t *testing.T) {
	withWorkflowNpm(t, failingLoudStderrWorkflowNpm{})
	withIsolatedGlobalWorkflows(t)
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap"); err != nil {
		t.Fatal(err)
	}
	pkg := writeWorkflowPackage(t, filepath.Join(t.TempDir(), "pkg"), "@autosk/fail-err", "0.1.0",
		workflowPackageAutosk(map[string]string{"name": "fail-err", "file": "./workflow.json"}),
		map[string]string{"workflow.json": humanWorkflowJSON("fail-err")})

	out, err := runRoot(t, dir, "workflow", "global", "install", pkg, "--quiet")
	if err == nil {
		t.Fatal("expected error for failing npm install")
	}
	if !strings.Contains(err.Error(), "npm stderr:") {
		t.Fatalf("expected captured stderr in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "registry not found") {
		t.Fatalf("expected stderr content in error, got: %v", err)
	}
	if strings.Contains(out, "npm ERR!") {
		t.Fatalf("npm stderr leaked to stdout: %q", out)
	}
}
