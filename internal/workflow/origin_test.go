package workflow_test

import (
	"context"
	"errors"
	"testing"

	"autosk/internal/workflow"
)

func TestOrigin_UpsertGetListAndActive(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	def, err := workflow.ParseFile("../../docs/examples/workflows/workflow-example.json")
	if err != nil {
		t.Fatal(err)
	}
	w, err := wf.Create(ctx, def, false)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := workflow.HashDefinition(def)
	if err != nil {
		t.Fatal(err)
	}
	origin, err := wf.UpsertOrigin(ctx, workflow.Origin{
		WorkflowID:     w.ID,
		SourceType:     "global",
		Source:         "feature-dev",
		SourceMetadata: map[string]any{"package": "@autosk/workflows"},
		DefinitionHash: hash,
		Revision:       "rev-1",
	})
	if err != nil {
		t.Fatalf("UpsertOrigin insert: %v", err)
	}
	if origin.WorkflowID != w.ID || origin.SourceType != "global" || origin.DefinitionHash != hash || !origin.Active {
		t.Fatalf("unexpected origin: %+v", origin)
	}
	if got := origin.SourceMetadata["package"]; got != "@autosk/workflows" {
		t.Fatalf("metadata lost: %#v", origin.SourceMetadata)
	}
	if origin.CreatedAt.IsZero() || origin.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not populated: %+v", origin)
	}

	updated, err := wf.UpsertOrigin(ctx, workflow.Origin{
		WorkflowID:     w.ID,
		SourceType:     "package",
		Source:         "@autosk/workflows/feature-dev",
		DefinitionHash: "hash-2",
		Revision:       "rev-2",
	})
	if err != nil {
		t.Fatalf("UpsertOrigin update: %v", err)
	}
	if updated.SourceType != "package" || updated.DefinitionHash != "hash-2" || !updated.Active {
		t.Fatalf("origin not updated or active not preserved: %+v", updated)
	}

	all, err := wf.ListOrigins(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].WorkflowID != w.ID {
		t.Fatalf("all origins: %+v", all)
	}
	disabled, err := wf.SetOriginActive(ctx, w.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.Active {
		t.Fatalf("origin should be disabled: %+v", disabled)
	}
	active, err := wf.ListOrigins(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("active origins after disable: %+v", active)
	}
	preservedDisabled, err := wf.UpsertOrigin(ctx, workflow.Origin{
		WorkflowID:     w.ID,
		SourceType:     "package",
		Source:         "@autosk/workflows/feature-dev",
		DefinitionHash: "hash-3",
		Revision:       "rev-3",
	})
	if err != nil {
		t.Fatalf("UpsertOrigin preserve disabled: %v", err)
	}
	if preservedDisabled.Active {
		t.Fatalf("zero-value UpsertOrigin should preserve disabled state: %+v", preservedDisabled)
	}
	reenabled, err := wf.SetOriginActive(ctx, w.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reenabled.Active {
		t.Fatalf("origin should be active: %+v", reenabled)
	}
}

func TestOrigin_UpsertActiveOverride(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	def, err := workflow.ParseFile("../../docs/examples/workflows/workflow-example.json")
	if err != nil {
		t.Fatal(err)
	}
	w, err := wf.Create(ctx, def, false)
	if err != nil {
		t.Fatal(err)
	}
	inactive := false
	origin, err := wf.UpsertOrigin(ctx, workflow.Origin{WorkflowID: w.ID, SourceType: "file", ActiveOverride: &inactive})
	if err != nil {
		t.Fatal(err)
	}
	if origin.Active {
		t.Fatalf("ActiveOverride=false should insert inactive origin: %+v", origin)
	}
	active := true
	origin, err = wf.UpsertOrigin(ctx, workflow.Origin{WorkflowID: w.ID, SourceType: "file", ActiveOverride: &active})
	if err != nil {
		t.Fatal(err)
	}
	if !origin.Active {
		t.Fatalf("ActiveOverride=true should activate origin: %+v", origin)
	}
}

func TestOrigin_CascadeDelete(t *testing.T) {
	wf, _, _, done := newWFFixture(t)
	defer done()
	ctx := context.Background()
	def, err := workflow.ParseFile("../../docs/examples/workflows/workflow-example.json")
	if err != nil {
		t.Fatal(err)
	}
	w, err := wf.Create(ctx, def, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wf.UpsertOrigin(ctx, workflow.Origin{WorkflowID: w.ID, SourceType: "file", Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := wf.Delete(ctx, w.Name); err != nil {
		t.Fatal(err)
	}
	_, err = wf.GetOrigin(ctx, w.ID)
	if !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("GetOrigin after workflow delete: %v", err)
	}
}
