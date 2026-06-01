package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"autosk/internal/agent"
	"autosk/internal/agent/pkgregistry"
	"autosk/internal/render"
	"autosk/internal/store/doltlite"
	"autosk/internal/timeformat"
)

// envAgentName is the env var that names the agent invoking the CLI.
// Defaults to "human" when unset.
const envAgentName = "AUTOSK_AGENT"

// callerAgentName returns the name of the agent the CLI is running as.
// "human" is the default.
func callerAgentName() string {
	name := strings.TrimSpace(os.Getenv(envAgentName))
	if name == "" {
		return "human"
	}
	return name
}

// resolveCallerAgent ensures the caller's agent row exists in the DB and
// returns it. Used by future write commands to fill author_id columns.
// Lazy semantics:
//   - If $AUTOSK_AGENT names an existing agent, return it.
//   - If it names a new one, insert (is_human=true only for "human").
//   - When unset, return the seeded `human` row.
func resolveCallerAgent(ctx context.Context, ag *agent.Store) (agent.Agent, error) {
	return ag.EnsureByName(ctx, callerAgentName())
}

// agentStoreFromCmd opens the project DB and returns an agent.Store +
// the doltlite handle (so commits can be issued) + a close func.
//
// The returned Store has the global package resolver attached, so any
// EnsureByName / Create call for a non-human name will be rejected
// unless the matching npm package has been installed via `autosk agent
// install`.
func agentStoreFromCmd(ctx context.Context, writeOK bool) (*agent.Store, *doltlite.Store, func(), error) {
	s, closeFn, err := openStore(ctx, writeOK)
	if err != nil {
		return nil, nil, nil, err
	}
	dl, ok := s.(*doltlite.Store)
	if !ok {
		closeFn()
		return nil, nil, nil, errors.New("agent CLI: doltlite store required")
	}
	reg, err := openPackagesRegistry()
	if err != nil {
		closeFn()
		return nil, nil, nil, fmt.Errorf("pkgregistry: %w", err)
	}
	return agent.New(dl.DB()).WithResolver(reg), dl, closeFn, nil
}

// agentStoreNoResolver returns an agent.Store without a resolver. Used
// for read-only verbs that just dump the agents table (list / show)
// where blocking on the global packages prefix would be heavy-handed.
func agentStoreNoResolver(ctx context.Context, writeOK bool) (*agent.Store, *doltlite.Store, func(), error) {
	s, closeFn, err := openStore(ctx, writeOK)
	if err != nil {
		return nil, nil, nil, err
	}
	dl, ok := s.(*doltlite.Store)
	if !ok {
		closeFn()
		return nil, nil, nil, errors.New("agent CLI: doltlite store required")
	}
	return agent.New(dl.DB()), dl, closeFn, nil
}

// ---- root command --------------------------------------------------------

func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Manage agents (install/uninstall/list/show npm-package agents)",
		Long: "Agents are named actors (humans or background workers) that can own\n" +
			"autosk tasks. The DB stores only id + name + is_human; executable\n" +
			"config (model, thinking, first-message prompt, optional custom runner)\n" +
			"lives in npm packages installed into ~/.autosk/packages/ via the agent\n" +
			"install command. See docs/plans/20260518-Agent-Packages.md.",
	}
	cmd.AddCommand(
		newAgentInstallCmd(),
		newAgentUninstallCmd(),
		newAgentListCmd(),
		newAgentShowCmd(),
		newAgentRuntimeCmd(),
	)
	return cmd
}

