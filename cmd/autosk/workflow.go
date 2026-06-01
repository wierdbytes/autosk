package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"autosk/internal/agent"
	"autosk/internal/agent/pkgregistry"
	"autosk/internal/globalworkflow"
	"autosk/internal/render"
	"autosk/internal/store/doltlite"
	"autosk/internal/timeformat"
	"autosk/internal/workflow"
	"autosk/internal/worktree"
)

// workflowStoreFromCmd opens the project DB and returns a workflow.Store +
// the doltlite handle (for commits) + a close func.
func workflowStoreFromCmd(ctx context.Context, writeOK bool) (*workflow.Store, *doltlite.Store, func(), error) {
	s, closeFn, err := openStore(ctx, writeOK)
	if err != nil {
		return nil, nil, nil, err
	}
	dl, ok := s.(*doltlite.Store)
	if !ok {
		closeFn()
		return nil, nil, nil, errors.New("workflow CLI: doltlite store required")
	}
	ag := agent.New(dl.DB())
	return workflow.New(dl.DB(), ag), dl, closeFn, nil
}

func newWorkflowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workflow",
		Short: "Manage workflows (step graphs that agents traverse)",
		Long: "A workflow is a small directed graph of steps; each step has an agent\n" +
			"and a list of transitions annotated with prompt_rule text. Define one\n" +
			"in JSON (see docs/examples/workflows/workflow-example.json) and create it with\n" +
			"`autosk workflow create --file PATH`.",
	}
	cmd.AddCommand(
		newWorkflowCreateCmd(),
		newWorkflowInstallCmd(),
		newWorkflowListCmd(),
		newWorkflowShowCmd(),
		newWorkflowDeleteCmd(),
		newWorkflowUpdateCmd(),
		newWorkflowSyncCmd(),
	)
	return cmd
}

// newWorkflowUpdateCmd implements `autosk workflow update <name>
// --isolation <none|worktree>`. See docs/plans/20260521-Workflow-Update-Isolation.md
// for the safety semantics; in short:
//
//   - synthetic workflows are always rejected (single:<agent> is
//     pinned to none by construction);
//   - non-terminal tasks referencing the workflow refuse the update
//     unless --force is passed;
//   - none→worktree --force allocates a fresh worktree per task,
//     atomically rolling back every prior Ensure on the first
//     failure;
//   - worktree→none --force does NOT remove leftover directories
//     (that would discard uncommitted state); the leftover paths
//     are printed for human cleanup via `autosk worktree rm`.
func newWorkflowUpdateCmd() *cobra.Command {
	var (
		isolationFlag string
		force         bool
		dryRun        bool
	)
	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Update mutable fields on a workflow (v0.4: only --isolation)",
		Long: "Flip a workflow's isolation mode.\n\n" +
			"The only mutable field today is --isolation. Other workflow fields\n" +
			"(description, first_step, step graph) are immutable; delete+recreate\n" +
			"the workflow instead.\n\n" +
			"By default the verb refuses to run if any task currently references\n" +
			"the workflow in a non-terminal state (new+workflow_id, work,\n" +
			"human). --force overrides the refusal:\n\n" +
			"  none → worktree   each non-terminal task gets a fresh worktree\n" +
			"                    allocated from HEAD. The update is atomic: if any\n" +
			"                    one Ensure fails, every prior Ensure for this run\n" +
			"                    is rolled back.\n\n" +
			"  worktree → none   leftover worktree directories are NOT removed\n" +
			"                    (that would discard uncommitted state). Their\n" +
			"                    paths are printed; clean them with\n" +
			"                    'autosk worktree rm <task-id>' once you have\n" +
			"                    salvaged anything you need.\n\n" +
			"Use --dry-run to preview the side-effects without committing.\n\n" +
			"Synthetic workflows (single:<agent>) are always rejected: their\n" +
			"isolation is pinned to 'none' by construction.\n\n" +
			"Daemon race: this verb does NOT lock against an in-flight daemon.\n" +
			"A run that's already picked up will finish in its current cwd;\n" +
			"the new mode takes effect on the next step run. Stop the daemon\n" +
			"first if you need to be strict about the cutover.\n\n" +
			"Examples:\n" +
			"  autosk workflow update feature-dev --isolation worktree\n" +
			"  autosk workflow update feature-dev --isolation worktree --force --dry-run\n" +
			"  autosk workflow update feature-dev --isolation none --force\n",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			target, err := parseIsolationFlag(isolationFlag)
			if err != nil {
				return err
			}
			wf, dl, closeFn, err := workflowStoreFromCmd(cmd.Context(), !dryRun)
			if err != nil {
				return err
			}
			defer closeFn()

			root, perr := projectRootFromCwd()
			if perr != nil {
				return perr
			}
			wtMgr := worktree.NewManager()
			rep, err := wf.UpdateIsolation(cmd.Context(), name, target, workflow.UpdateIsolationOpts{
				Force:       force,
				DryRun:      dryRun,
				ProjectRoot: root,
				Worktrees:   wtMgr,
			})
			if err != nil {
				// FR10 / report contract: --json must always emit the
				// UpdateIsolationReport to stdout, even on the error
				// paths, so tooling that parses the JSON output sees
				// the structured outcome (offending task ids,
				// rolled-back ensures, etc.) instead of an empty
				// stdout + a string error on stderr. The exit code (set
				// non-zero by cobra returning err) carries the failure
				// signal; the JSON body carries the diagnosis.
				if flagJSON {
					_ = json.NewEncoder(os.Stdout).Encode(toWorkflowUpdateJSON(rep))
					return err
				}
				return renderWorkflowUpdateError(err, rep)
			}
			if !dryRun && !rep.Noop {
				msg := fmt.Sprintf("workflow update %s isolation=%s→%s", name, rep.From, rep.To)
				_ = dl.DoltCommit(cmd.Context(), msg)
			}
			return emitWorkflowUpdateReport(rep)
		},
	}
	cmd.Flags().StringVar(&isolationFlag, "isolation", "", "target isolation mode (none|worktree)")
	_ = cmd.MarkFlagRequired("isolation")
	cmd.Flags().BoolVar(&force, "force", false, "bypass the non-terminal-tasks guard with mode-specific side-effects")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would happen without committing")
	return cmd
}

