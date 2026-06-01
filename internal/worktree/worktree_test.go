package worktree_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"autosk/internal/worktree"
)

// gitProject initialises a fresh git repo under t.TempDir() with one
// commit so HEAD resolves. Returns the absolute project root. Skips
// the test if `git` isn't available.
func gitProject(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed; skipping worktree tests")
	}
	dir := filepath.Join(t.TempDir(), "ProjectRoot")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir project root: %v", err)
	}
	mustRun(t, dir, "git", "init", "--initial-branch=main")
	mustRun(t, dir, "git", "config", "user.email", "test@autosk.local")
	mustRun(t, dir, "git", "config", "user.name", "autosk test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	mustRun(t, dir, "git", "add", "README.md")
	mustRun(t, dir, "git", "commit", "-m", "init")
	// Resolve symlinks so test assertions match canonRoot's view.
	canon, err := filepath.EvalSymlinks(dir)
	if err != nil {
		canon = dir
	}
	return canon
}

func mustRun(t *testing.T, cwd, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = cwd
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// isolateHome points $HOME (and HOMEDRIVE / HOMEPATH on Windows) at a
// temp dir for the duration of the test so PathFor's derivation lands
// somewhere t.Cleanup will sweep.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestPathFor_DeterministicSlug(t *testing.T) {
	isolateHome(t)
	root := t.TempDir()
	canon, err := filepath.EvalSymlinks(root)
	if err != nil {
		canon = root
	}
	a, err := worktree.PathFor(canon, "ask-aaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	b, err := worktree.PathFor(canon, "ask-aaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("PathFor not deterministic: %s vs %s", a, b)
	}
	if !strings.Contains(a, ".autosk/worktrees/") {
		t.Fatalf("path does not live under ~/.autosk/worktrees: %s", a)
	}
	if !strings.HasSuffix(a, "/ask-aaaaaa") {
		t.Fatalf("path does not end with task id: %s", a)
	}
}

func TestPathFor_DifferentRootsDifferentSlugs(t *testing.T) {
	isolateHome(t)
	root1 := t.TempDir()
	root2 := t.TempDir()
	a, _ := worktree.PathFor(root1, "ask-111111")
	b, _ := worktree.PathFor(root2, "ask-111111")
	if a == b {
		t.Fatalf("expected different slugs for distinct roots: %s == %s", a, b)
	}
}

func TestBranchFor(t *testing.T) {
	if got := worktree.BranchFor("ask-bea999"); got != "autosk/ask-bea999" {
		t.Fatalf("BranchFor: %q", got)
	}
}

func TestEnsure_CreatesBranchAndWorktree(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	res, err := mgr.Ensure(ctx, root, "ask-000001", "")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Existing || res.BaseRefIgnored {
		t.Fatalf("unexpected flags on fresh Ensure: %+v", res)
	}
	if res.Path == "" || res.Branch != "autosk/ask-000001" {
		t.Fatalf("Result missing path/branch: %+v", res)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatalf("worktree directory not created: %v", err)
	}
	// Branch should now exist.
	out, err := exec.Command("git", "-C", root, "rev-parse", "--verify", "refs/heads/autosk/ask-000001").CombinedOutput()
	if err != nil {
		t.Fatalf("expected branch to exist: %v: %s", err, out)
	}
}

func TestEnsure_IdempotentSecondCall(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	if _, err := mgr.Ensure(ctx, root, "ask-000002", ""); err != nil {
		t.Fatalf("Ensure 1: %v", err)
	}
	res2, err := mgr.Ensure(ctx, root, "ask-000002", "")
	if err != nil {
		t.Fatalf("Ensure 2: %v", err)
	}
	if !res2.Existing {
		t.Fatalf("second Ensure should report Existing=true: %+v", res2)
	}
}