// newAgentInstallCmd: `autosk agent install <pkg-or-path> [--version SPEC]`.
//
// Installs the package into the global prefix (~/.autosk/packages by
// default), validates its `autosk.agent` block, and lazily inserts an
// agents row in this project's DB so workflows can reference it.
//
// `<pkg-or-path>` is either an npm registry name (e.g. `@autosk/dev`)
// or a local file path starting with `./`, `../`, `/` or `~/` (e.g.
// `./agents/developer`). For local installs, the registered agent name
// is read from the package's package.json `name` field; --version is
// ignored.
func newAgentInstallCmd() *cobra.Command {
	var version string
	cmd := &cobra.Command{
		Use:   "install <npm-name-or-path>",
		Short: "Install an npm package as an autosk agent",
		Long: "Installs the given npm package or local-path package into the\n" +
			"global agents prefix (~/.autosk/packages by default) and registers\n" +
			"it as an agent for workflows. The agent's name in the DB is the\n" +
			"package's package.json `name` field. Requires `npm` on PATH at\n" +
			"install time only.",
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
			entry, err := reg.InstallSpec(ctx, name, spec)
			if err != nil {
				return err
			}

			// Auto-install the Node bootstrapper iff the just-installed
			// agent actually needs it (custom runner). Standard agents
			// never spawn Node, so no reason to pay the round trip.
			cfg, _ := reg.Resolve(entry.Name)
			runtimeStatus := ""
			if cfg.Runner != "" {
				if rerr := reg.EnsureRuntime(ctx, ""); rerr != nil {
					runtimeStatus = fmt.Sprintf("\nwarning: %s is a custom-runner agent but %s could not be installed: %v\nrun `autosk agent runtime install` once it is available.", entry.Name, pkgregistry.RuntimePackageName, rerr)
				}
			}
			// Mirror into the project's agents table so workflows can FK
			// onto it.
			ag, dl, closeFn, err := agentStoreFromCmd(ctx, true)
			if err != nil {
				return err
			}
			defer closeFn()
			created, err := ag.EnsureByName(ctx, entry.Name)
			if err != nil {
				return fmt.Errorf("ensure DB row for %s: %w", entry.Name, err)
			}
			_ = dl.DoltCommit(ctx, "agent install "+entry.Name+"@"+entry.Version)

			if !flagQuiet {
				cfg, _ := reg.Resolve(entry.Name)
				kind := "standard (pi-spawn)"
				if cfg.Runner != "" {
					kind = "custom runner: " + cfg.Runner
				}
				if flagJSON {
					out := map[string]any{
						"agent":   toJSON(created),
						"package": entryToJSON(entry),
						"kind":    kind,
					}
					return json.NewEncoder(os.Stdout).Encode(out)
				}
				fmt.Printf("installed %s@%s\n", entry.Name, entry.Version)
				fmt.Printf("kind:       %s\n", kind)
				fmt.Printf("agent_id:   %s\n", created.ID)
				fmt.Printf("install:    %s\n", reg.PackageInstallDir(entry.Name))
				if runtimeStatus != "" {
					fmt.Println(runtimeStatus)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "npm version spec (default: latest)")
	return cmd
}

// newAgentUninstallCmd: `autosk agent uninstall <pkg>`.
//
// Removes the registry entry and shells `npm uninstall`. Refuses if any
// workflow step references the agent unless --force is given. Does NOT
// delete the agents row (workflows already reference it); users wanting
// a hard remove can `autosk sql --write` directly.
func newAgentUninstallCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "uninstall <npm-name>",
		Short: "Uninstall an agent package",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			reg, err := openPackagesRegistry()
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			// Refuse if a step in any workflow still references this
			// agent (unless --force).
			if !force {
				_, dl, closeFn, err := agentStoreNoResolver(ctx, false)
				if err == nil {
					defer closeFn()
					ag := agent.New(dl.DB())
					row, gerr := ag.GetByName(ctx, name)
					if gerr == nil {
						refs, refErr := countStepRefsByAgentID(ctx, dl, row.ID)
						if refErr == nil && refs > 0 {
							return fmt.Errorf("agent %s is referenced by %d workflow step(s); pass --force to uninstall anyway", name, refs)
						}
					}
				}
			}
			if err := reg.Uninstall(ctx, name); err != nil {
				return err
			}
			if !flagQuiet {
				fmt.Printf("uninstalled %s\n", name)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "uninstall even if the agent is referenced by a workflow step")
	return cmd
}

// countStepRefsByAgentID returns the number of workflow steps that
// point at the given agent_id. Used to gate uninstall.
func countStepRefsByAgentID(ctx context.Context, dl *doltlite.Store, agentID string) (int, error) {
	var n int
	err := dl.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM steps WHERE agent_id = ?`, agentID).Scan(&n)
	return n, err
}

func newAgentListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List installed agents (union of DB rows + installed packages)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ag, _, closeFn, err := agentStoreNoResolver(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer closeFn()
			dbRows, err := ag.List(cmd.Context())
			if err != nil {
				return err
			}
			reg, err := openPackagesRegistry()
			if err != nil {
				return err
			}
			var entries []pkgregistry.Entry
			if e, err := reg.List(); err == nil {
				for _, entry := range e {
					if _, err := reg.Resolve(entry.Name); err == nil {
						entries = append(entries, entry)
					}
				}
			}
			return emitAgentUnion(dbRows, entries)
		},
	}
	return cmd
}

func newAgentShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show one agent (registry + DB)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			ag, _, closeFn, err := agentStoreNoResolver(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer closeFn()
			a, dbErr := ag.GetByName(cmd.Context(), name)
			reg, regErr := openPackagesRegistry()
			var (
				entry  pkgregistry.Entry
				cfg    pkgregistry.PackageConfig
				gotPkg bool
			)
			if regErr == nil {
				if e, err := reg.Get(name); err == nil {
					if c, err := reg.Resolve(name); err == nil {
						entry = e
						cfg = c
						gotPkg = true
					}
				}
			}
			if dbErr != nil && !gotPkg {
				if errors.Is(dbErr, agent.ErrNotFound) {
					return fmt.Errorf("agent not found: %s", name)
				}
				return dbErr
			}
			return emitAgentDetailed(name, a, dbErr == nil, entry, cfg, gotPkg)
		},
	}
	return cmd
}

// ---- runtime subcommands -------------------------------------------------

func newAgentRuntimeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "runtime",
		Short: "Manage the Node bootstrapper used by custom-runner agents",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "install",
			Short: "Eagerly install " + pkgregistry.RuntimePackageName,
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				reg, err := openPackagesRegistry()
				if err != nil {
					return err
				}
				if err := reg.EnsurePrefix(); err != nil {
					return err
				}
				if err := reg.EnsureRuntime(cmd.Context(), ""); err != nil {
					return err
				}
				if !flagQuiet {
					fmt.Printf("runtime installed at %s\n", reg.PackageInstallDir(pkgregistry.RuntimePackageName))
				}
				return nil
			},
		},
	)
	return cmd
}

// ---- render --------------------------------------------------------------

// agentJSON is the JSON wire shape for `agent show/create` and as one
// element of the array returned by `agent list --json`.
type agentJSON struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	IsHuman   bool   `json:"is_human"`
	CreatedAt string `json:"created_at"`
}

func toJSON(a agent.Agent) agentJSON {
	return agentJSON{
		ID:        a.ID,
		Name:      a.Name,
		IsHuman:   a.IsHuman,
		CreatedAt: a.CreatedAt.Format("2006-01-02T15:04:05Z"),
	}
}

type entryJSON struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	InstalledAt string `json:"installed_at"`
}

func entryToJSON(e pkgregistry.Entry) entryJSON {
	return entryJSON{
		Name:        e.Name,
		Version:     e.Version,
		InstalledAt: e.InstalledAt.Format(time.RFC3339),
	}
}

func emitAgent(a agent.Agent) error {
	if flagQuiet {
		return nil
	}
	if flagJSON {
		return json.NewEncoder(os.Stdout).Encode(toJSON(a))
	}
	fmt.Printf("%s\n", render.BracketedRef(a.ID, a.Name))
	fmt.Printf("is_human:   %t\n", a.IsHuman)
	// Text output uses operator's local TZ; toJSON keeps RFC3339 UTC.
	fmt.Printf("created_at: %s\n", timeformat.FormatDateTime(a.CreatedAt))
	return nil
}

// emitAgentUnion prints the merged view of DB rows + installed packages.
//
//	NAME                  SOURCE       VERSION  IN_DB
//	human                 builtin      -        yes
//	@autosk/developer     package      0.3.1    yes
//	@autosk/orphan        package      0.1.0    no
//	@org/lostagent        db_only      -        yes
func emitAgentUnion(dbRows []agent.Agent, entries []pkgregistry.Entry) error {
	type row struct {
		Name    string `json:"name"`
		Source  string `json:"source"` // builtin|package|db_only
		Version string `json:"version,omitempty"`
		InDB    bool   `json:"in_db"`
	}
	dbSet := make(map[string]agent.Agent, len(dbRows))
	for _, a := range dbRows {
		dbSet[a.Name] = a
	}
	regSet := make(map[string]pkgregistry.Entry, len(entries))
	for _, e := range entries {
		regSet[e.Name] = e
	}

	rows := make([]row, 0, len(dbSet)+len(regSet))
	for name, a := range dbSet {
		r := row{Name: name, InDB: true}
		switch {
		case a.IsHuman || name == agent.HumanAgentName:
			r.Source = "builtin"
		default:
			if e, ok := regSet[name]; ok {
				r.Source = "package"
				r.Version = e.Version
			} else {
				r.Source = "db_only"
			}
		}
		rows = append(rows, r)
	}
	for name, e := range regSet {
		if _, ok := dbSet[name]; ok {
			continue
		}
		rows = append(rows, row{Name: name, Source: "package", Version: e.Version, InDB: false})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })

	if flagJSON {
		return json.NewEncoder(os.Stdout).Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "(no agents)")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSOURCE\tVERSION\tIN_DB")
	for _, r := range rows {
		ver := r.Version
		if ver == "" {
			ver = "-"
		}
		inDB := "no"
		if r.InDB {
			inDB = "yes"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Name, r.Source, ver, inDB)
	}
	return w.Flush()
}

func emitAgentDetailed(name string, a agent.Agent, inDB bool, e pkgregistry.Entry, cfg pkgregistry.PackageConfig, hasPkg bool) error {
	if flagJSON {
		out := map[string]any{
			"name":  name,
			"in_db": inDB,
		}
		if inDB {
			out["agent"] = toJSON(a)
		}
		if hasPkg {
			out["package"] = entryToJSON(e)
			out["runner"] = cfg.Runner
			out["install_dir"] = cfg.InstallDir
			out["model"] = cfg.Model
			out["thinking"] = cfg.Thinking
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	if flagQuiet {
		return nil
	}
	fmt.Printf("name:       %s\n", name)
	if inDB {
		fmt.Printf("agent_id:   %s\n", a.ID)
		fmt.Printf("is_human:   %t\n", a.IsHuman)
		// Text output uses operator's local TZ; toJSON keeps RFC3339 UTC.
		fmt.Printf("created_at: %s\n", timeformat.FormatDateTime(a.CreatedAt))
	} else {
		fmt.Println("(not in DB)")
	}
	if hasPkg {
		fmt.Printf("version:    %s\n", e.Version)
		fmt.Printf("install:    %s\n", cfg.InstallDir)
		if cfg.Runner != "" {
			fmt.Printf("runner:     %s\n", cfg.Runner)
		} else {
			fmt.Println("runner:     (standard pi-spawn)")
			if cfg.Model != "" {
				fmt.Printf("model:      %s\n", cfg.Model)
			}
			if cfg.Thinking != "" {
				fmt.Printf("thinking:   %s\n", cfg.Thinking)
			}
		}
	} else if name != agent.HumanAgentName {
		fmt.Println("package:    (not installed; run `autosk agent install " + name + "`)")
	}
	return nil
}
