package workflow_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosk/internal/workflow"
)

func TestParseFile_Example(t *testing.T) {
	// The repo ships an example; round-trip through ParseFile.
	def, err := workflow.ParseFile("../../docs/examples/workflows/workflow-example.json")
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if def.Name != "feature-dev" {
		t.Errorf("name: %q", def.Name)
	}
	if def.FirstStep != "dev" {
		t.Errorf("first_step: %q", def.FirstStep)
	}
	if len(def.Steps) != 3 {
		t.Errorf("steps count: %d", len(def.Steps))
	}
	if got := def.StepNames; len(got) != 3 || got[0] != "dev" || got[1] != "review" || got[2] != "validator" {
		t.Errorf("step order: %v", got)
	}
	val, ok := def.Steps["validator"]
	if !ok {
		t.Fatal("missing validator")
	}
	if len(val.NextSteps) != 2 {
		t.Fatalf("validator transitions: %d", len(val.NextSteps))
	}
	gotStatus := val.NextSteps[1].TaskStatus
	if gotStatus != "human" {
		t.Errorf("validator's second transition task_status: %q", gotStatus)
	}
}

func TestParse_ExactlyOneTarget(t *testing.T) {
	body := `{
		"name": "x", "first_step": "a",
		"steps": {
			"a": {"agent": {"name": "dev"},
			      "next_steps": [{"step": "a", "task_status": "done", "prompt_rule": "..."}]}
		}}`
	_, err := workflow.ParseReader(strings.NewReader(body))
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("want exactly-one error, got %v", err)
	}
}

func TestParse_RejectsLegacyStringAgent(t *testing.T) {
	body := `{
		"name": "x", "first_step": "a",
		"steps": {
			"a": {"agent": "dev",
			      "next_steps": [{"task_status": "done", "prompt_rule": "..."}]}
		}}`
	_, err := workflow.ParseReader(strings.NewReader(body))
	if err == nil || !strings.Contains(err.Error(), "object") {
		t.Fatalf("want object-shape error, got %v", err)
	}
}

func TestParse_AgentParamsHappyPath(t *testing.T) {
	body := `{
		"name": "x", "first_step": "a",
		"steps": {
			"a": {
				"agent": {
					"name": "dev",
					"params": {
						"model": "claude-sonnet-4-6",
						"thinking": "high",
						"first_message": "You are generic agent",
						"extra_args": ["--no-tool", "web_fetch"],
						"pi_extensions": ["/abs/ext.ts"],
						"pi_skills": ["/abs/skill"]
					}
				},
				"next_steps": [{"task_status": "done", "prompt_rule": "complete"}]
			}
		}}`
	def, err := workflow.ParseReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	step := def.Steps["a"]
	if step.AgentName != "dev" {
		t.Fatalf("agent name: %q", step.AgentName)
	}
	if step.AgentParams == nil {
		t.Fatal("AgentParams nil")
	}
	if step.AgentParams.Model == nil || *step.AgentParams.Model != "claude-sonnet-4-6" {
		t.Errorf("model: %v", step.AgentParams.Model)
	}
	if step.AgentParams.Thinking == nil || *step.AgentParams.Thinking != "high" {
		t.Errorf("thinking: %v", step.AgentParams.Thinking)
	}
	if step.AgentParams.FirstMessage == nil || *step.AgentParams.FirstMessage != "You are generic agent" {
		t.Errorf("first_message: %v", step.AgentParams.FirstMessage)
	}
	if len(step.AgentParams.ExtraArgs) != 2 || step.AgentParams.ExtraArgs[0] != "--no-tool" {
		t.Errorf("extra_args: %v", step.AgentParams.ExtraArgs)
	}
}

func TestParse_AgentParams_EmptyObjectIsNil(t *testing.T) {
	body := `{
		"name": "x", "first_step": "a",
		"steps": {
			"a": {"agent": {"name": "dev", "params": {}},
			      "next_steps": [{"task_status": "done", "prompt_rule": "."}]}
		}}`
	def, err := workflow.ParseReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if def.Steps["a"].AgentParams != nil {
		t.Fatalf("empty params should normalise to nil, got %+v", def.Steps["a"].AgentParams)
	}
}

func TestParse_AgentParams_RejectsUnknownKey(t *testing.T) {
	body := `{
		"name": "x", "first_step": "a",
		"steps": {
			"a": {"agent": {"name": "dev", "params": {"runner": "./r.ts"}},
			      "next_steps": [{"task_status": "done", "prompt_rule": "."}]}
		}}`
	_, err := workflow.ParseReader(strings.NewReader(body))
	if err == nil || !strings.Contains(err.Error(), "unknown key") || !strings.Contains(err.Error(), "runner") {
		t.Fatalf("want unknown-key error mentioning runner, got %v", err)
	}
}

