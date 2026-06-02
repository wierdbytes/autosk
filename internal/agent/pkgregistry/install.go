package pkgregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// NpmRunner is the side-effect boundary between pkgregistry and npm.
// Tests substitute a fake that writes the same on-disk shape directly.
type NpmRunner interface {
	// Install runs the moral equivalent of:
	//   npm --prefix <prefix> install <spec>
	// where spec is "<name>" or "<name>@<version>".
	Install(ctx context.Context, prefix, spec string) error

	// Uninstall runs:
	//   npm --prefix <prefix> uninstall <name>
	Uninstall(ctx context.Context, prefix, name string) error
}

// ExecNpm shells out to a `npm` binary on PATH. Stdout/stderr are
// inherited so users see install progress.
type ExecNpm struct {
	// Bin overrides the npm binary. Defaults to "npm".
	Bin string
}

func (e ExecNpm) Install(ctx context.Context, prefix, spec string) error {
	bin := e.Bin
	if bin == "" {
		bin = "npm"
	}
	cmd := exec.CommandContext(ctx, bin, "--prefix", prefix, "install", "--save", "--no-audit", "--no-fund", spec)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s install %s: %w", bin, spec, err)
	}
	return nil
}

func (e ExecNpm) Uninstall(ctx context.Context, prefix, name string) error {
	bin := e.Bin
	if bin == "" {
		bin = "npm"
	}
	cmd := exec.CommandContext(ctx, bin, "--prefix", prefix, "uninstall", "--save", "--no-audit", "--no-fund", name)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s uninstall %s: %w", bin, name, err)
	}
	return nil
}

// Install registers an agent package by npm-registry name. version may
// be empty (= latest), a semver, or anything npm understands as a
// suffix after `@`.
//
// For local-file installs, see InstallSpec.
//
// Flow:
//  1. EnsurePrefix.
//  2. Shell npm install <name>@<version>.
//  3. Read back the resolved version from the installed package.json.
//  4. Validate the autosk.agent block (via Resolve).
//  5. Write/update the registry.json entry.
//
// On a Resolve failure after a successful npm install, we leave the
// node_modules artefacts in place (npm will reuse them) but do NOT add
// a registry entry. The caller can rerun install after fixing the
// upstream package.
func (r *Registry) Install(ctx context.Context, name, version string) (Entry, error) {
	spec := name
	if version != "" {
		spec = name + "@" + version
	}
	return r.InstallSpec(ctx, name, spec)
}

// InstallSpec is the lower-level install entry point. `name` is the
// canonical registry key (= package.json `name`); `spec` is whatever
// npm-style install spec describes the source (e.g. an `<name>@<ver>`
// string for the registry path, or a relative file path like
// `./agents/developer` for a local install).
//
// The package is installed under <prefix>/node_modules/<name>/, and the
// resolved version is read from that directory's package.json. The
// caller's `name` must match the on-disk `package.json#name`, otherwise
// the install is rejected (and rolled out of the registry).
func (r *Registry) InstallSpec(ctx context.Context, name, spec string) (Entry, error) {
	return r.installSpec(ctx, name, spec, func(name string) error {
		_, err := r.Resolve(name)
		return err
	})
}

// InstallWorkflowSpec installs a package that is expected to declare at
// least one autosk.workflow bundle. Unlike InstallSpec, autosk.agent is
// optional; if present it is validated so the CLI can register it in the
// project DB safely.
func (r *Registry) InstallWorkflowSpec(ctx context.Context, name, spec string) (Entry, error) {
	return r.installSpec(ctx, name, spec, func(name string) error {
		if _, err := r.ResolveWorkflows(name); err != nil {
			return err
		}
		_, _, err := r.ResolveAgentIfPresent(name)
		return err
	})
}