func TestEnsure_ReusesExistingBranch_BaseRefIgnored(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	// Allocate, then remove the worktree directory (mimicking
	// `autosk done` cleanup) so the branch survives.
	if _, err := mgr.Ensure(ctx, root, "ask-000003", ""); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if _, err := mgr.OnTerminal(ctx, root, "ask-000003"); err != nil {
		t.Fatalf("OnTerminal: %v", err)
	}
	// Re-ensure with an explicit base-ref; we should get a warning back
	// and the existing branch should be reused.
	res, err := mgr.Ensure(ctx, root, "ask-000003", "main")
	if err != nil {
		t.Fatalf("re-Ensure: %v", err)
	}
	if !res.BaseRefIgnored {
		t.Fatalf("expected BaseRefIgnored=true on re-use, got %+v", res)
	}
	if res.Existing {
		t.Fatalf("expected Existing=false after directory removal, got %+v", res)
	}
}

func TestEnsure_NonGitRepo(t *testing.T) {
	isolateHome(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	dir := t.TempDir()
	_, err := mgr.Ensure(ctx, dir, "ask-999999", "")
	if !errors.Is(err, worktree.ErrNotGitRepo) {
		t.Fatalf("want ErrNotGitRepo, got %v", err)
	}
}

func TestEnsure_BaseRefHonouredOnFreshBranch(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	// Create a second branch off main and use it as base.
	mustRun(t, root, "git", "checkout", "-b", "feature/seed")
	mustRun(t, root, "git", "commit", "--allow-empty", "-m", "seed-only")
	seedSHA := strings.TrimSpace(mustOutput(t, root, "git", "rev-parse", "HEAD"))
	mustRun(t, root, "git", "checkout", "main")

	mgr := worktree.NewManager()
	if _, err := mgr.Ensure(context.Background(), root, "ask-000004", "feature/seed"); err != nil {
		t.Fatalf("Ensure with base: %v", err)
	}
	// The new branch tip should equal feature/seed's tip.
	got := strings.TrimSpace(mustOutput(t, root, "git", "rev-parse", "refs/heads/autosk/ask-000004"))
	if got != seedSHA {
		t.Fatalf("new branch did not start at base-ref: branch=%s base=%s", got, seedSHA)
	}
}

func TestOnTerminal_Idempotent(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	if _, err := mgr.Ensure(ctx, root, "ask-000005", ""); err != nil {
		t.Fatal(err)
	}
	r1, err := mgr.OnTerminal(ctx, root, "ask-000005")
	if err != nil {
		t.Fatal(err)
	}
	if !r1.Existed {
		t.Fatalf("first OnTerminal should report Existed=true, got %+v", r1)
	}
	if _, err := os.Stat(r1.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected path removed, stat err=%v", err)
	}
	r2, err := mgr.OnTerminal(ctx, root, "ask-000005")
	if err != nil {
		t.Fatal(err)
	}
	if r2.Existed {
		t.Fatalf("second OnTerminal should be a no-op, got %+v", r2)
	}
	// Branch survives.
	if _, err := exec.Command("git", "-C", root, "rev-parse", "--verify", "refs/heads/autosk/ask-000005").CombinedOutput(); err != nil {
		t.Fatalf("branch should survive OnTerminal, lookup err=%v", err)
	}
}

func TestVerify_OK(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	if _, err := mgr.Ensure(ctx, root, "ask-000006", ""); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Verify(ctx, root, "ask-000006"); err != nil {
		t.Fatalf("Verify after Ensure should succeed: %v", err)
	}
}

func TestVerify_MissingDirectory(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	if _, err := mgr.Ensure(ctx, root, "ask-000007", ""); err != nil {
		t.Fatal(err)
	}
	path, _ := worktree.PathFor(root, "ask-000007")
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	err := mgr.Verify(ctx, root, "ask-000007")
	if !errors.Is(err, worktree.ErrWorktreeMissing) {
		t.Fatalf("want ErrWorktreeMissing, got %v", err)
	}
}

func TestEnsure_PerTaskMutex_Serialises(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	// Race N concurrent Ensure on the SAME task. Exactly one must
	// observe Existing=false (the real allocator); the rest must see
	// Existing=true. All N must succeed.
	const N = 4
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		fresh    int
		existing int
		errs     []error
	)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := mgr.Ensure(ctx, root, "ask-000010", "")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if res.Existing {
				existing++
			} else {
				fresh++
			}
		}()
	}
	wg.Wait()
	if len(errs) != 0 {
		t.Fatalf("racing Ensure returned errors: %v", errs)
	}
	if fresh != 1 || existing != N-1 {
		t.Fatalf("expected exactly one fresh allocator, got fresh=%d existing=%d", fresh, existing)
	}
}