func TestParse_AgentParams_RejectsFirstMessageFileViaReader(t *testing.T) {
	body := `{
		"name": "x", "first_step": "a",
		"steps": {
			"a": {"agent": {"name": "dev", "params": {"first_message_file": "./p.md"}},
			      "next_steps": [{"task_status": "done", "prompt_rule": "."}]}
		}}`
	_, err := workflow.ParseReader(strings.NewReader(body))
	if err == nil || !strings.Contains(err.Error(), "first_message_file") {
		t.Fatalf("want first_message_file error, got %v", err)
	}
}

func TestParseFile_AgentParams_InlinesFirstMessageFile(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte("You are inlined."), 0o644); err != nil {
		t.Fatal(err)
	}
	wfBody := `{
		"name": "x", "first_step": "a",
		"steps": {
			"a": {
				"agent": {"name": "dev", "params": {"first_message_file": "prompt.md"}},
				"next_steps": [{"task_status": "done", "prompt_rule": "."}]
			}
		}}`
	wfPath := filepath.Join(dir, "wf.json")
	if err := os.WriteFile(wfPath, []byte(wfBody), 0o644); err != nil {
		t.Fatal(err)
	}
	def, err := workflow.ParseFile(wfPath)
	if err != nil {
		t.Fatal(err)
	}
	p := def.Steps["a"].AgentParams
	if p == nil || p.FirstMessage == nil || *p.FirstMessage != "You are inlined." {
		t.Fatalf("first_message not inlined: %+v", p)
	}
	if p.FirstMessageFile != "" {
		t.Fatalf("FirstMessageFile should be zeroed after inline, got %q", p.FirstMessageFile)
	}
}