// parseIsolationFlag validates the --isolation CLI value. We do this
// in the verb rather than at flag-parse time so the error message
// matches the contract spelled out in plan FR2.
func parseIsolationFlag(raw string) (workflow.IsolationMode, error) {
	switch raw {
	case string(workflow.IsolationNone), string(workflow.IsolationWorktree):
		return workflow.IsolationMode(raw), nil
	default:
		return "", fmt.Errorf("invalid --isolation: %q (want none|worktree)", raw)
	}
}

// workflowUpdateJSON is the JSON projection of UpdateIsolationReport.
// We re-emit it through a CLI-owned struct so the wire shape stays
// stable across internal-package refactors (lazygit's HTTP API
// pattern).
type workflowUpdateJSON struct {
	Workflow          string                 `json:"workflow"`
	From              string                 `json:"from"`
	To                string                 `json:"to"`
	Noop              bool                   `json:"noop"`
	DryRun            bool                   `json:"dry_run"`
	NonTerminalTasks  []string               `json:"non_terminal_tasks,omitempty"`
	EnsuredTasks      []workflowEnsureJSON   `json:"ensured_tasks,omitempty"`
	LeftoverWorktrees []workflowLeftoverJSON `json:"leftover_worktrees,omitempty"`
	RolledBackEnsures []workflowEnsureJSON   `json:"rolled_back_ensures,omitempty"`
	FailedTask        string                 `json:"failed_task,omitempty"`
}

type workflowEnsureJSON struct {
	TaskID   string `json:"task_id"`
	Path     string `json:"path"`
	Branch   string `json:"branch"`
	Existing bool   `json:"existing"`
}

type workflowLeftoverJSON struct {
	TaskID string `json:"task_id"`
	Path   string `json:"path"`
}

func toWorkflowUpdateJSON(rep workflow.UpdateIsolationReport) workflowUpdateJSON {
	out := workflowUpdateJSON{
		Workflow:         rep.Workflow,
		From:             string(rep.From),
		To:               string(rep.To),
		Noop:             rep.Noop,
		DryRun:           rep.DryRun,
		NonTerminalTasks: rep.NonTerminalTasks,
		FailedTask:       rep.FailedTask,
	}
	for _, e := range rep.EnsuredTasks {
		out.EnsuredTasks = append(out.EnsuredTasks, workflowEnsureJSON{
			TaskID: e.TaskID, Path: e.Path, Branch: e.Branch, Existing: e.Existing,
		})
	}
	for _, l := range rep.LeftoverWorktrees {
		out.LeftoverWorktrees = append(out.LeftoverWorktrees, workflowLeftoverJSON{
			TaskID: l.TaskID, Path: l.Path,
		})
	}
	for _, e := range rep.RolledBackEnsures {
		out.RolledBackEnsures = append(out.RolledBackEnsures, workflowEnsureJSON{
			TaskID: e.TaskID, Path: e.Path, Branch: e.Branch, Existing: e.Existing,
		})
	}
	return out
}

// emitWorkflowUpdateReport prints the report. Text and JSON branches
// surface the same fields with the same semantics.
func emitWorkflowUpdateReport(rep workflow.UpdateIsolationReport) error {
	if flagJSON {
		return json.NewEncoder(os.Stdout).Encode(toWorkflowUpdateJSON(rep))
	}
	if flagQuiet {
		return nil
	}
	if rep.Noop {
		fmt.Printf("workflow %s: isolation already %s (no-op)\n", rep.Workflow, rep.To)
		return nil
	}
	prefix := ""
	if rep.DryRun {
		prefix = "DRY-RUN: "
	}
	fmt.Printf("%sworkflow %s: isolation %s → %s\n", prefix, rep.Workflow, rep.From, rep.To)
	if len(rep.EnsuredTasks) > 0 {
		fmt.Println("ensured worktrees:")
		for _, e := range rep.EnsuredTasks {
			state := "new"
			if e.Existing {
				state = "existing"
			}
			fmt.Printf("  %s  %s  (branch=%s, %s)\n", e.TaskID, e.Path, e.Branch, state)
		}
	}
	if len(rep.LeftoverWorktrees) > 0 {
		fmt.Println("leftover worktree (not removed):")
		for _, l := range rep.LeftoverWorktrees {
			fmt.Printf("  %s  %s\n", l.TaskID, l.Path)
		}
		fmt.Println("  (run `autosk worktree rm <task-id>` once you have salvaged uncommitted state)")
	}
	return nil
}