// TestOnTerminal_RemovesDirEvenWhenGitBroken asserts that the
// directory is still reaped when the project's git state has been
// nuked (operator re-init'd or rm -rf'd .git). Without this, both
// the executor's cleanup hook and `autosk worktree rm` would leak
// the dir forever — the branch is the only thing requiring a
// working git and OnTerminal never touches it.
func TestOnTerminal_RemovesDirEvenWhenGitBroken(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	if _, err := mgr.Ensure(ctx, root, "ask-000020", ""); err != nil {
		t.Fatal(err)
	}
	path, _ := worktree.PathFor(root, "ask-000020")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pre-condition: worktree dir should exist: %v", err)
	}

	// Nuke .git — verifyGitRepo will now fail.
	if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}

	res, err := mgr.OnTerminal(ctx, root, "ask-000020")
	if err != nil {
		t.Fatalf("OnTerminal should succeed best-effort when git is broken: %v", err)
	}
	if !res.Existed {
		t.Errorf("expected Existed=true (we did reap the dir), got %+v", res)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("worktree dir should be removed even though git is broken; stat err=%v", err)
	}
}

// TestOnTerminal_NeverAllocated_GitBroken_ReportsExistedFalse asserts
// that the git-broken recovery branch only sets Result.Existed=true
// when it actually reaped a directory. Without this guard, a stat-of
// -a-nonexistent path returns nil + ErrNotExist, the (now removed)
// pathWasRemoved helper used to return true, and Existed was reported
// as true even though nothing happened. CLI rendering then printed
// "removed: <path>" on a no-op, which is a real correctness lie in
// `autosk worktree rm` on the §8.3 recovery path.
func TestOnTerminal_NeverAllocated_GitBroken_ReportsExistedFalse(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	// Nuke .git so verifyGitRepo fails immediately.
	if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}

	// Task was never allocated; OnTerminal must report Existed=false.
	res, err := mgr.OnTerminal(ctx, root, "ask-000022")
	if err != nil {
		t.Fatalf("OnTerminal: %v", err)
	}
	if res.Existed {
		t.Errorf("Existed=true reported for path that was never allocated (git broken): %+v", res)
	}
	// Path should remain absent (we never created it, and the recovery
	// branch must not have side-effects on missing paths).
	if _, err := os.Stat(res.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path should remain absent, stat err=%v", err)
	}
}