func (r *Registry) installSpec(ctx context.Context, name, spec string, validate func(string) error) (Entry, error) {
	if err := ValidatePkgName(name); err != nil {
		return Entry{}, err
	}
	if spec == "" {
		spec = name
	}
	if err := r.EnsurePrefix(); err != nil {
		return Entry{}, err
	}
	if err := r.npm.Install(ctx, r.prefix, spec); err != nil {
		return Entry{}, err
	}
	// Pull the resolved version off-disk.
	installed, err := readInstalledVersion(r.PackageInstallDir(name))
	if err != nil {
		return Entry{}, fmt.Errorf("%w (did %q install under a different name?)", err, spec)
	}
	entry := Entry{Name: name, Version: installed, InstalledAt: time.Now().UTC()}

	// Tentatively register so Resolve helpers can find the row.
	f, err := r.readRegistry()
	if err != nil {
		return Entry{}, err
	}
	prev, hadPrev := f.Agents[name]
	f.Agents[name] = entry
	if err := r.writeRegistry(f); err != nil {
		return Entry{}, err
	}
	if rerr := validate(name); rerr != nil {
		// Roll back the registry change (but not the npm install — npm
		// state is shared and may have other consumers).
		if hadPrev {
			f.Agents[name] = prev
		} else {
			delete(f.Agents, name)
		}
		_ = r.writeRegistry(f)
		return Entry{}, fmt.Errorf("installed %s but it failed validation: %w", name, rerr)
	}
	return entry, nil
}

// Uninstall removes an agent package from the registry and shells npm
// uninstall. Idempotent: removing an absent agent returns nil. If the
// underlying npm uninstall fails the registry entry stays put so the
// user can retry.
func (r *Registry) Uninstall(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("pkgregistry.Uninstall: empty name")
	}
	if err := r.EnsurePrefix(); err != nil {
		return err
	}
	f, err := r.readRegistry()
	if err != nil {
		return err
	}
	if _, ok := f.Agents[name]; !ok {
		return nil
	}
	if err := r.npm.Uninstall(ctx, r.prefix, name); err != nil {
		return err
	}
	delete(f.Agents, name)
	return r.writeRegistry(f)
}

// EnsureRuntime installs @autosk/agent-runtime at the prefix if it
// isn't already present. Idempotent. version may be empty for latest.
// Does NOT add a registry entry — the runtime is infrastructure, not an
// agent.
func (r *Registry) EnsureRuntime(ctx context.Context, version string) error {
	if err := r.EnsurePrefix(); err != nil {
		return err
	}
	dir := r.PackageInstallDir(RuntimePackageName)
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	spec := RuntimePackageName
	if version != "" {
		spec = RuntimePackageName + "@" + version
	}
	return r.npm.Install(ctx, r.prefix, spec)
}

// validatePkgName runs the minimum sanity checks before shelling npm.
// We don't try to fully replicate npm's validate-npm-package-name
// algorithm; npm itself will reject ill-formed names on install.
// ValidatePkgName runs the minimum sanity checks before shelling npm.
// Exported so the CLI can pre-validate local package specs.
func ValidatePkgName(name string) error {
	if name == "" {
		return fmt.Errorf("empty package name")
	}
	if name == HumanAgentName {
		return fmt.Errorf("%q is a reserved agent name (the human is not a package)", HumanAgentName)
	}
	if strings.ContainsAny(name, " \t\n\r") {
		return fmt.Errorf("invalid package name (contains whitespace): %q", name)
	}
	return nil
}

func readInstalledVersion(installDir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(installDir, "package.json"))
	if err != nil {
		return "", fmt.Errorf("read installed package.json: %w", err)
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("parse installed package.json: %w", err)
	}
	if m.Version == "" {
		return "", fmt.Errorf("installed package.json missing version field")
	}
	return m.Version, nil
}

// ReadPackageNameFromPath reads the `name` field of the package.json
// inside dir. Used by the CLI to translate a local-file install spec
// (`./agents/developer`) into the canonical registry key for the
// package being installed.
func ReadPackageNameFromPath(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return "", fmt.Errorf("read %s/package.json: %w", dir, err)
	}
	var m struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("parse %s/package.json: %w", dir, err)
	}
	if m.Name == "" {
		return "", fmt.Errorf("%s/package.json missing name field", dir)
	}
	return m.Name, nil
}
