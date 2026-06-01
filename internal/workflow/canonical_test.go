package workflow_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"autosk/internal/workflow"
)

func TestCanonicalDefinitionJSON_StableAcrossStepOrder(t *testing.T) {
	bodyA := `{
		"name": "stable",
		"description": "Stable hash",
		"first_step": "dev",
		"steps": {
			"dev": {"agent": {"name": "developer"}, "next_steps": [{"step": "review", "prompt_rule": "go"}]},
			"review": {"agent": {"name": "code-reviewer"}, "next_steps": [{"task_status": "done", "prompt_rule": "ok"}]}
		}
	}`
	bodyB := `{
		"first_step": "dev",
		"description": "Stable hash",
		"name": "stable",
		"steps": {
			"review": {"next_steps": [{"prompt_rule": "ok", "task_status": "done"}], "agent": {"name": "code-reviewer"}},
			"dev": {"next_steps": [{"prompt_rule": "go", "step": "review"}], "agent": {"name": "developer"}}
		}
	}`
	defA, err := workflow.ParseReader(strings.NewReader(bodyA))
	if err != nil {
		t.Fatal(err)
	}
	defB, err := workflow.ParseReader(strings.NewReader(bodyB))
	if err != nil {
		t.Fatal(err)
	}
	canonA, err := workflow.CanonicalDefinitionJSON(defA)
	if err != nil {
		t.Fatal(err)
	}
	canonB, err := workflow.CanonicalDefinitionJSON(defB)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonA) != string(canonB) {
		t.Fatalf("canonical JSON differs:\nA=%s\nB=%s", canonA, canonB)
	}
	if !strings.Contains(string(canonA), `"isolation":"none"`) {
		t.Fatalf("canonical JSON should include normalized isolation: %s", canonA)
	}
	hash, err := workflow.HashDefinition(defA)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonA)
	if hash != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash mismatch: %s", hash)
	}
}

func TestCanonicalDefinitionJSON_EmptyArrayParamsRoundTripStable(t *testing.T) {
	body := `{
		"name": "empty-arrays",
		"description": "Empty array params",
		"first_step": "dev",
		"steps": {
			"dev": {
				"agent": {"name": "developer", "params": {"extra_args": [], "pi_extensions": [], "pi_skills": []}},
				"next_steps": [{"task_status": "done", "prompt_rule": "ok"}]
			}
		}
	}`
	def, err := workflow.ParseReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	canonA, err := workflow.CanonicalDefinitionJSON(def)
	if err != nil {
		t.Fatal(err)
	}
	hashA, err := workflow.HashDefinition(def)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := workflow.ParseReader(strings.NewReader(string(canonA)))
	if err != nil {
		t.Fatalf("parse canonical JSON: %v", err)
	}
	canonB, err := workflow.CanonicalDefinitionJSON(roundTrip)
	if err != nil {
		t.Fatal(err)
	}
	hashB, err := workflow.HashDefinition(roundTrip)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonA) != string(canonB) {
		t.Fatalf("canonical JSON changed after round trip:\nA=%s\nB=%s", canonA, canonB)
	}
	if hashA != hashB {
		t.Fatalf("hash changed after round trip: %s != %s", hashA, hashB)
	}
}

func TestCanonicalDefinitionJSON_TransitionOrderAffectsHash(t *testing.T) {
	base := workflow.Definition{
		Name:      "ordered",
		FirstStep: "dev",
		Steps: map[string]workflow.StepDef{
			"dev": {
				AgentName: "developer",
				NextSteps: []workflow.TransitionDef{
					{TaskStatus: "human", PromptRule: "ask"},
					{TaskStatus: "done", PromptRule: "finish"},
				},
			},
		},
	}
	reversed := base
	reversed.Steps = map[string]workflow.StepDef{
		"dev": {
			AgentName: "developer",
			NextSteps: []workflow.TransitionDef{
				{TaskStatus: "done", PromptRule: "finish"},
				{TaskStatus: "human", PromptRule: "ask"},
			},
		},
	}
	hashA, err := workflow.HashDefinition(base)
	if err != nil {
		t.Fatal(err)
	}
	hashB, err := workflow.HashDefinition(reversed)
	if err != nil {
		t.Fatal(err)
	}
	if hashA == hashB {
		t.Fatalf("transition order should affect definition hash")
	}
}