// renderWorkflowUpdateError emits structured details on the error
// paths so the operator sees the offending task ids (refusal) or the
// rolled-back set (mid-run Ensure failure) without having to re-run
// with --json.
//
// Every branch preserves the sentinel via %w so callers higher up
// (integration tests, future daemon wrappers) can still pattern-match
// with errors.Is(err, workflow.ErrNonTerminalTasks) on the cobra
// surface. errors.New() unwraps to nothing and would silently break
// that contract.
func renderWorkflowUpdateError(err error, rep workflow.UpdateIsolationReport) error {
	switch {
	case errors.Is(err, workflow.ErrNonTerminalTasks):
		var b strings.Builder
		for _, id := range rep.NonTerminalTasks {
			fmt.Fprintf(&b, "\nnon-terminal task in workflow: %s", id)
		}
		// The sentinel's own message already includes a `pass
		// --force to update` hint; we don't append a second copy.
		return fmt.Errorf("%w%s", err, b.String())
	case errors.Is(err, workflow.ErrEnsureFailed):
		var b strings.Builder
		if rep.FailedTask != "" {
			fmt.Fprintf(&b, "\nfailed task: %s", rep.FailedTask)
		}
		if len(rep.RolledBackEnsures) > 0 {
			b.WriteString("\nrolled back:")
			for _, e := range rep.RolledBackEnsures {
				fmt.Fprintf(&b, "\n  %s  %s", e.TaskID, e.Path)
			}
		}
		b.WriteString("\n(workflows.isolation left unchanged)")
		return fmt.Errorf("%w%s", err, b.String())
	case errors.Is(err, workflow.ErrSyntheticImmutable):
		return fmt.Errorf("%w %q (single:<agent> rows are pinned to isolation=none)", err, rep.Workflow)
	case errors.Is(err, workflow.ErrNotFound):
		return fmt.Errorf("%w: %s", err, rep.Workflow)
	}
	return err
}

func newWorkflowCreateCmd() *cobra.Command {
	var (
		file      string
		noInstall bool
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a workflow from a JSON definition file",
		Long: "Create a workflow from a JSON definition file.\n\n" +
			"Scoped npm-style agent names (e.g. @scope/name) that aren't yet\n" +
			"installed are auto-installed from the registry before the workflow\n" +
			"is created (same as `autosk agent install <name>`). Use\n" +
			"--no-install to disable this and fail on any missing agent instead.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if file == "" {
				return errors.New("--file is required")
			}
			def, err := workflow.ParseFile(file)
			if err != nil {
				return err
			}
			wf, dl, closeFn, err := workflowStoreFromCmd(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()

			if !noInstall {
				if _, err := autoInstallMissingAgents(cmd.Context(), def, wf.Agents(), dl); err != nil {
					return fmt.Errorf("%w (pass --no-install to skip)", err)
				}
			}

			w, err := wf.Create(cmd.Context(), def, false /*isSynthetic*/)
			if err != nil {
				if errors.Is(err, workflow.ErrAlreadyExist) {
					return fmt.Errorf("workflow already exists: %s", def.Name)
				}
				return err
			}
			_ = dl.DoltCommit(cmd.Context(), "workflow create "+w.Name)
			return emitWorkflow(w, false /*withSteps*/)
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "path to workflow JSON definition")
	cmd.Flags().BoolVar(&noInstall, "no-install", false, "do not auto-install missing scoped-npm agents; fail validation instead")
	return cmd
}