// TestVerify_StatErrorMapsToStranded asserts that any stat error other
// than ErrNotExist (e.g. EACCES on the parent directory) surfaces as
// ErrWorktreeStranded so the executor doesn't mislabel the run
// "worktree_missing" when the directory is right there.
func TestVerify_StatErrorMapsToStranded(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 000 is ineffective")
	}
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	if _, err := mgr.Ensure(ctx, root, "ask-000021", ""); err != nil {
		t.Fatal(err)
	}
	path, _ := worktree.PathFor(root, "ask-000021")
	parent := filepath.Dir(path)
	// Drop search bit on the parent so stat(path) returns EACCES.
	if err := os.Chmod(parent, 0o000); err != nil {
		t.Fatalf("chmod parent: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })

	err := mgr.Verify(ctx, root, "ask-000021")
	if err == nil {
		t.Fatal("expected Verify to error on EACCES")
	}
	if errors.Is(err, worktree.ErrWorktreeMissing) {
		t.Errorf("stat-EACCES must NOT map to ErrWorktreeMissing: %v", err)
	}
	if !errors.Is(err, worktree.ErrWorktreeStranded) {
		t.Errorf("want ErrWorktreeStranded, got %v", err)
	}
}

func TestEnsure_PathOccupied(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()

	// Pre-create the target path with some unrelated content.
	path, _ := worktree.PathFor(root, "ask-000011")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "loitering.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := mgr.Ensure(ctx, root, "ask-000011", "")
	if !errors.Is(err, worktree.ErrPathOccupied) {
		t.Fatalf("want ErrPathOccupied, got %v", err)
	}
}

func TestPathFor_CaseInsensitiveAliasSameSlug(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	alias := caseAlias(t, root)

	a, err := worktree.PathFor(root, "ask-case01")
	if err != nil {
		t.Fatal(err)
	}
	b, err := worktree.PathFor(alias, "ask-case01")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("case aliases produced different paths:\nroot:  %s\nalias: %s", a, b)
	}
}

func TestVerify_CaseInsensitiveProjectAlias(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	alias := caseAlias(t, root)
	mgr := worktree.NewManager()
	ctx := context.Background()

	if _, err := mgr.Ensure(ctx, root, "ask-case02", ""); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := mgr.Verify(ctx, alias, "ask-case02"); err != nil {
		t.Fatalf("Verify through case alias: %v", err)
	}
}

func TestEnsure_MigratesLegacyCaseAliasWorktree(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	alias := caseAlias(t, root)
	mgr := worktree.NewManager()
	ctx := context.Background()
	taskID := "ask-legacy01"
	legacy := legacyPathFor(t, alias, taskID)
	canonical, err := worktree.PathFor(root, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if legacy == canonical {
		t.Skip("legacy and canonical paths match on this filesystem")
	}
	mustRun(t, root, "git", "worktree", "add", legacy, "-b", worktree.BranchFor(taskID))

	res, err := mgr.Ensure(ctx, root, taskID, "main")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !res.Existing || !res.BaseRefIgnored {
		t.Fatalf("expected migrated existing worktree with base ref ignored, got %+v", res)
	}
	if _, err := os.Stat(canonical); err != nil {
		t.Fatalf("canonical worktree missing after migration: %v", err)
	}
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy worktree should have moved away, stat err=%v", err)
	}
}

func TestVerify_MigratesLegacyCaseAliasWorktree(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	alias := caseAlias(t, root)
	mgr := worktree.NewManager()
	ctx := context.Background()
	taskID := "ask-legacy02"
	legacy := legacyPathFor(t, alias, taskID)
	canonical, err := worktree.PathFor(root, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if legacy == canonical {
		t.Skip("legacy and canonical paths match on this filesystem")
	}
	mustRun(t, root, "git", "worktree", "add", legacy, "-b", worktree.BranchFor(taskID))

	if err := mgr.Verify(ctx, root, taskID); err != nil {
		t.Fatalf("Verify should migrate legacy worktree: %v", err)
	}
	if _, err := os.Stat(canonical); err != nil {
		t.Fatalf("canonical worktree missing after Verify migration: %v", err)
	}
}

func TestVerify_LegacyMigrationSerialisesConcurrentCalls(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	alias := caseAlias(t, root)
	mgr := worktree.NewManager()
	ctx := context.Background()
	taskID := "ask-legacy03"
	legacy := legacyPathFor(t, alias, taskID)
	canonical, err := worktree.PathFor(root, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if legacy == canonical {
		t.Skip("legacy and canonical paths match on this filesystem")
	}
	mustRun(t, root, "git", "worktree", "add", legacy, "-b", worktree.BranchFor(taskID))

	const n = 4
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- mgr.Verify(ctx, root, taskID)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Verify: %v", err)
		}
	}
	if _, err := os.Stat(canonical); err != nil {
		t.Fatalf("canonical worktree missing after concurrent Verify: %v", err)
	}
}

