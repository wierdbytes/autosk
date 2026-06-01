// Package worktree owns every git-and-filesystem interaction for the
// per-task worktree-isolation feature (docs/plans/20260521-Worktree-Isolation.md).
//
// Two surfaces live in this package:
//
//   - Package-level helpers PathFor / BranchFor encode the deterministic
//     mapping (canonical projectRoot, taskID) → on-disk worktree path +
//     branch name. They are pure functions; every caller that just needs
//     to compute or label a path (renderers, CLI verbs, the daemon
//     executor's cwd plumbing) goes through them directly.
//   - Manager wraps the mutating verbs (Ensure / OnTerminal / Verify) and
//     owns the per-(canonRoot, taskID) lock that serialises racing
//     callers on the same task.
//
// # Ownership pattern
//
// The daemon constructs a single Manager in projectmgr.New and shares it
// across every per-project executor, so racing Ensure/Verify/OnTerminal
// calls on the same task serialise correctly across projects in the
// same daemon process. Each CLI invocation is its own process, so the
// CLI constructs a fresh Manager ad-hoc at each call site — the in-memory
// lock has nothing to serialise against, and the underlying git
// operations are atomic relative to other git operations on the same
// repo.
//
// Branch policy
//
//   - The branch is always autosk/<taskID>. Not user-configurable; see
//     BranchFor.
//   - When Ensure runs against a missing branch it creates one off
//     baseRef (default HEAD). When the branch already exists, baseRef
//     is ignored and the returned Result.BaseRefIgnored is true so the
//     caller can warn the user.
//   - OnTerminal removes the on-disk directory but leaves the branch
//     intact — humans review / merge it manually.
package worktree

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"autosk/internal/pathcanon"
)

// Manager wraps the mutating git-worktree verbs.
//
// Pure-function helpers PathFor / BranchFor live at the package level
// rather than on this interface so that callers that only need a label
// don't have to build a Manager. The Manager exists for the lifecycle
// operations that touch disk + git AND need the per-(canonRoot, taskID)
// mutex to serialise racing callers. Tests can stub against this
// interface; production code shares one instance per process (see
// package docs).
type Manager interface {
	// Ensure allocates the worktree on disk if it doesn't already exist.
	// See package docs for the branch-existence semantics.
	Ensure(ctx context.Context, projectRoot, taskID, baseRef string) (Result, error)

	// OnTerminal removes the worktree directory for a task. The branch
	// is left alone. Idempotent.
	OnTerminal(ctx context.Context, projectRoot, taskID string) (Result, error)

	// Verify is the daemon's pre-flight check before spawning a step.
	// Returns ErrWorktreeMissing / ErrWorktreeStranded on mismatch.
	Verify(ctx context.Context, projectRoot, taskID string) error
}

// Result is the structured outcome of an Ensure / OnTerminal call.
//
// Path / Branch are always populated. The boolean flags are situational:
//
//   - Existing       — Ensure: the worktree was already on disk + tracked.
//   - BaseRefIgnored — Ensure: caller passed a baseRef but the branch
//     already existed so the value was ignored.
//   - Existed        — OnTerminal: a path was actually removed (vs the
//     no-op idempotent case).
type Result struct {
	Path           string
	Branch         string
	Existing       bool
	BaseRefIgnored bool
	Existed        bool
}

// Sentinel errors. Callers test with errors.Is — implementations may
// wrap them with extra context.
var (
	ErrNotGitRepo                = errors.New("worktree: project root is not a git repo")
	ErrGitMissing                = errors.New("worktree: git binary not found on PATH")
	ErrPathOccupied              = errors.New("worktree: target path exists and is not a registered worktree")
	ErrWorktreeMissing           = errors.New("worktree: directory missing on disk")
	ErrWorktreeStranded          = errors.New("worktree: .git does not point at the project's gitdir")
	ErrBranchCheckedOutElsewhere = errors.New("worktree: branch is checked out in a non-autosk worktree")
)

// manager is the default Manager implementation. Per-task locking
// serialises racing Ensure / OnTerminal callers on the same task.
type manager struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewManager constructs a fresh Manager.
func NewManager() Manager {
	return &manager{locks: make(map[string]*sync.Mutex)}
}

// BranchFor returns the canonical branch name for the given task id
// ("autosk/<taskID>"). Pure; safe to call without a Manager.
func BranchFor(taskID string) string {
	return "autosk/" + taskID
}

