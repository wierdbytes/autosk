package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"autosk/internal/agent"
	"autosk/internal/agent/pkgregistry"
	"autosk/internal/bootstrap"
	"autosk/internal/projectdb"
	"autosk/internal/store/doltlite"
	"autosk/internal/workflow"
)

// newInitCmd: explicit `autosk init`. Idempotent — runs migrations on an
// existing DB and seeds the default `feature-dev-generic` workflow on
// first run (see `internal/bootstrap`).
func newInitCmd() *cobra.Command {
	var (
		prefix        string
		skipBootstrap bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize an autosk database in the current directory",
		Long: `Create ./.autosk/db and apply schema migrations.

Idempotent: running on an existing project re-applies any pending migrations.

After the schema is current, init seeds the default workflow
` + "`feature-dev-generic`" + ` (canonical definition:
internal/bootstrap/feature-dev-generic.json) and auto-installs the
` + "`@autogent/generic`" + ` agent it references. The bootstrap step is
idempotent (re-running init is a no-op once the workflow row exists) and
failures are non-fatal — a warning is printed to stderr and the DB is
still left in a usable state.

Write commands auto-init the database on first use, so calling init is
optional unless you want the default workflow seeded or you want to set
a custom ID prefix (not yet implemented).

Use --skip-bootstrap to opt out of the workflow seed (useful for tests,
CI and offline machines).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if prefix != "" {
				// Reserved for when we persist config.
				return errors.New("--prefix is reserved for a future release (current default: 'as')")
			}
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			dbDir := filepath.Join(cwd, projectdb.Dir)
			if err := os.MkdirAll(dbDir, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", dbDir, err)
			}
			dbPath := filepath.Join(dbDir, projectdb.FileName)

			s := doltlite.New()
			if err := s.Open(cmd.Context(), dbPath); err != nil {
				return err
			}
			defer s.Close()
			if err := s.Migrate(cmd.Context()); err != nil {
				return err
			}
			v, _ := s.SchemaVersion(cmd.Context())

			// Eagerly create the global agent-packages prefix so subsequent
			// `autosk agent install` calls have somewhere to write to.
			// Failure here is non-fatal: the prefix is created lazily on
			// first install anyway. We just emit a warning to surface
			// permission issues early.
			if reg, perr := openPackagesRegistry(); perr == nil {
				if err := reg.EnsurePrefix(); err != nil && !flagQuiet {
					fmt.Fprintf(os.Stderr, "warning: could not create packages prefix at %s: %v\n", reg.Prefix(), err)
				}
			}

			if !flagQuiet {
				fmt.Printf("initialized %s (schema_version=%d)\n", dbPath, v)
			}

			// Seed the default workflow. Failures here are non-fatal:
			// a warning lands on stderr and the DB is still a usable
			// migrated project root (mirror of the EnsurePrefix block
			// above). The warning is emitted regardless of --quiet —
			// quiet only suppresses informational stdout, not the
			// stderr signal that bootstrap silently did not happen.
			if !skipBootstrap {
				if err := bootstrapDefaultWorkflow(cmd.Context(), s); err != nil {
					fmt.Fprintf(os.Stderr,
						"warning: could not bootstrap default workflow: %v (re-run with --skip-bootstrap to opt out of the seed)\n",
						err)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&prefix, "prefix", "", "ID prefix (reserved; currently always 'as')")
	cmd.Flags().BoolVar(&skipBootstrap, "skip-bootstrap", false,
		"do not seed the default feature-dev-generic workflow")
	return cmd
}

// bootstrapDefaultWorkflow seeds the canonical `feature-dev-generic`
// workflow (definition lives at internal/bootstrap/feature-dev-generic.json).
//
// Behaviour is intentionally idempotent for unmanaged same-name rows and
// unchanged managed rows: those cases are silent no-ops (one informational
// line at non-quiet verbosity). When the existing managed row has a different
// canonical hash, SyncManagedDefinition installs a safe new revision while
// preserving referenced old rows. Otherwise this function:
//
//  1. Auto-installs @autogent/generic via the same path used by
//     `autosk workflow create --file` (only when create/sync is needed).
//  2. Creates or revises the workflow row + its steps + transitions.
//  3. Commits the change to doltlite with a dedicated message.
//
// Callers wrap the error in a warning; this function does not print on
// the happy path beyond the "bootstrapped" / "already present" line.
func bootstrapDefaultWorkflow(ctx context.Context, s *doltlite.Store) error {
	def, err := bootstrap.FeatureDevGeneric()
	if err != nil {
		return fmt.Errorf("load embedded definition: %w", err)
	}
	wf := workflow.New(s.DB(), agent.New(s.DB()))
	origin := workflow.Origin{
		SourceType:     "bootstrap",
		Source:         "internal/bootstrap/feature-dev-generic.json",
		SourceMetadata: map[string]any{"workflow": def.Name},
		Revision:       "embedded",
	}

	existing, err := wf.GetByName(ctx, def.Name)
	if err == nil {
		existingOrigin, oerr := wf.GetOrigin(ctx, existing.ID)
		if errors.Is(oerr, workflow.ErrNotFound) {
			if !flagQuiet {
				fmt.Printf("bootstrap: workflow %q already present, skipping\n", def.Name)
			}
			return nil
		}
		if oerr != nil {
			return fmt.Errorf("read workflow origin: %w", oerr)
		}
		hash, herr := workflow.HashDefinition(def)
		if herr != nil {
			return fmt.Errorf("hash embedded definition: %w", herr)
		}
		if existingOrigin.DefinitionHash == hash {
			if !flagQuiet {
				fmt.Printf("bootstrap: workflow %q already present, skipping\n", def.Name)
			}
			return nil
		}
	} else if !errors.Is(err, workflow.ErrNotFound) {
		return err
	}

	// Auto-install any scoped npm agents the bootstrap references. The
	// helper short-circuits cleanly when the agent is already in the
	// DB (re-runs of init after the user manually installed the agent
	// then deleted the workflow row). Existing unmanaged/name-only and
	// managed no-op rows return above so init does not touch the package
	// registry just to discover SyncManagedDefinition will skip.
	installed, err := autoInstallMissingAgents(ctx, def, wf.Agents(), s)
	if err != nil {
		return err
	}

	rep, err := wf.SyncManagedDefinition(ctx, def, origin)
	if err != nil {
		// Belt-and-suspenders against a TOCTOU race with a parallel writer
		// (two `autosk init` processes on a fresh DB, the daemon racing a
		// manual init, etc.).
		if errors.Is(err, workflow.ErrAlreadyExist) {
			if !flagQuiet {
				fmt.Printf("bootstrap: workflow %q already present, skipping\n", def.Name)
			}
			return nil
		}
		return fmt.Errorf("sync workflow: %w", err)
	}
	w := rep.Workflow
	if rep.Noop {
		if !flagQuiet {
			fmt.Printf("bootstrap: workflow %q already present, skipping\n", def.Name)
		}
		return nil
	}
	_ = s.DoltCommit(ctx, "init: bootstrap "+w.Name+" workflow")

	if !flagQuiet {
		// Format `(agent @scope/name@version installed[, ...])` so the
		// success line matches plan §4.6. We only list agents we
		// actually installed in this call; a re-run that re-creates a
		// previously-deleted workflow but finds the agent already in
		// the DB will print the bare "bootstrapped workflow X" form.
		agentSuffix := formatInstalledAgentSuffix(installed)
		if agentSuffix != "" {
			fmt.Printf("bootstrapped workflow %s %s\n", w.Name, agentSuffix)
		} else {
			fmt.Printf("bootstrapped workflow %s\n", w.Name)
		}
	}
	return nil
}

// formatInstalledAgentSuffix renders the parenthesised suffix appended
// to the bootstrap success line, matching plan §4.6:
//
//	bootstrapped workflow feature-dev-generic (agent @autogent/generic@0.1.0 installed)
//
// Returns the empty string when nothing was installed (the bare
// "bootstrapped workflow X" form is used in that case).
func formatInstalledAgentSuffix(installed []pkgregistry.Entry) string {
	if len(installed) == 0 {
		return ""
	}
	parts := make([]string, len(installed))
	for i, e := range installed {
		parts[i] = e.Name + "@" + e.Version
	}
	label := "agent"
	if len(installed) > 1 {
		label = "agents"
	}
	return "(" + label + " " + strings.Join(parts, ", ") + " installed)"
}

// newMigrateCmd: explicit migrate (also runs implicitly on every Open).
func newMigrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply any pending schema migrations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, closeFn, err := openStore(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer closeFn()
			// Migrate already ran inside openStore — report current version.
			v, err := s.SchemaVersion(cmd.Context())
			if err != nil {
				return err
			}
			if !flagQuiet {
				fmt.Printf("schema_version=%d\n", v)
			}
			return nil
		},
	}
}

// newHistoryCmd: stub. Reserves the verb until doltlite log integration lands.
func newHistoryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "history <id>",
		Short: "Show change history for a task (stub — coming in v0.2)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(os.Stderr, "history: delegates to doltlite log; planned for v0.2")
			return errSilentExit1
		},
	}
}
