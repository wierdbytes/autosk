package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// CanonicalDefinitionJSON marshals def into a deterministic JSON document.
//
// The output mirrors the workflow definition file shape accepted by
// ParseReader, while normalising defaults (notably isolation="none") and
// sorting step names so map iteration and source-file object order do not
// affect hashes. Transition order is preserved because it is authored order.
func CanonicalDefinitionJSON(def Definition) ([]byte, error) {
	steps := make(map[string]canonicalStep, len(def.Steps))
	for _, stepName := range canonicalStepNames(def) {
		sd := def.Steps[stepName]
		step := canonicalStep{
			Agent: canonicalAgentRef{Name: sd.AgentName},
		}
		if params := canonicalAgentParams(sd.AgentParams); params != nil {
			step.Agent.Params = params
		}
		if sd.MaxVisits > 0 {
			step.MaxVisits = sd.MaxVisits
		}
		for _, tr := range sd.NextSteps {
			step.NextSteps = append(step.NextSteps, canonicalTransition(tr))
		}
		steps[stepName] = step
	}

	body := canonicalDoc{
		Name:        def.Name,
		Description: def.Description,
		FirstStep:   def.FirstStep,
		Isolation:   string(def.Isolation.Normalize()),
		Steps:       steps,
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal canonical workflow definition: %w", err)
	}
	return b, nil
}

// HashDefinition returns the sha256 hex digest of CanonicalDefinitionJSON(def).
func HashDefinition(def Definition) (string, error) {
	b, err := CanonicalDefinitionJSON(def)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

type canonicalDoc struct {
	Name        string                   `json:"name"`
	Description string                   `json:"description"`
	FirstStep   string                   `json:"first_step"`
	Isolation   string                   `json:"isolation"`
	Steps       map[string]canonicalStep `json:"steps"`
}

type canonicalStep struct {
	Agent     canonicalAgentRef     `json:"agent"`
	NextSteps []canonicalTransition `json:"next_steps"`
	MaxVisits int                   `json:"max_visits,omitempty"`
}

type canonicalAgentRef struct {
	Name   string       `json:"name"`
	Params *AgentParams `json:"params,omitempty"`
}

type canonicalTransition struct {
	Step       string `json:"step,omitempty"`
	TaskStatus string `json:"task_status,omitempty"`
	PromptRule string `json:"prompt_rule"`
}

func canonicalStepNames(def Definition) []string {
	out := make([]string, 0, len(def.Steps))
	for name := range def.Steps {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func canonicalAgentParams(p *AgentParams) *AgentParams {
	if p == nil {
		return nil
	}
	cp := *p
	cp.ExtraArgs = cloneNonEmptyStrings(p.ExtraArgs)
	cp.PiExtensions = cloneNonEmptyStrings(p.PiExtensions)
	cp.PiSkills = cloneNonEmptyStrings(p.PiSkills)
	if cp.IsZero() {
		return nil
	}
	return &cp
}

func cloneNonEmptyStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	return append([]string(nil), in...)
}