// PathFor returns the absolute on-disk path for the (projectRoot,
// taskID) pair. The path lives under $HOME/.autosk/worktrees/<slug>/
// where slug = filepath.Base(canonRoot) + "-" + 8hex(sha256(canonRoot))
// so distinct project roots with the same basename don't collide.
//
// The slug is computed against the symlink-resolved root so callers
// that pass either form get the same answer. Pure; safe to call
// without a Manager.
func PathFor(projectRoot, taskID string) (string, error) {
	if strings.TrimSpace(projectRoot) == "" {
		return "", fmt.Errorf("worktree: project root is empty")
	}
	if strings.TrimSpace(taskID) == "" {
		return "", fmt.Errorf("worktree: task id is empty")
	}
	canon, err := canonRoot(projectRoot)
	if err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("worktree: user home dir: %w", err)
	}
	return filepath.Join(home, ".autosk", "worktrees", slugFor(canon), taskID), nil
}

// Ensure allocates the worktree, creating the branch if necessary. See
// package docs for semantics.
func (m *manager) Ensure(ctx context.Context, projectRoot, taskID, baseRef string) (Result, error) {
	canon, err := canonRoot(projectRoot)
	if err != nil {
		return Result{}, err
	}
	unlock := m.lock(canon, taskID)
	defer unlock()

	if err := verifyGitAvailable(); err != nil {
		return Result{}, err
	}
	if err := verifyGitRepo(ctx, canon); err != nil {
		return Result{}, err
	}
	path, err := PathFor(canon, taskID)
	if err != nil {
		return Result{}, err
	}
	branch := BranchFor(taskID)
	res := Result{Path: path, Branch: branch}

	// Already-registered worktree at the target path → fast no-op.
	registered, err := worktreeRegisteredAt(ctx, canon, path)
	if err != nil {
		return res, err
	}
	if registered {
		res.Existing = true
		if strings.TrimSpace(baseRef) != "" {
			// We can't honour baseRef on an existing worktree; flag it.
			res.BaseRefIgnored = true
		}
		// Ensure the parent dir actually exists (defensive — should be
		// true if registered is true) and return.
		return res, nil
	}

	if legacy, found, err := worktreeRegisteredForBranch(ctx, canon, branch); err != nil {
		return res, err
	} else if found {
		if !autoskManagedWorktreePath(legacy.Path, taskID) {
			return res, fmt.Errorf("%w: %s at %s", ErrBranchCheckedOutElsewhere, branch, legacy.Path)
		}
		if _, statErr := os.Stat(legacy.Path); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				_, _ = runGit(ctx, canon, "worktree", "prune")
			} else {
				return res, fmt.Errorf("worktree: stat legacy path %s: %w", legacy.Path, statErr)
			}
		} else {
			if err := moveRegisteredWorktree(ctx, canon, legacy.Path, path); err != nil {
				return res, err
			}
			res.Existing = true
			if strings.TrimSpace(baseRef) != "" {
				res.BaseRefIgnored = true
			}
			return res, nil
		}
	}

	// Path occupied by something that isn't a registered worktree → hard
	// error. We refuse to clobber unknown state in the user's $HOME.
	if _, statErr := os.Stat(path); statErr == nil {
		return res, fmt.Errorf("%w: %s", ErrPathOccupied, path)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return res, fmt.Errorf("worktree: stat %s: %w", path, statErr)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return res, fmt.Errorf("worktree: mkdir %s: %w", filepath.Dir(path), err)
	}

	branchExists, err := branchExists(ctx, canon, branch)
	if err != nil {
		return res, err
	}
	if branchExists {
		// Reuse existing branch (typical reopen+enroll). baseRef is
		// ignored in that case; warn via the result.
		if strings.TrimSpace(baseRef) != "" {
			res.BaseRefIgnored = true
		}
		if out, gerr := runGit(ctx, canon, "worktree", "add", path, branch); gerr != nil {
			return res, gitErr("worktree add (existing branch)", gerr, out)
		}
	} else {
		args := []string{"worktree", "add", path, "-b", branch}
		if base := strings.TrimSpace(baseRef); base != "" {
			args = append(args, base)
		}
		if out, gerr := runGit(ctx, canon, args...); gerr != nil {
			return res, gitErr("worktree add (new branch)", gerr, out)
		}
	}
	return res, nil
}