// autoInstallMissingAgents scans def.Steps for agents not present in the
// DB and installs the ones whose names look like scoped npm packages
// (`@scope/name`). Bare names are skipped: we can't safely guess that
// `developer` on the public npm registry is the same agent the workflow
// author had in mind, so those still produce the standard validation
// error.
//
// On a successful install the matching `agents` row is created so the
// downstream workflow validation sees a real agent.
//
// The function returns the slice of `pkgregistry.Entry` rows it just
// installed so callers can surface them in their own success output
// (used by `autosk init`'s bootstrap line). Empty slice + nil error
// means every referenced agent was already present.
//
// The function is a no-op (returns nil) when every agent is either
// `human` or already in the DB.
func autoInstallMissingAgents(ctx context.Context, def workflow.Definition, ag *agent.Store, dl *doltlite.Store) ([]pkgregistry.Entry, error) {
	todo, err := missingScopedWorkflowAgents(ctx, def, ag)
	if err != nil {
		return nil, err
	}
	if len(todo) == 0 {
		return nil, nil
	}

	reg, err := openPackagesRegistry()
	if err != nil {
		return nil, fmt.Errorf("open packages registry: %w", err)
	}
	if err := reg.EnsurePrefix(); err != nil {
		return nil, fmt.Errorf("ensure packages prefix: %w", err)
	}

	// Attach the registry to the agent store so EnsureByName accepts the
	// newly-installed names. Re-uses the same *sql.DB handle.
	agWithResolver := agent.New(dl.DB()).WithResolver(reg)

	if !flagQuiet && !flagJSON {
		fmt.Fprintf(os.Stderr, "workflow references %d uninstalled agent(s); installing: %s\n",
			len(todo), strings.Join(todo, ", "))
	}

	installed := make([]pkgregistry.Entry, 0, len(todo))
	for _, name := range todo {
		if !flagQuiet && !flagJSON {
			fmt.Fprintf(os.Stderr, "\u2192 agent install %s\n", name)
		}
		entry, ierr := reg.Install(ctx, name, "")
		if ierr != nil {
			// The helper deliberately does NOT mention --no-install
			// because not every caller has that flag (e.g. `autosk init`
			// uses --skip-bootstrap instead). Each caller wraps this
			// error with its own contextual flag hint.
			return installed, fmt.Errorf("auto-install %s failed: %w (install manually with `autosk agent install %s`)",
				name, ierr, name)
		}
		if cfg, rerr := reg.Resolve(entry.Name); rerr == nil && cfg.Runner != "" {
			if err := reg.EnsureRuntime(ctx, ""); err != nil {
				return installed, fmt.Errorf("install runtime for custom-runner agent %s: %w", entry.Name, err)
			}
		}
		if _, eerr := agWithResolver.EnsureByName(ctx, entry.Name); eerr != nil {
			return installed, fmt.Errorf("register %s in agents table: %w", entry.Name, eerr)
		}
		if !flagQuiet && !flagJSON {
			fmt.Fprintf(os.Stderr, "  installed %s@%s\n", entry.Name, entry.Version)
		}
		installed = append(installed, entry)
	}
	return installed, nil
}

func missingScopedWorkflowAgents(ctx context.Context, def workflow.Definition, ag *agent.Store) ([]string, error) {
	seen := make(map[string]struct{}, len(def.Steps))
	for _, s := range def.Steps {
		if s.AgentName == "" {
			continue
		}
		seen[s.AgentName] = struct{}{}
	}
	var todo []string
	for name := range seen {
		if name == agent.HumanAgentName || !looksLikeScopedNpmName(name) {
			continue
		}
		if _, err := ag.GetByName(ctx, name); err == nil {
			continue
		} else if !errors.Is(err, agent.ErrNotFound) {
			return nil, fmt.Errorf("check agent %s: %w", name, err)
		}
		todo = append(todo, name)
	}
	sort.Strings(todo)
	return todo, nil
}

func registerPreinstalledWorkflowAgents(ctx context.Context, def workflow.Definition, ag *agent.Store, dl *doltlite.Store, reg *pkgregistry.Registry) error {
	missing, err := missingScopedWorkflowAgents(ctx, def, ag)
	if err != nil {
		return err
	}
	if len(missing) == 0 {
		return nil
	}
	var absent []string
	for _, name := range missing {
		if cfg, err := reg.Resolve(name); err != nil {
			if errors.Is(err, pkgregistry.ErrNotInstalled) {
				absent = append(absent, name)
				continue
			}
			return fmt.Errorf("resolve preinstalled agent %s: %w", name, err)
		} else if cfg.Runner != "" {
			if err := ensureRuntimePreinstalled(reg, name); err != nil {
				return err
			}
		}
	}
	if len(absent) > 0 {
		return fmt.Errorf("--no-install: missing agent package(s): %s (install them first or omit --no-install)", strings.Join(absent, ", "))
	}

	agWithResolver := agent.New(dl.DB()).WithResolver(reg)
	for _, name := range missing {
		if _, err := agWithResolver.EnsureByName(ctx, name); err != nil {
			return fmt.Errorf("register preinstalled agent %s in agents table: %w", name, err)
		}
	}
	return nil
}

func withAutoInstallJSONSilenced(ctx context.Context, def workflow.Definition, ag *agent.Store, dl *doltlite.Store) ([]pkgregistry.Entry, error) {
	var installed []pkgregistry.Entry
	err := withJSONStdoutSilenced(func() error {
		var err error
		installed, err = autoInstallMissingAgents(ctx, def, ag, dl)
		return err
	})
	return installed, err
}

