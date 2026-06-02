package main

import (
	"os"
	"strings"
	"testing"

	"autosk/internal/globalworkflow"
	"autosk/internal/workflow"
)

func TestWorkflowSync_AddsGlobalWorkflowAndAutoInstallsAgent(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	r := withIsolatedGlobalWorkflows(t)
	def := cliSyncDefinition("synced-wf", "@autosk/dev-fixture", "synced workflow")
	if _, err := r.StoreDefinition(def, globalworkflow.StoreOptions{Revision: "rev-1"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap", "--skip-global-workflows"); err != nil {
		t.Fatalf("init: %v", err)
	}

	out, err := runRoot(t, dir, "workflow", "sync")
	if err != nil {
		t.Fatalf("workflow sync: %v\n%s", err, out)
	}
	if !strings.Contains(out, "workflow synced-wf: added") || !strings.Contains(out, "auto_agents: @autosk/dev-fixture@0.2.5") {
		t.Fatalf("unexpected sync output:\n%s", out)
	}
	show, err := runRoot(t, dir, "workflow", "show", "synced-wf")
	if err != nil {
		t.Fatalf("workflow show: %v\n%s", err, show)
	}
	if !strings.Contains(show, "synced-wf") || !strings.Contains(show, "@autosk/dev-fixture") {
		t.Fatalf("synced workflow missing expected content:\n%s", show)
	}
	agents, err := runRoot(t, dir, "agent", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(agents, "@autosk/dev-fixture") {
		t.Fatalf("agent was not auto-installed:\n%s", agents)
	}

	out, err = runRoot(t, dir, "workflow", "sync")
	if err != nil {
		t.Fatalf("second workflow sync: %v\n%s", err, out)
	}
	if !strings.Contains(out, "workflow synced-wf: noop") {
		t.Fatalf("second sync should be noop:\n%s", out)
	}
}

func TestWorkflowSync_DryRunDoesNotMaterialize(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	r := withIsolatedGlobalWorkflows(t)
	def := cliSyncDefinition("dry-global", "@autosk/dev-fixture", "dry run")
	if _, err := r.StoreDefinition(def, globalworkflow.StoreOptions{}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap", "--skip-global-workflows"); err != nil {
		t.Fatalf("init: %v", err)
	}

	out, err := runRoot(t, dir, "workflow", "sync", "--dry-run")
	if err != nil {
		t.Fatalf("workflow sync --dry-run: %v\n%s", err, out)
	}
	if !strings.Contains(out, "dry-run: workflow dry-global: added") {
		t.Fatalf("unexpected dry-run output:\n%s", out)
	}
	if show, err := runRoot(t, dir, "workflow", "show", "dry-global"); err == nil {
		t.Fatalf("dry-run created workflow unexpectedly:\n%s", show)
	}
	agents, err := runRoot(t, dir, "agent", "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(agents, "@autosk/dev-fixture") {
		t.Fatalf("dry-run installed agent unexpectedly:\n%s", agents)
	}
}

func TestWorkflowSync_ConflictDoesNotOverwrite(t *testing.T) {
	withIsolatedPackagesPrefix(t)
	r := withIsolatedGlobalWorkflows(t)
	global := cliSyncDefinition("same-name", "human", "global")
	if _, err := r.StoreDefinition(global, globalworkflow.StoreOptions{}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := runRoot(t, dir, "init", "--skip-bootstrap", "--skip-global-workflows"); err != nil {
		t.Fatalf("init: %v", err)
	}
	localPath := writeTempWorkflowJSON(t, dir, humanWorkflowJSON("same-name"))
	if _, err := runRoot(t, dir, "workflow", "create", "--file", localPath); err != nil {
		t.Fatalf("create local workflow: %v", err)
	}

	out, err := runRoot(t, dir, "workflow", "sync")
	if err != nil {
		t.Fatalf("workflow sync conflict should not be fatal: %v\n%s", err, out)
	}
	if !strings.Contains(out, "workflow same-name: conflict") {
		t.Fatalf("expected conflict output:\n%s", out)
	}
	show, err := runRoot(t, dir, "workflow", "show", "same-name")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(show, "global") {
		t.Fatalf("local workflow appears overwritten:\n%s", show)
	}
}

func withIsolatedGlobalWorkflows(t *testing.T) *globalworkflow.Registry {
	t.Helper()
	prefix := t.TempDir()
	t.Setenv(globalworkflow.EnvWorkflows, prefix)
	r, err := globalworkflow.Default()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func cliSyncDefinition(name, agentName, description string) workflow.Definition {
	return workflow.Definition{
		Name:        name,
		Description: description,
		FirstStep:   "do",
		Steps: map[string]workflow.StepDef{
			"do": {
				AgentName: agentName,
				NextSteps: []workflow.TransitionDef{{TaskStatus: "done", PromptRule: "done"}},
			},
		},
	}
}

func writeTempWorkflowJSON(t *testing.T, dir, body string) string {
	t.Helper()
	path := dir + "/workflow.json"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