// OnTerminal removes the worktree directory for a task. Idempotent: a
// missing directory + unknown-to-git path both return (Result{Existed:
// false}, nil).
//
// Failure-mode contract: when the project's git state itself is broken
// (project re-initialised, .git nuked, etc.) we still try to remove the
// on-disk directory best-effort, because the branch is the only piece
// that genuinely needs a working git and we don't touch the branch here
// anyway. Otherwise the worktree dir would leak forever, and both the
// daemon's terminal-cleanup hook and `autosk worktree rm` would fail.
func (m *manager) OnTerminal(ctx context.Context, projectRoot, taskID string) (Result, error) {
	canon, err := canonRoot(projectRoot)
	if err != nil {
		return Result{}, err
	}
	unlock := m.lock(canon, taskID)
	defer unlock()

	path, err := PathFor(canon, taskID)
	if err != nil {
		return Result{}, err
	}
	res := Result{Path: path, Branch: BranchFor(taskID)}

	if err := verifyGitAvailable(); err != nil {
		return res, err
	}
	// If git itself is broken (project re-initialised, .git nuked) we
	// still try to reap the on-disk directory. The branch is the only
	// piece that genuinely needs git, and we never touch it here.
	//
	// Stat-then-remove (rather than blind RemoveAll) so the returned
	// Result.Existed distinguishes "we did reap something" from "the
	// path was already gone". A blind RemoveAll on a missing path is
	// a silent no-op but would lie via Existed=true to downstream
	// CLI rendering (`autosk worktree rm` prints "removed: <path>").
	if err := verifyGitRepo(ctx, canon); err != nil {
		_, sErr := os.Stat(path)
		switch {
		case sErr == nil:
			if rerr := os.RemoveAll(path); rerr != nil {
				return res, fmt.Errorf("worktree: remove %s after git failure: %w", path, rerr)
			}
			res.Existed = true
		case !errors.Is(sErr, os.ErrNotExist):
			return res, fmt.Errorf("worktree: stat %s after git failure: %w", path, sErr)
		}
		return res, nil
	}

	registered, err := worktreeRegisteredAt(ctx, canon, path)
	if err != nil {
		return res, err
	}
	if registered {
		if out, gerr := runGit(ctx, canon, "worktree", "remove", "--force", path); gerr != nil {
			return res, gitErr("worktree remove", gerr, out)
		}
		res.Existed = true
		// `worktree remove` deletes the directory, but a stale parent
		// dir is fine — leave it for future tasks of the same project.
		return res, nil
	}

	if legacy, found, err := worktreeRegisteredForBranch(ctx, canon, res.Branch); err != nil {
		return res, err
	} else if found {
		if !autoskManagedWorktreePath(legacy.Path, taskID) {
			return res, fmt.Errorf("%w: %s at %s", ErrBranchCheckedOutElsewhere, res.Branch, legacy.Path)
		}
		if _, statErr := os.Stat(legacy.Path); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				_, _ = runGit(ctx, canon, "worktree", "prune")
				return res, nil
			}
			return res, fmt.Errorf("worktree: stat legacy path %s: %w", legacy.Path, statErr)
		}
		if out, gerr := runGit(ctx, canon, "worktree", "remove", "--force", legacy.Path); gerr != nil {
			return res, gitErr("worktree remove legacy", gerr, out)
		}
		res.Existed = true
		return res, nil
	}

	// Not registered. The path may still exist (e.g. user manually
	// removed the worktree from git but left files behind). Remove it
	// best-effort; absent is fine.
	if _, statErr := os.Stat(path); statErr == nil {
		if rerr := os.RemoveAll(path); rerr != nil {
			return res, fmt.Errorf("worktree: remove orphan path %s: %w", path, rerr)
		}
		res.Existed = true
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return res, fmt.Errorf("worktree: stat %s: %w", path, statErr)
	}
	// Best-effort `git worktree prune` so a stale `git worktree list`
	// entry left behind by an external `rm -rf` doesn't haunt us.
	_, _ = runGit(ctx, canon, "worktree", "prune")
	return res, nil
}

