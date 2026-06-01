package pkgregistry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is the current registry.json schema version.
const SchemaVersion = 1

// RuntimePackageName is the npm package name of the Node bootstrapper
// installed alongside agent packages.
const RuntimePackageName = "@autosk/agent-runtime"

// HumanAgentName is the reserved name of the seeded human agent. Calls
// to Resolve for this name return ErrNotInstalled without touching the
// filesystem — the human agent has no package config.
const HumanAgentName = "human"

// Sentinel errors.
var (
	// ErrNotInstalled is returned when a package isn't in registry.json.
	ErrNotInstalled = errors.New("agent_not_installed")
	// ErrPackageMalformed is returned when an installed package's
	// package.json is missing or has an invalid autosk.agent block.
	ErrPackageMalformed = errors.New("agent_package_malformed")
)

// Entry is a single row from registry.json.
type Entry struct {
	Name        string    `json:"-"` // map key in registry.json
	Version     string    `json:"version"`
	InstalledAt time.Time `json:"installed_at"`
}

// Registry is a handle on a packages prefix.
//
// All write methods serialise on a single in-memory mutex; cross-process
// concurrent install/uninstall is undefined (and unsupported).
type Registry struct {
	prefix string
	npm    NpmRunner
}

// Option configures a Registry.
type Option func(*Registry)

// WithNpm overrides the NpmRunner used for install/uninstall. Tests use
// this to inject a fake.
func WithNpm(npm NpmRunner) Option {
	return func(r *Registry) { r.npm = npm }
}

// Open returns a Registry rooted at prefix. The directory is created on
// first call.
func Open(prefix string, opts ...Option) (*Registry, error) {
	if prefix == "" {
		return nil, fmt.Errorf("pkgregistry.Open: empty prefix")
	}
	abs, err := filepath.Abs(prefix)
	if err != nil {
		return nil, fmt.Errorf("pkgregistry.Open: %w", err)
	}
	r := &Registry{prefix: abs, npm: ExecNpm{}}
	for _, o := range opts {
		o(r)
	}
	return r, nil
}

// Default returns a Registry rooted at the conventional location:
//
//	$AUTOSK_PACKAGES                                 (if set)
//	$XDG_DATA_HOME/autosk/packages                   (if XDG_DATA_HOME set)
//	$HOME/.autosk/packages                           (otherwise)
func Default(opts ...Option) (*Registry, error) {
	if p := os.Getenv("AUTOSK_PACKAGES"); p != "" {
		return Open(p, opts...)
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return Open(filepath.Join(x, "autosk", "packages"), opts...)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("pkgregistry.Default: %w", err)
	}
	return Open(filepath.Join(home, ".autosk", "packages"), opts...)
}

// Prefix returns the absolute prefix directory.
func (r *Registry) Prefix() string { return r.prefix }

// RuntimeBootstrapPath returns the absolute path of the bootstrapper
// script (dist/bootstrap.js inside the runtime package). Whether the
// file actually exists is the caller's problem; use EnsureRuntime first.
func (r *Registry) RuntimeBootstrapPath() string {
	return filepath.Join(r.prefix, "node_modules", RuntimePackageName, "dist", "bootstrap.js")
}

// EnsurePrefix creates the prefix dir + initial package.json + initial
// empty registry.json if they don't exist. Idempotent.
func (r *Registry) EnsurePrefix() error {
	if err := os.MkdirAll(r.prefix, 0o755); err != nil {
		return fmt.Errorf("mkdir prefix: %w", err)
	}
	// Skeleton package.json so npm doesn't complain about a missing
	// package on first install.
	pj := filepath.Join(r.prefix, "package.json")
	if _, err := os.Stat(pj); errors.Is(err, os.ErrNotExist) {
		body := []byte(`{
  "name": "autosk-packages",
  "version": "0.0.0",
  "private": true,
  "description": "Autosk-managed agent packages prefix. Do not edit by hand."
}
`)
		if err := os.WriteFile(pj, body, 0o644); err != nil {
			return fmt.Errorf("seed package.json: %w", err)
		}
	}
	// Empty registry.json.
	if _, err := os.Stat(r.registryPath()); errors.Is(err, os.ErrNotExist) {
		if err := writeJSON(r.registryPath(), registryFile{
			SchemaVersion: SchemaVersion,
			Agents:        map[string]Entry{},
		}); err != nil {
			return err
		}
	}
	return nil
}

// List returns all registered agent packages, sorted by name.
func (r *Registry) List() ([]Entry, error) {
	f, err := r.readRegistry()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(f.Agents))
	for name, e := range f.Agents {
		e.Name = name
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get returns one registry entry. ErrNotInstalled if absent.
func (r *Registry) Get(name string) (Entry, error) {
	f, err := r.readRegistry()
	if err != nil {
		return Entry{}, err
	}
	e, ok := f.Agents[name]
	if !ok {
		return Entry{}, fmt.Errorf("%w: %s", ErrNotInstalled, name)
	}
	e.Name = name
	return e, nil
}

// Has reports whether the named package is registered as a valid
// autosk agent package. This method is used as agent.PackageResolver;
// workflow-only packages in the registry must not satisfy agent FKs.
func (r *Registry) Has(name string) bool {
	_, err := r.Resolve(name)
	return err == nil
}

// PackageInstallDir returns the absolute install directory for an
// installed package. The directory may not exist (npm may have failed
// to lay down the files); callers should stat before use.
func (r *Registry) PackageInstallDir(name string) string {
	return filepath.Join(r.prefix, "node_modules", filepath.FromSlash(name))
}

// ---- internals ------------------------------------------------------------

func (r *Registry) registryPath() string {
	return filepath.Join(r.prefix, "registry.json")
}

type registryFile struct {
	SchemaVersion int              `json:"schema_version"`
	Agents        map[string]Entry `json:"agents"`
}

func (r *Registry) readRegistry() (registryFile, error) {
	b, err := os.ReadFile(r.registryPath())
	if errors.Is(err, os.ErrNotExist) {
		return registryFile{SchemaVersion: SchemaVersion, Agents: map[string]Entry{}}, nil
	}
	if err != nil {
		return registryFile{}, fmt.Errorf("read registry.json: %w", err)
	}
	var f registryFile
	if err := json.Unmarshal(b, &f); err != nil {
		return registryFile{}, fmt.Errorf("parse registry.json: %w", err)
	}
	if f.Agents == nil {
		f.Agents = map[string]Entry{}
	}
	if f.SchemaVersion == 0 {
		f.SchemaVersion = SchemaVersion
	}
	if f.SchemaVersion != SchemaVersion {
		return registryFile{}, fmt.Errorf("registry.json schema_version=%d (this binary expects %d)", f.SchemaVersion, SchemaVersion)
	}
	return f, nil
}

func (r *Registry) writeRegistry(f registryFile) error {
	if f.Agents == nil {
		f.Agents = map[string]Entry{}
	}
	if f.SchemaVersion == 0 {
		f.SchemaVersion = SchemaVersion
	}
	return writeJSON(r.registryPath(), f)
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir for %s: %w", path, err)
	}
	tmp := path + ".tmp"
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// trimSlash drops a leading "./" so resolve paths inside a package join
// cleanly against the install dir.
func trimSlash(p string) string {
	return strings.TrimPrefix(strings.TrimPrefix(p, "./"), "/")
}
