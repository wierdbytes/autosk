package globalworkflow_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosk/internal/globalworkflow"
	"autosk/internal/workflow"
)

func TestDefaultResolution(t *testing.T) {
	t.Setenv(globalworkflow.EnvWorkflows, filepath.Join(t.TempDir(), "env-workflows"))
	r, err := globalworkflow.Default()
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Prefix(); got != os.Getenv(globalworkflow.EnvWorkflows) {
		t.Fatalf("AUTOSK_WORKFLOWS prefix=%q", got)
	}

	t.Setenv(globalworkflow.EnvWorkflows, "")
	xdg := filepath.Join(t.TempDir(), "xdg")
	t.Setenv("XDG_DATA_HOME", xdg)
	r, err = globalworkflow.Default()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(xdg, "autosk", "workflows")
	if r.Prefix() != want {
		t.Fatalf("XDG prefix=%q want %q", r.Prefix(), want)
	}
}

func TestRegistryStoreLoadEnableDisableRemove(t *testing.T) {
	r, err := globalworkflow.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	def := testDefinition()
	entry, err := r.StoreDefinition(def, globalworkflow.StoreOptions{
		Revision:       "rev-1",
		SourceType:     "package",
		Source:         "@autosk/workflows",
		SourceMetadata: map[string]any{"workflow": "global-test"},
	})
	if err != nil {
		t.Fatalf("StoreDefinition: %v", err)
	}
	if entry.Name != "global-test" || entry.DefinitionHash == "" || !entry.Enabled {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	if _, err := os.Stat(filepath.Join(r.Prefix(), filepath.FromSlash(entry.DefinitionFile))); err != nil {
		t.Fatalf("definition file missing: %v", err)
	}
	loaded, err := r.LoadDefinition("global-test")
	if err != nil {
		t.Fatalf("LoadDefinition: %v", err)
	}
	if loaded.Name != def.Name || loaded.FirstStep != def.FirstStep {
		t.Fatalf("loaded wrong definition: %+v", loaded)
	}

	list, err := r.List(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "global-test" {
		t.Fatalf("list enabled: %+v", list)
	}
	if _, err := r.Disable("global-test"); err != nil {
		t.Fatal(err)
	}
	list, err = r.List(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("disabled entry should be hidden: %+v", list)
	}
	list, err = r.List(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Enabled {
		t.Fatalf("include disabled: %+v", list)
	}
	if _, err := r.Enable("global-test"); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove("global-test"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := r.Get("global-test"); !errors.Is(err, globalworkflow.ErrNotInstalled) {
		t.Fatalf("Get after Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.Prefix(), filepath.FromSlash(entry.DefinitionFile))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("definition file after Remove: %v", err)
	}
}

func TestRegistryStoreDefinitionRejectsReservedRevisionSuffix(t *testing.T) {
	r, err := globalworkflow.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	def := testDefinition()
	def.Name = "global-test" + workflow.RevisionSuffixMarker + "old"
	_, err = r.StoreDefinition(def, globalworkflow.StoreOptions{})
	if !errors.Is(err, globalworkflow.ErrInvalidName) {
		t.Fatalf("StoreDefinition error=%v, want ErrInvalidName", err)
	}
}

func TestRegistryLoadDefinitionRejectsTamperedHash(t *testing.T) {
	r, err := globalworkflow.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	def := testDefinition()
	entry, err := r.StoreDefinition(def, globalworkflow.StoreOptions{})
	if err != nil {
		t.Fatalf("StoreDefinition: %v", err)
	}
	tampered := def
	tampered.Steps = map[string]workflow.StepDef{
		"dev": {
			AgentName: "developer",
			NextSteps: []workflow.TransitionDef{{TaskStatus: "done", PromptRule: "tampered"}},
		},
	}
	writeStoredDefinition(t, r, entry, tampered)
	_, err = r.LoadDefinition(def.Name)
	if err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("LoadDefinition error=%v, want hash mismatch", err)
	}
}

func TestRegistryLoadDefinitionRejectsWrongName(t *testing.T) {
	r, err := globalworkflow.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	def := testDefinition()
	entry, err := r.StoreDefinition(def, globalworkflow.StoreOptions{})
	if err != nil {
		t.Fatalf("StoreDefinition: %v", err)
	}
	tampered := def
	tampered.Name = "other-workflow"
	writeStoredDefinition(t, r, entry, tampered)
	_, err = r.LoadDefinition(def.Name)
	if err == nil || !strings.Contains(err.Error(), "definition name") {
		t.Fatalf("LoadDefinition error=%v, want definition name mismatch", err)
	}
}

func writeStoredDefinition(t *testing.T, r *globalworkflow.Registry, entry globalworkflow.Entry, def workflow.Definition) {
	t.Helper()
	body, err := workflow.CanonicalDefinitionJSON(def)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(r.Prefix(), filepath.FromSlash(entry.DefinitionFile))
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func testDefinition() workflow.Definition {
	return workflow.Definition{
		Name:        "global-test",
		Description: "global registry test",
		FirstStep:   "dev",
		Steps: map[string]workflow.StepDef{
			"dev": {
				AgentName: "developer",
				NextSteps: []workflow.TransitionDef{{TaskStatus: "done", PromptRule: "done"}},
			},
		},
	}
}