// Verify is the daemon's pre-flight check.
//
// The path must exist, must be the canonical worktree path for the
// task, and its `.git` pointer must resolve to a gitdir under the
// project root's gitdir. The latter is what catches the
// "project directory moved" failure mode.
func (m *manager) Verify(ctx context.Context, projectRoot, taskID string) error {
	canon, err := canonRoot(projectRoot)
	if err != nil {
		return err
	}
	unlock := m.lock(canon, taskID)
	defer unlock()

	path, err := PathFor(canon, taskID)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(path); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			branch := BranchFor(taskID)
			legacy, found, lerr := worktreeRegisteredForBranch(ctx, canon, branch)
			if lerr != nil {
				return lerr
			}
			if !found {
				return fmt.Errorf("%w: %s", ErrWorktreeMissing, path)
			}
			if !autoskManagedWorktreePath(legacy.Path, taskID) {
				return fmt.Errorf("%w: %s at %s", ErrBranchCheckedOutElsewhere, branch, legacy.Path)
			}
			if _, legacyStatErr := os.Stat(legacy.Path); legacyStatErr != nil {
				if errors.Is(legacyStatErr, os.ErrNotExist) {
					_, _ = runGit(ctx, canon, "worktree", "prune")
					return fmt.Errorf("%w: %s", ErrWorktreeMissing, path)
				}
				return fmt.Errorf("%w: stat %s: %v", ErrWorktreeStranded, legacy.Path, legacyStatErr)
			}
			if err := moveRegisteredWorktree(ctx, canon, legacy.Path, path); err != nil {
				return err
			}
		} else {
			// Any other stat failure (EACCES, dangling symlink, etc.) is a
			// stranded worktree from the operator's point of view: the
			// directory is *there* on disk in some form, we just can't
			// safely use it. Wrap the sentinel so the executor's mapping
			// labels the run "worktree_stranded" rather than the misleading
			// "worktree_missing".
			return fmt.Errorf("%w: stat %s: %v", ErrWorktreeStranded, path, statErr)
		}
	}
	// Resolve the worktree's gitdir.
	wtGitDir, gerr := gitCommonDirFrom(ctx, path)
	if gerr != nil {
		return fmt.Errorf("%w: %s: %v", ErrWorktreeStranded, path, gerr)
	}
	// Resolve the project's gitdir; both should match.
	projGitDir, perr := gitCommonDirFrom(ctx, canon)
	if perr != nil {
		return fmt.Errorf("%w: %s: %v", ErrNotGitRepo, canon, perr)
	}
	if !sameDir(wtGitDir, projGitDir) {
		return fmt.Errorf("%w: worktree gitdir=%s, project gitdir=%s",
			ErrWorktreeStranded, wtGitDir, projGitDir)
	}
	return nil
}

// ---- helpers --------------------------------------------------------------

func (m *manager) lock(canon, taskID string) func() {
	key := canon + "\x00" + taskID
	m.mu.Lock()
	mu, ok := m.locks[key]
	if !ok {
		mu = &sync.Mutex{}
		m.locks[key] = mu
	}
	m.mu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// canonRoot returns the symlink-resolved, absolutised project root. It
// is the load-bearing helper that keeps every caller computing the
// same slug for the same project.
func canonRoot(projectRoot string) (string, error) {
	canon, err := pathcanon.Existing(projectRoot)
	if err != nil {
		return "", fmt.Errorf("worktree: canonicalise %q: %w", projectRoot, err)
	}
	return canon, nil
}

// slugFor turns a canonical project root into the on-disk slug used as
// the parent directory under ~/.autosk/worktrees/.
func slugFor(canon string) string {
	base := filepath.Base(canon)
	sum := sha256.Sum256([]byte(canon))
	return base + "-" + hex.EncodeToString(sum[:4])
}

// verifyGitAvailable returns ErrGitMissing when `git` isn't on PATH.
func verifyGitAvailable() error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("%w: %v", ErrGitMissing, err)
	}
	return nil
}