func ensureRuntimePreinstalled(reg *pkgregistry.Registry, agentName string) error {
	if _, err := os.Stat(filepath.Join(reg.PackageInstallDir(pkgregistry.RuntimePackageName), "package.json")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("--no-install: custom-runner agent %s requires preinstalled %s (run `autosk agent runtime install` or omit --no-install)", agentName, pkgregistry.RuntimePackageName)
		}
		return fmt.Errorf("stat %s runtime package: %w", pkgregistry.RuntimePackageName, err)
	}
	return nil
}

// looksLikeScopedNpmName reports whether s is an npm name with a
// leading `@scope/` prefix. We only auto-install scoped names because
// bare names (`developer`, `code-reviewer`) collide too easily with
// arbitrary public npm packages.
func looksLikeScopedNpmName(s string) bool {
	if len(s) < 3 || s[0] != '@' {
		return false
	}
	slash := strings.IndexByte(s, '/')
	return slash > 1 && slash < len(s)-1
}

func newWorkflowInstallCmd() *cobra.Command {
	var (
		version      string
		workflowName string
		noInstall    bool
	)
	cmd := &cobra.Command{
		Use:   "install <npm-name-or-path>",
		Short: "Install a workflow from an npm-style package",
		Long: "Installs a package from the npm registry or a local package path,\n" +
			"selects one workflow declared under package.json autosk.workflows,\n" +
			"and creates it in the current project's workflow DB.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			arg := strings.TrimSpace(args[0])
			reg, err := openPackagesRegistry()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if err := reg.EnsurePrefix(); err != nil {
				return err
			}

			name, spec, err := resolveInstallSpec(arg, version)
			if err != nil {
				return err
			}
			var entry pkgregistry.Entry
			if noInstall {
				entry, err = reg.Get(name)
				if err != nil {
					if errors.Is(err, pkgregistry.ErrNotInstalled) {
						return fmt.Errorf("package not installed: %s (remove --no-install to install it)", name)
					}
					return err
				}
			} else {
				err = withJSONStdoutSilenced(func() error {
					var ierr error
					entry, ierr = reg.InstallWorkflowSpec(ctx, name, spec)
					return ierr
				})
				if err != nil {
					return err
				}
			}

			workflowConfigs, err := reg.ResolveWorkflows(entry.Name)
			if err != nil {
				return err
			}
			selected, err := selectPackageWorkflow(workflowConfigs, workflowName)
			if err != nil {
				return err
			}
			if _, err := os.Stat(selected.File); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("workflow file missing: %s", selected.File)
				}
				return fmt.Errorf("stat workflow file %s: %w", selected.File, err)
			}
			def, err := workflow.ParseFile(selected.File)
			if err != nil {
				return err
			}
			if def.Name != selected.Name {
				return fmt.Errorf("workflow manifest name %q does not match workflow file name %q", selected.Name, def.Name)
			}

			wf, dl, closeFn, err := workflowStoreFromCmd(ctx, true)
			if err != nil {
				return err
			}
			defer closeFn()

			var registeredAgent *agent.Agent
			if cfg, ok, err := reg.ResolveAgentIfPresent(entry.Name); err != nil {
				return err
			} else if ok {
				if cfg.Runner != "" {
					if noInstall {
						if err := ensureRuntimePreinstalled(reg, entry.Name); err != nil {
							return err
						}
					} else {
						if err := withJSONStdoutSilenced(func() error { return reg.EnsureRuntime(ctx, "") }); err != nil {
							return fmt.Errorf("install runtime for custom-runner agent %s: %w", entry.Name, err)
						}
					}
				}
				agWithResolver := agent.New(dl.DB()).WithResolver(reg)
				created, err := agWithResolver.EnsureByName(ctx, entry.Name)
				if err != nil {
					return fmt.Errorf("ensure DB row for %s: %w", entry.Name, err)
				}
				registeredAgent = &created
			}

			var installedAgents []pkgregistry.Entry
			if noInstall {
				if err := registerPreinstalledWorkflowAgents(ctx, def, wf.Agents(), dl, reg); err != nil {
					return err
				}
			} else {
				installedAgents, err = withAutoInstallJSONSilenced(ctx, def, wf.Agents(), dl)
				if err != nil {
					return err
				}
			}
			w, err := wf.Create(ctx, def, false /*isSynthetic*/)
			if err != nil {
				if errors.Is(err, workflow.ErrAlreadyExist) {
					return fmt.Errorf("workflow already exists: %s", def.Name)
				}
				return err
			}
			_ = dl.DoltCommit(ctx, "workflow install "+w.Name+" from "+entry.Name+"@"+entry.Version)
			return emitWorkflowInstall(entry, selected, w, registeredAgent, installedAgents)
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "npm version spec (default: latest)")
	cmd.Flags().StringVar(&workflowName, "workflow", "", "workflow name to install when the package declares multiple workflows")
	cmd.Flags().BoolVar(&noInstall, "no-install", false, "use an already-installed package only; do not run npm install")
	return cmd
}

