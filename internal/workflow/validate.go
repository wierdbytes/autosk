package workflow

import (
	"context"
	"fmt"
	"strings"

	"autosk/internal/agent"
)

// ValidTaskStatuses is the closed set of `task_status` targets a
// transition may carry. Mirrors the SQL CHECK on
// step_transitions.task_status.
var ValidTaskStatuses = map[string]struct{}{
	"human":  {},
	"done":   {},
	"cancel": {},
}

// ValidThinkingLevels mirrors the table in pkgregistry/resolve.go.
// Kept in sync with pi's thinking levels.
var ValidThinkingLevels = map[string]struct{}{
	"":        {},
	"off":     {},
	"minimal": {},
	"low":     {},
	"medium":  {},
	"high":    {},
	"xhigh":   {},
}

// ValidateOpts narrows what Validate enforces.
type ValidateOpts struct {
	// AllowSyntheticName lets the synthetic-workflow generator bypass the
	// reserved-prefix check. User-driven `workflow create` must keep it
	// false so authors can't squat on the `single:` namespace.
	AllowSyntheticName bool
}

// Validate runs structural + referential checks on a parsed Definition.
// Agent existence is checked via the provided agent store. Pass nil for
// the agent store to skip that check (used by parser-level tests).
//
// Errors are joined into a single message so the user sees every problem
// in one pass.
func Validate(ctx context.Context, def Definition, ag *agent.Store, opts ValidateOpts) error {
	var problems []string
	addf := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if def.Name == "" {
		addf("`name` is required")
	}
	if !opts.AllowSyntheticName && strings.HasPrefix(def.Name, SyntheticPrefix) {
		addf("`name` starts with reserved prefix %q (used for synthetic single-agent workflows)", SyntheticPrefix)
	}
	if HasReservedRevisionSuffix(def.Name) {
		addf("`name` uses reserved revision suffix marker %q (used for superseded managed workflow revisions)", RevisionSuffixMarker)
	}
	iso := def.Isolation.Normalize()
	if !iso.Valid() {
		addf("`isolation` has unknown value %q (want none|worktree)", string(def.Isolation))
	}
	// Synthetic single:<agent> workflows must always run with no
	// isolation — the `--agent` shorthand has no project-root
	// expectations and must not silently allocate worktrees. EnsureSingle
	// also pins this on the insert side, but failing here gives a clean
	// error if a programmer ever tries to bypass it.
	if strings.HasPrefix(def.Name, SyntheticPrefix) && iso == IsolationWorktree {
		addf("synthetic `single:<agent>` workflows cannot use `isolation: worktree` (synthetic workflows must remain `none`)")
	}
	if def.FirstStep == "" {
		addf("`first_step` is required")
	}
	if len(def.Steps) == 0 {
		addf("`steps` is empty")
	} else if def.FirstStep != "" {
		if _, ok := def.Steps[def.FirstStep]; !ok {
			addf("`first_step` %q is not defined under `steps`", def.FirstStep)
		}
	}

	for stepName, s := range def.Steps {
		if stepName == "" {
			addf("step has empty name")
		}
		if s.AgentName == "" {
			addf("step %q: `agent.name` is required", stepName)
		}
		if len(s.NextSteps) == 0 {
			addf("step %q: needs at least one transition in `next_steps`", stepName)
		}
		if s.MaxVisits < 0 {
			addf("step %q: max_visits must be >= 0 (0 = unlimited)", stepName)
		}
		if s.AgentParams != nil {
			if s.AgentParams.Thinking != nil {
				if _, ok := ValidThinkingLevels[*s.AgentParams.Thinking]; !ok {
					addf("step %q: agent.params.thinking=%q (want one of off|minimal|low|medium|high|xhigh)",
						stepName, *s.AgentParams.Thinking)
				}
			}
			if s.AgentParams.FirstMessage != nil && s.AgentParams.FirstMessageFile != "" {
				addf("step %q: agent.params declares both first_message and first_message_file (pick one)", stepName)
			}
		}
		for i, tr := range s.NextSteps {
			if tr.PromptRule == "" {
				addf("step %q transition %d: `prompt_rule` is required", stepName, i)
			}
			if tr.IsTaskStatus() {
				if _, ok := ValidTaskStatuses[tr.TaskStatus]; !ok {
					addf("step %q transition %d: task_status %q is not in {human,done,cancel}",
						stepName, i, tr.TaskStatus)
				}
				continue
			}
			// Sibling-step target.
			if _, ok := def.Steps[tr.Step]; !ok {
				addf("step %q transition %d: target step %q is not defined", stepName, i, tr.Step)
			}
		}
	}

	// Agent existence — checked last so the user sees structural issues
	// first (they're usually the cause when an agent ref looks wrong).
	if ag != nil {
		seen := make(map[string]struct{}, len(def.Steps))
		for _, s := range def.Steps {
			if s.AgentName == "" {
				continue
			}
			if _, dup := seen[s.AgentName]; dup {
				continue
			}
			seen[s.AgentName] = struct{}{}
			if _, err := ag.GetByName(ctx, s.AgentName); err != nil {
				addf("agent %q is referenced by a step but is not installed (run `autosk agent install %s`)", s.AgentName, s.AgentName)
			}
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("workflow %q has %d problem(s):\n  - %s",
		def.Name, len(problems), strings.Join(problems, "\n  - "))
}