// verifyGitRepo returns ErrNotGitRepo unless the project root is inside
// a git repo. We use --git-dir rather than --is-inside-work-tree so
// bare repos still count (although autosk is unlikely to ever live in
// one).
func verifyGitRepo(ctx context.Context, canon string) error {
	out, err := runGit(ctx, canon, "rev-parse", "--git-dir")
	if err != nil {
		return fmt.Errorf("%w: %s: %v (%s)", ErrNotGitRepo, canon, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// branchExists asks git whether refs/heads/<branch> resolves.
func branchExists(ctx context.Context, canon, branch string) (bool, error) {
	ref := "refs/heads/" + branch
	cmd := exec.CommandContext(ctx, "git", "-C", canon, "show-ref", "--verify", "--quiet", ref)
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// Exit code 1 = not found; any other code is a real error.
			if ee.ExitCode() == 1 {
				return false, nil
			}
		}
		return false, fmt.Errorf("worktree: show-ref %s: %w", branch, err)
	}
	return true, nil
}

type registeredWorktree struct {
	Path   string
	Branch string
}

// worktreeRegisteredAt parses `git worktree list --porcelain` looking
// for a `worktree <path>` line that matches the canonical target path.
func worktreeRegisteredAt(ctx context.Context, canon, target string) (bool, error) {
	entries, err := listRegisteredWorktrees(ctx, canon)
	if err != nil {
		return false, err
	}
	canonTarget, _ := pathcanon.Existing(target)
	if canonTarget == "" {
		canonTarget = filepath.Clean(target)
	}
	for _, entry := range entries {
		if sameDir(entry.Path, target) || sameDir(entry.Path, canonTarget) {
			return true, nil
		}
	}
	return false, nil
}

func worktreeRegisteredForBranch(ctx context.Context, canon, branch string) (registeredWorktree, bool, error) {
	entries, err := listRegisteredWorktrees(ctx, canon)
	if err != nil {
		return registeredWorktree{}, false, err
	}
	ref := "refs/heads/" + branch
	for _, entry := range entries {
		if entry.Branch == ref || entry.Branch == branch {
			return entry, true, nil
		}
	}
	return registeredWorktree{}, false, nil
}

func listRegisteredWorktrees(ctx context.Context, canon string) ([]registeredWorktree, error) {
	out, err := runGit(ctx, canon, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, gitErr("worktree list", err, out)
	}
	var entries []registeredWorktree
	var cur registeredWorktree
	flush := func() {
		if cur.Path != "" {
			entries = append(entries, cur)
			cur = registeredWorktree{}
		}
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "worktree ") {
			flush()
			cur.Path = strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
			continue
		}
		if strings.HasPrefix(line, "branch ") {
			cur.Branch = strings.TrimSpace(strings.TrimPrefix(line, "branch "))
		}
	}
	flush()
	return entries, nil
}

func moveRegisteredWorktree(ctx context.Context, canon, from, to string) error {
	if sameDir(from, to) {
		return nil
	}
	if _, statErr := os.Stat(to); statErr == nil {
		return fmt.Errorf("%w: %s", ErrPathOccupied, to)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("worktree: stat %s: %w", to, statErr)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return fmt.Errorf("worktree: mkdir %s: %w", filepath.Dir(to), err)
	}
	if out, gerr := runGit(ctx, canon, "worktree", "move", from, to); gerr != nil {
		return gitErr("worktree move", gerr, out)
	}
	return nil
}

func autoskManagedWorktreePath(path, taskID string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	root := filepath.Join(home, ".autosk", "worktrees")
	canonRoot, err := pathcanon.Existing(root)
	if err != nil {
		canonRoot = filepath.Clean(root)
	}
	canonPath, err := pathcanon.Existing(path)
	if err != nil {
		canonPath = filepath.Clean(path)
	}
	rel, err := filepath.Rel(canonRoot, canonPath)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	return len(parts) == 2 && parts[0] != "" && parts[1] == taskID
}

// gitCommonDirFrom asks git for the absolute common-dir of the repo
// containing `cwd`. For a worktree this is the project's main gitdir;
// for the project's main root it's the same value. We use this for
// the Verify cross-check.
func gitCommonDirFrom(ctx context.Context, cwd string) (string, error) {
	out, err := runGit(ctx, cwd, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("rev-parse --git-common-dir at %s: %w (%s)", cwd, err, strings.TrimSpace(string(out)))
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return "", fmt.Errorf("rev-parse --git-common-dir at %s: empty output", cwd)
	}
	abs := raw
	if !filepath.IsAbs(raw) {
		abs = filepath.Join(cwd, raw)
	}
	if resolved, rerr := pathcanon.Existing(abs); rerr == nil {
		abs = resolved
	}
	return filepath.Clean(abs), nil
}

// sameDir is a forgiving directory-path comparison for paths that may
// differ by symlink spelling or by case-insensitive aliases.
func sameDir(a, b string) bool {
	return pathcanon.SameDir(a, b)
}

// runGit runs `git -C <cwd> <args>` and captures combined output. Used
// for every operation in this file so error messages carry stderr.
func runGit(ctx context.Context, cwd string, args ...string) ([]byte, error) {
	all := append([]string{"-C", cwd}, args...)
	cmd := exec.CommandContext(ctx, "git", all...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.Bytes(), err
}

// gitErr wraps a `git …` failure with the captured output.
func gitErr(op string, err error, out []byte) error {
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		return fmt.Errorf("worktree: %s: %w", op, err)
	}
	return fmt.Errorf("worktree: %s: %w: %s", op, err, msg)
}