func selectPackageWorkflow(workflows []pkgregistry.WorkflowConfig, name string) (pkgregistry.WorkflowConfig, error) {
	if len(workflows) == 0 {
		return pkgregistry.WorkflowConfig{}, errors.New("package declares no workflows")
	}
	if name == "" {
		if len(workflows) == 1 {
			return workflows[0], nil
		}
		names := make([]string, 0, len(workflows))
		for _, wf := range workflows {
			names = append(names, wf.Name)
		}
		sort.Strings(names)
		return pkgregistry.WorkflowConfig{}, fmt.Errorf("package declares multiple workflows (%s); pass --workflow NAME", strings.Join(names, ", "))
	}
	for _, wf := range workflows {
		if wf.Name == name {
			return wf, nil
		}
	}
	names := make([]string, 0, len(workflows))
	for _, wf := range workflows {
		names = append(names, wf.Name)
	}
	sort.Strings(names)
	return pkgregistry.WorkflowConfig{}, fmt.Errorf("workflow %q not found in package (available: %s)", name, strings.Join(names, ", "))
}

type workflowInstallJSON struct {
	Package             entryJSON    `json:"package"`
	Workflow            workflowJSON `json:"workflow"`
	WorkflowFile        string       `json:"workflow_file"`
	RegisteredAgent     *agentJSON   `json:"registered_agent,omitempty"`
	AutoInstalledAgents []entryJSON  `json:"auto_installed_agents,omitempty"`
}

func emitWorkflowInstall(entry pkgregistry.Entry, selected pkgregistry.WorkflowConfig, w workflow.Workflow, registeredAgent *agent.Agent, installedAgents []pkgregistry.Entry) error {
	if flagQuiet {
		return nil
	}
	if flagJSON {
		out := workflowInstallJSON{
			Package:      entryToJSON(entry),
			Workflow:     toWorkflowJSON(w, false),
			WorkflowFile: selected.File,
		}
		if registeredAgent != nil {
			agentJSON := toJSON(*registeredAgent)
			out.RegisteredAgent = &agentJSON
		}
		for _, installed := range installedAgents {
			out.AutoInstalledAgents = append(out.AutoInstalledAgents, entryToJSON(installed))
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	fmt.Printf("installed workflow %s from %s@%s\n", w.Name, entry.Name, entry.Version)
	fmt.Printf("workflow_file: %s\n", filepath.Clean(selected.File))
	if registeredAgent != nil {
		fmt.Printf("agent_id:      %s\n", registeredAgent.ID)
	}
	if len(installedAgents) > 0 {
		names := make([]string, 0, len(installedAgents))
		for _, installed := range installedAgents {
			names = append(names, installed.Name+"@"+installed.Version)
		}
		fmt.Printf("auto_agents:   %s\n", strings.Join(names, ", "))
	}
	return nil
}

func newWorkflowSyncCmd() *cobra.Command {
	var (
		force  bool
		dryRun bool
	)
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Materialize enabled global workflows into this project",
		Long: "Materialize enabled workflows from the global workflow registry into the current project.\n\n" +
			"Local workflows with the same name but no matching global origin are reported as conflicts\n" +
			"and are never overwritten. Changed global-managed workflows are skipped unless --force\n" +
			"is passed. Use --dry-run to preview changes without installing agents or writing the DB.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := globalworkflow.Default()
			if err != nil {
				return err
			}
			wf, dl, closeFn, err := workflowStoreFromCmd(cmd.Context(), !dryRun)
			if err != nil {
				return err
			}
			defer closeFn()

			report, err := globalworkflow.SyncGlobalWorkflows(cmd.Context(), reg, wf, globalworkflow.SyncOptions{
				DryRun: dryRun,
				Force:  force,
				InstallAgents: func(ctx context.Context, def workflow.Definition) ([]pkgregistry.Entry, error) {
					return withAutoInstallJSONSilenced(ctx, def, wf.Agents(), dl)
				},
			})
			if !dryRun && report.Mutated() {
				_ = dl.DoltCommit(cmd.Context(), "workflow sync global")
			}
			if emitErr := emitWorkflowSyncReport(report); emitErr != nil {
				return emitErr
			}
			if err != nil {
				return renderWorkflowSyncError(err, report)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "replace changed global-managed workflows when safe")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview sync without installing agents or writing the project DB")
	return cmd
}

func newWorkflowListCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List workflows (synthetic single:<agent> rows hidden unless --all)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			wf, _, closeFn, err := workflowStoreFromCmd(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer closeFn()
			ws, err := wf.List(cmd.Context(), all)
			if err != nil {
				return err
			}
			return emitWorkflows(ws)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include auto-generated single:<agent> workflows")
	return cmd
}

func newWorkflowShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show one workflow with its steps and transitions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wf, _, closeFn, err := workflowStoreFromCmd(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer closeFn()
			w, err := wf.GetByName(cmd.Context(), args[0])
			if err != nil {
				if errors.Is(err, workflow.ErrNotFound) {
					return fmt.Errorf("workflow not found: %s", args[0])
				}
				return err
			}
			return emitWorkflow(w, true /*withSteps*/)
		},
	}
	return cmd
}

func newWorkflowDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a workflow (refuses if any task references it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			wf, dl, closeFn, err := workflowStoreFromCmd(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()
			if err := wf.Delete(cmd.Context(), args[0]); err != nil {
				if errors.Is(err, workflow.ErrNotFound) {
					return fmt.Errorf("workflow not found: %s", args[0])
				}
				return err
			}
			_ = dl.DoltCommit(cmd.Context(), "workflow delete "+args[0])
			if !flagQuiet {
				fmt.Printf("deleted: %s\n", args[0])
			}
			return nil
		},
	}
	return cmd
}

type workflowSyncJSON struct {
	Prefix    string                     `json:"prefix"`
	DryRun    bool                       `json:"dry_run"`
	Force     bool                       `json:"force"`
	Workflows []workflowSyncWorkflowJSON `json:"workflows"`
}

type workflowSyncWorkflowJSON struct {
	Name                string      `json:"name"`
	Status              string      `json:"status"`
	WorkflowID          string      `json:"workflow_id,omitempty"`
	DefinitionHash      string      `json:"definition_hash,omitempty"`
	PreviousHash        string      `json:"previous_hash,omitempty"`
	Revision            string      `json:"revision,omitempty"`
	Reason              string      `json:"reason,omitempty"`
	Error               string      `json:"error,omitempty"`
	AutoInstalledAgents []entryJSON `json:"auto_installed_agents,omitempty"`
}

func toWorkflowSyncJSON(rep globalworkflow.SyncReport) workflowSyncJSON {
	out := workflowSyncJSON{Prefix: rep.Prefix, DryRun: rep.DryRun, Force: rep.Force}
	for _, item := range rep.Workflows {
		wj := workflowSyncWorkflowJSON{
			Name:           item.Name,
			Status:         string(item.Status),
			WorkflowID:     item.WorkflowID,
			DefinitionHash: item.DefinitionHash,
			PreviousHash:   item.PreviousHash,
			Revision:       item.Revision,
			Reason:         item.Reason,
			Error:          item.Error,
		}
		for _, installed := range item.AutoInstalledAgents {
			wj.AutoInstalledAgents = append(wj.AutoInstalledAgents, entryToJSON(installed))
		}
		out.Workflows = append(out.Workflows, wj)
	}
	return out
}

func emitWorkflowSyncReport(rep globalworkflow.SyncReport) error {
	if flagQuiet {
		return nil
	}
	if flagJSON {
		return json.NewEncoder(os.Stdout).Encode(toWorkflowSyncJSON(rep))
	}
	prefix := ""
	if rep.DryRun {
		prefix = "dry-run: "
	}
	if len(rep.Workflows) == 0 {
		fmt.Printf("%sworkflow sync: no enabled global workflows\n", prefix)
		return nil
	}
	for _, item := range rep.Workflows {
		fmt.Printf("%sworkflow %s: %s", prefix, item.Name, item.Status)
		if item.Reason != "" {
			fmt.Printf(" (%s)", item.Reason)
		}
		if item.Error != "" {
			fmt.Printf(": %s", item.Error)
		}
		fmt.Println()
		if len(item.AutoInstalledAgents) > 0 {
			names := make([]string, 0, len(item.AutoInstalledAgents))
			for _, installed := range item.AutoInstalledAgents {
				names = append(names, installed.Name+"@"+installed.Version)
			}
			fmt.Printf("  auto_agents: %s\n", strings.Join(names, ", "))
		}
	}
	return nil
}

func renderWorkflowSyncError(err error, rep globalworkflow.SyncReport) error {
	if !errors.Is(err, globalworkflow.ErrSyncFailed) {
		return err
	}
	var parts []string
	for _, item := range rep.Workflows {
		switch item.Status {
		case globalworkflow.SyncConflict, globalworkflow.SyncError:
			msg := item.Name + ": " + string(item.Status)
			if item.Reason != "" {
				msg += " (" + item.Reason + ")"
			}
			if item.Error != "" {
				msg += ": " + item.Error
			}
			parts = append(parts, msg)
		}
	}
	if len(parts) == 0 {
		return err
	}
	return fmt.Errorf("%w: %s", err, strings.Join(parts, "; "))
}

// ---- render --------------------------------------------------------------

// workflowJSON is the JSON projection used by `workflow show/create --json`.
type workflowJSON struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	FirstStepID string     `json:"first_step_id"`
	IsSynthetic bool       `json:"is_synthetic"`
	Isolation   string     `json:"isolation"`
	CreatedAt   string     `json:"created_at"`
	Steps       []stepJSON `json:"steps,omitempty"`
}

type stepJSON struct {
	ID          string                `json:"id"`
	Name        string                `json:"name"`
	AgentID     string                `json:"agent_id"`
	AgentName   string                `json:"agent_name"`
	AgentParams *workflow.AgentParams `json:"agent_params,omitempty"`
	Transitions []transitionJSON      `json:"transitions"`
}

type transitionJSON struct {
	ID           int64  `json:"id"`
	NextStepID   string `json:"next_step_id,omitempty"`
	NextStepName string `json:"next_step_name,omitempty"`
	TaskStatus   string `json:"task_status,omitempty"`
	PromptRule   string `json:"prompt_rule"`
}