func TestVerify_PrunesMissingLegacyCaseAliasWorktree(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	alias := caseAlias(t, root)
	mgr := worktree.NewManager()
	ctx := context.Background()
	taskID := "ask-legacy04"
	legacy := legacyPathFor(t, alias, taskID)
	canonical, err := worktree.PathFor(root, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if legacy == canonical {
		t.Skip("legacy and canonical paths match on this filesystem")
	}
	mustRun(t, root, "git", "worktree", "add", legacy, "-b", worktree.BranchFor(taskID))
	if err := os.RemoveAll(legacy); err != nil {
		t.Fatal(err)
	}

	err = mgr.Verify(ctx, root, taskID)
	if !errors.Is(err, worktree.ErrWorktreeMissing) {
		t.Fatalf("want ErrWorktreeMissing after pruning stale legacy entry, got %v", err)
	}
	if _, err := os.Stat(canonical); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canonical worktree should not be created by Verify after stale prune, stat err=%v", err)
	}
}

func TestEnsure_DoesNotMoveExternalBranchWorktree(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()
	taskID := "ask-external01"
	external := filepath.Join(t.TempDir(), "HumanWorktree")
	mustRun(t, root, "git", "worktree", "add", external, "-b", worktree.BranchFor(taskID))

	_, err := mgr.Ensure(ctx, root, taskID, "")
	if !errors.Is(err, worktree.ErrBranchCheckedOutElsewhere) {
		t.Fatalf("want ErrBranchCheckedOutElsewhere, got %v", err)
	}
	if _, err := os.Stat(external); err != nil {
		t.Fatalf("external worktree should remain in place: %v", err)
	}
	canonical, _ := worktree.PathFor(root, taskID)
	if _, err := os.Stat(canonical); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canonical worktree should not be created, stat err=%v", err)
	}
}

func TestOnTerminal_DoesNotRemoveExternalBranchWorktree(t *testing.T) {
	isolateHome(t)
	root := gitProject(t)
	mgr := worktree.NewManager()
	ctx := context.Background()
	taskID := "ask-external02"
	external := filepath.Join(t.TempDir(), "HumanWorktree")
	mustRun(t, root, "git", "worktree", "add", external, "-b", worktree.BranchFor(taskID))

	_, err := mgr.OnTerminal(ctx, root, taskID)
	if !errors.Is(err, worktree.ErrBranchCheckedOutElsewhere) {
		t.Fatalf("want ErrBranchCheckedOutElsewhere, got %v", err)
	}
	if _, err := os.Stat(external); err != nil {
		t.Fatalf("external worktree should remain in place: %v", err)
	}
}

func caseAlias(t *testing.T, path string) string {
	t.Helper()
	base := filepath.Base(path)
	aliasBase := toggleCase(base)
	if aliasBase == base {
		t.Fatalf("test setup needs alphabetic project root basename, got %q", base)
	}
	alias := filepath.Join(filepath.Dir(path), aliasBase)
	origInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat original path: %v", err)
	}
	aliasInfo, err := os.Stat(alias)
	if err != nil || !os.SameFile(origInfo, aliasInfo) {
		t.Skipf("filesystem is case-sensitive for %q", path)
	}
	return alias
}

func toggleCase(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case 'a' <= r && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
		case 'A' <= r && r <= 'Z':
			b.WriteRune(r - 'A' + 'a')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func legacyPathFor(t *testing.T, projectRoot, taskID string) string {
	t.Helper()
	abs, err := filepath.Abs(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	canon, err := filepath.EvalSymlinks(abs)
	if err != nil {
		canon = filepath.Clean(abs)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(canon))
	slug := filepath.Base(canon) + "-" + hex.EncodeToString(sum[:4])
	return filepath.Join(home, ".autosk", "worktrees", slug, taskID)
}

func mustOutput(t *testing.T, cwd, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = cwd
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}
