package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"autosk/internal/projectdb"
	"autosk/internal/store"
	"autosk/internal/store/doltlite"
)

// commitWrite records a doltlite commit after a successful mutation. Best-effort.
// Has no effect on non-doltlite backends.
func commitWrite(ctx context.Context, s store.Store, msg string) {
	if dl, ok := s.(*doltlite.Store); ok {
		_ = dl.DoltCommit(ctx, msg)
	}
}

// projectRootFromCwd returns the absolute project root for the
// current working directory: the directory that contains .autosk/.
// Honours the same --db override / AUTOSK_DB precedence as openStore.
//
// Used by worktree-isolation callers that need the project root for
// deterministic worktree-path derivation. Returns an error when no
// .autosk/db can be located.
func projectRootFromCwd() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dbPath, err := projectdb.Resolve(cwd, flagDB)
	if err != nil {
		return "", err
	}
	absDB, err := filepath.Abs(dbPath)
	if err != nil {
		return "", err
	}
	// dbPath is .../<root>/.autosk/db; root = parent of .autosk.
	return filepath.Dir(filepath.Dir(absDB)), nil
}

// openStore resolves the DB path and opens it, running migrations.
//
// writeOK controls whether AutoInit is allowed (write commands pass
// true). When AutoInit fires, openStore also runs the same workflow
// bootstrap that `autosk init` does so a fresh project gets the
// canonical `feature-dev-generic` workflow seeded regardless of which
// verb triggered the auto-init (write verb, `autosk lazy`, ...).
//
// Bootstrap can be suppressed by setting AUTOSK_AUTOINIT_SKIP_BOOTSTRAP
// (see autoinit.go). Bootstrap failures are non-fatal: a warning is
// printed to stderr and the migrated DB is still returned.
//
// Returns the store and a close func that the caller must defer.
func openStore(ctx context.Context, writeOK bool) (store.Store, func(), error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, err
	}

	var (
		dbPath  string
		created bool
	)
	if writeOK {
		dbPath, created, err = resolveOrInitInteractive(cwd, flagDB)
	} else {
		dbPath, err = projectdb.Resolve(cwd, flagDB)
	}
	if err != nil {
		switch {
		case errors.Is(err, projectdb.ErrNotFound):
			return nil, nil, fmt.Errorf("no .autosk/db found in this directory or any parent (run `autosk init`, or run a write command to auto-init)")
		case errors.Is(err, ErrAutoInitDeclined):
			return nil, nil, fmt.Errorf("no .autosk/db: declined to create one (run `autosk init` explicitly, or point at an existing project with --db <path> / AUTOSK_DB)")
		}
		return nil, nil, err
	}

	s := doltlite.New()
	if err := s.Open(ctx, dbPath); err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	if err := s.Migrate(ctx); err != nil {
		_ = s.Close()
		return nil, nil, fmt.Errorf("migrate %s: %w", dbPath, err)
	}

	if created {
		if !flagQuiet {
			fmt.Fprintf(os.Stderr, "autosk: created %s\n", dbPath)
		}
		// Mirror `autosk init`'s post-migrate work so the auto-init
		// path leaves the same state as an explicit init:
		//   1. Ensure the global packages prefix exists (best-effort).
		//   2. Seed the bootstrap workflow (best-effort).
		//   3. Sync enabled global workflows (best-effort).
		// Steps 2 and 3 can be suppressed independently:
		//   - AUTOSK_AUTOINIT_SKIP_BOOTSTRAP skips the built-in workflow seed.
		//   - AUTOSK_AUTOINIT_SKIP_GLOBAL_WORKFLOWS skips global workflow sync.
		// These env vars cover test helpers and pipelines that want a
		// strictly minimal DB or need to avoid npm/registry touches.
		if os.Getenv(EnvAutoInitSkipBootstrap) == "" {
			if reg, perr := openPackagesRegistry(); perr == nil {
				if err := reg.EnsurePrefix(); err != nil && !flagQuiet {
					fmt.Fprintf(os.Stderr,
						"warning: could not create packages prefix at %s: %v\n",
						reg.Prefix(), err)
				}
			}
			if berr := bootstrapDefaultWorkflow(ctx, s); berr != nil {
				fmt.Fprintf(os.Stderr,
					"warning: could not bootstrap default workflow: %v (set %s=1 to suppress this step)\n",
					berr, EnvAutoInitSkipBootstrap)
			}
		}
		if os.Getenv(EnvAutoInitSkipGlobalWorkflows) == "" {
			// Suppress the sync report to stdout when the outer command
			// runs with --json, so auto-init does not break the
			// one-JSON-document contract. Warnings still land on stderr.
			if serr := syncGlobalWorkflows(ctx, s, flagJSON); serr != nil {
				fmt.Fprintf(os.Stderr,
					"warning: could not sync global workflows: %v (set %s=1 to suppress this step)\n",
					serr, EnvAutoInitSkipGlobalWorkflows)
			}
		}
	}
	return s, func() { _ = s.Close() }, nil
}