func TestParseFile_AgentParams_RejectsEscapingFirstMessageFile(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "secret.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	wfDir := filepath.Join(dir, "workflows")
	if err := os.MkdirAll(wfDir, 0o750); err != nil {
		t.Fatal(err)
	}
	wfBody := `{
		"name": "x", "first_step": "a",
		"steps": {
			"a": {
				"agent": {"name": "dev", "params": {"first_message_file": "../secret.md"}},
				"next_steps": [{"task_status": "done", "prompt_rule": "."}]
			}
		}}`
	wfPath := filepath.Join(wfDir, "wf.json")
	if err := os.WriteFile(wfPath, []byte(wfBody), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := workflow.ParseFile(wfPath)
	if err == nil || !strings.Contains(err.Error(), "escapes workflow file") {
		t.Fatalf("want escaping first_message_file error, got %v", err)
	}
}

func TestValidate_Structural(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "missing first_step",
			body:    `{"name":"x","steps":{"a":{"agent":{"name":"dev"},"next_steps":[{"task_status":"done","prompt_rule":"."}]}}}`,
			wantErr: "first_step",
		},
		{
			name:    "first_step not in steps",
			body:    `{"name":"x","first_step":"b","steps":{"a":{"agent":{"name":"dev"},"next_steps":[{"task_status":"done","prompt_rule":"."}]}}}`,
			wantErr: "first_step",
		},
		{
			name:    "transition to unknown step",
			body:    `{"name":"x","first_step":"a","steps":{"a":{"agent":{"name":"dev"},"next_steps":[{"step":"b","prompt_rule":"."}]}}}`,
			wantErr: "target step",
		},
		{
			name:    "bad task_status",
			body:    `{"name":"x","first_step":"a","steps":{"a":{"agent":{"name":"dev"},"next_steps":[{"task_status":"deleted","prompt_rule":"."}]}}}`,
			wantErr: "task_status",
		},
		{
			name:    "missing prompt_rule",
			body:    `{"name":"x","first_step":"a","steps":{"a":{"agent":{"name":"dev"},"next_steps":[{"task_status":"done","prompt_rule":""}]}}}`,
			wantErr: "prompt_rule",
		},
		{
			name:    "reserved prefix",
			body:    `{"name":"single:dev","first_step":"a","steps":{"a":{"agent":{"name":"dev"},"next_steps":[{"task_status":"done","prompt_rule":"."}]}}}`,
			wantErr: "reserved prefix",
		},
		{
			name:    "reserved revision suffix",
			body:    `{"name":"feature-dev@rev-old","first_step":"a","steps":{"a":{"agent":{"name":"dev"},"next_steps":[{"task_status":"done","prompt_rule":"."}]}}}`,
			wantErr: "reserved revision suffix",
		},
		{
			name:    "step with no transitions",
			body:    `{"name":"x","first_step":"a","steps":{"a":{"agent":{"name":"dev"},"next_steps":[]}}}`,
			wantErr: "at least one transition",
		},
		{
			name:    "bad thinking enum",
			body:    `{"name":"x","first_step":"a","steps":{"a":{"agent":{"name":"dev","params":{"thinking":"bogus"}},"next_steps":[{"task_status":"done","prompt_rule":"."}]}}}`,
			wantErr: "thinking",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			def, err := workflow.ParseReader(strings.NewReader(c.body))
			if err != nil {
				// Parser-level errors are also acceptable (e.g.
				// exactly-one); they're caught at parse time so
				// validate isn't reached.
				if strings.Contains(err.Error(), c.wantErr) {
					return
				}
				t.Fatalf("parse: %v", err)
			}
			err = workflow.Validate(ctx, def, nil, workflow.ValidateOpts{})
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestValidate_AgentMissing(t *testing.T) {
	t.Skip("requires real agent store; covered in store_test.go via Create round-trip")
}

func TestParse_MaxVisitsRoundTrips(t *testing.T) {
	body := `{
		"name": "x", "first_step": "a",
		"steps": {
			"a": {
				"agent": {"name": "dev"},
				"max_visits": 5,
				"next_steps": [{"task_status": "done", "prompt_rule": "."}]
			}
		}}`
	def, err := workflow.ParseReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if got := def.Steps["a"].MaxVisits; got != 5 {
		t.Fatalf("max_visits: %d (want 5)", got)
	}
}

func TestParse_MaxVisitsAbsentIsZero(t *testing.T) {
	body := `{
		"name": "x", "first_step": "a",
		"steps": {
			"a": {
				"agent": {"name": "dev"},
				"next_steps": [{"task_status": "done", "prompt_rule": "."}]
			}
		}}`
	def, err := workflow.ParseReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if got := def.Steps["a"].MaxVisits; got != 0 {
		t.Fatalf("absent max_visits: %d (want 0)", got)
	}
}

// TestParse_IsolationAbsentIsNone verifies the default for the new
// `isolation` field.
func TestParse_IsolationAbsentIsNone(t *testing.T) {
	body := `{
		"name": "x", "first_step": "a",
		"steps": {
			"a": {"agent": {"name": "dev"}, "next_steps": [{"task_status": "done", "prompt_rule": "."}]}
		}}`
	def, err := workflow.ParseReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if def.Isolation != workflow.IsolationNone {
		t.Fatalf("isolation default: %q (want none)", def.Isolation)
	}
}

func TestParse_IsolationWorktree(t *testing.T) {
	body := `{
		"name": "x", "first_step": "a", "isolation": "worktree",
		"steps": {
			"a": {"agent": {"name": "dev"}, "next_steps": [{"task_status": "done", "prompt_rule": "."}]}
		}}`
	def, err := workflow.ParseReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if def.Isolation != workflow.IsolationWorktree {
		t.Fatalf("isolation: %q (want worktree)", def.Isolation)
	}
}

func TestParse_IsolationUnknownRejected(t *testing.T) {
	body := `{
		"name": "x", "first_step": "a", "isolation": "docker",
		"steps": {
			"a": {"agent": {"name": "dev"}, "next_steps": [{"task_status": "done", "prompt_rule": "."}]}
		}}`
	_, err := workflow.ParseReader(strings.NewReader(body))
	if err == nil || !strings.Contains(err.Error(), "unknown isolation") {
		t.Fatalf("want unknown isolation error, got %v", err)
	}
}

func TestValidate_RejectsSyntheticPrefixWithWorktree(t *testing.T) {
	body := `{
		"name": "single:dev", "first_step": "a", "isolation": "worktree",
		"steps": {
			"a": {"agent": {"name": "dev"}, "next_steps": [{"task_status": "done", "prompt_rule": "."}]}
		}}`
	def, err := workflow.ParseReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	err = workflow.Validate(context.Background(), def, nil, workflow.ValidateOpts{AllowSyntheticName: true})
	if err == nil || !strings.Contains(err.Error(), "synthetic") {
		t.Fatalf("want synthetic+worktree error, got %v", err)
	}
}

func TestValidate_RejectsNegativeMaxVisits(t *testing.T) {
	body := `{
		"name": "x", "first_step": "a",
		"steps": {
			"a": {
				"agent": {"name": "dev"},
				"max_visits": -3,
				"next_steps": [{"task_status": "done", "prompt_rule": "."}]
			}
		}}`
	def, err := workflow.ParseReader(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	err = workflow.Validate(context.Background(), def, nil, workflow.ValidateOpts{})
	if err == nil || !strings.Contains(err.Error(), "max_visits") {
		t.Fatalf("want max_visits validation error, got %v", err)
	}
}