func toWorkflowJSON(w workflow.Workflow, withSteps bool) workflowJSON {
	iso := string(w.Isolation)
	if iso == "" {
		iso = string(workflow.IsolationNone)
	}
	wj := workflowJSON{
		ID:          w.ID,
		Name:        w.Name,
		Description: w.Description,
		FirstStepID: w.FirstStepID,
		IsSynthetic: w.IsSynthetic,
		Isolation:   iso,
		CreatedAt:   w.CreatedAt.Format("2006-01-02T15:04:05Z"),
	}
	if withSteps {
		for _, st := range w.Steps {
			sj := stepJSON{
				ID: st.ID, Name: st.Name, AgentID: st.AgentID, AgentName: st.AgentName,
				AgentParams: st.AgentParams,
			}
			for _, tr := range st.Transitions {
				sj.Transitions = append(sj.Transitions, transitionJSON{
					ID:           tr.ID,
					NextStepID:   tr.NextStepID,
					NextStepName: tr.NextStepName,
					TaskStatus:   tr.TaskStatus,
					PromptRule:   tr.PromptRule,
				})
			}
			wj.Steps = append(wj.Steps, sj)
		}
	}
	return wj
}

func emitWorkflow(w workflow.Workflow, withSteps bool) error {
	if flagQuiet {
		return nil
	}
	if flagJSON {
		return json.NewEncoder(os.Stdout).Encode(toWorkflowJSON(w, withSteps))
	}
	fmt.Printf("%s\n", render.BracketedRef(w.ID, w.Name))
	if w.Description != "" {
		fmt.Printf("description:  %s\n", w.Description)
	}
	// first_step: render with the step name when we can find it among
	// the loaded steps; fall back to raw id otherwise.
	var firstStepName string
	for _, st := range w.Steps {
		if st.ID == w.FirstStepID {
			firstStepName = st.Name
			break
		}
	}
	fmt.Printf("first_step:   %s\n", render.BracketedRef(w.FirstStepID, firstStepName))
	fmt.Printf("synthetic:    %t\n", w.IsSynthetic)
	if w.Isolation != "" && w.Isolation != workflow.IsolationNone {
		fmt.Printf("isolation:    %s\n", w.Isolation)
	}
	// Text output uses the operator's local TZ; the JSON form
	// (toWorkflowJSON above) keeps the RFC3339 UTC string.
	fmt.Printf("created_at:   %s\n", timeformat.FormatDateTime(w.CreatedAt))
	if withSteps && len(w.Steps) > 0 {
		fmt.Println("steps:")
		for _, st := range w.Steps {
			fmt.Printf("  %s\n", render.BracketedRef(st.ID, st.Name))
			fmt.Printf("    agent: %s\n", render.BracketedRef(st.AgentID, st.AgentName))
			renderAgentParams(st.AgentParams)
			for _, tr := range st.Transitions {
				if tr.IsTaskStatus() {
					fmt.Printf("    → task_status=%s: %s\n", tr.TaskStatus, tr.PromptRule)
				} else {
					fmt.Printf("    → step %s: %s\n", tr.NextStepName, tr.PromptRule)
				}
			}
		}
	}
	return nil
}

// renderAgentParams pretty-prints the per-step agent.params overrides
// under `agent:` in `workflow show`. Skips the long FirstMessage body
// (shows just the byte count) so the output stays scannable.
func renderAgentParams(p *workflow.AgentParams) {
	if p.IsZero() {
		return
	}
	fmt.Println("    agent_params:")
	if p.Model != nil {
		fmt.Printf("      model: %q\n", *p.Model)
	}
	if p.Thinking != nil {
		fmt.Printf("      thinking: %q\n", *p.Thinking)
	}
	if p.FirstMessage != nil {
		fmt.Printf("      first_message: <%d bytes>\n", len(*p.FirstMessage))
	}
	if p.ExtraArgs != nil {
		fmt.Printf("      extra_args: %v\n", p.ExtraArgs)
	}
	if p.PiExtensions != nil {
		fmt.Printf("      pi_extensions: %v\n", p.PiExtensions)
	}
	if p.PiSkills != nil {
		fmt.Printf("      pi_skills: %v\n", p.PiSkills)
	}
}

func emitWorkflows(ws []workflow.Workflow) error {
	if flagJSON {
		out := make([]workflowJSON, len(ws))
		for i, w := range ws {
			out[i] = toWorkflowJSON(w, false)
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	if len(ws) == 0 {
		fmt.Fprintln(os.Stderr, "(no workflows)")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tSYNTHETIC\tDESCRIPTION")
	for _, wf := range ws {
		synth := "no"
		if wf.IsSynthetic {
			synth = "yes"
		}
		desc := wf.Description
		if len(desc) > 60 {
			desc = desc[:57] + "…"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", wf.ID, wf.Name, synth, desc)
	}
	return w.Flush()
}
