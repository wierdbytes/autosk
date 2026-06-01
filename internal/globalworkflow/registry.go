// Package globalworkflow manages workflow definitions installed outside any
// single project database.
package globalworkflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"autosk/internal/workflow"
)

// SchemaVersion is the current registry.json schema version.
const SchemaVersion = 1

// EnvWorkflows overrides the default global workflow prefix.
const EnvWorkflows = "AUTOSK_WORKFLOWS"

// Sentinel errors.
var (
	ErrNotInstalled = errors.New("global workflow not installed")
	ErrInvalidName  = errors.New("invalid global workflow name")
)

// Entry is a single workflow row from registry.json.
type Entry struct {
	Name           string         `json:"-"` // map key in registry.json
	DefinitionHash string         `json:"definition_hash"`
	DefinitionFile string         `json:"definition_file"`
	Revision       string         `json:"revision,omitempty"`
	SourceType     string         `json:"source_type,omitempty"`
	Source         string         `json:"source,omitempty"`
	SourceMetadata map[string]any `json:"source_metadata,omitempty"`
	Enabled        bool           `json:"enabled"`
	InstalledAt    time.Time      `json:"installed_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

// StoreOptions carries provenance fields written alongside a definition.
type StoreOptions struct {
	Revision       string
	SourceType     string
	Source         string
	SourceMetadata map[string]any
	// Enabled optionally overrides the stored enabled state. Nil means enabled.
	Enabled *bool
}

// Registry is a handle on a global workflow prefix.
type Registry struct {
	prefix string
}

// Open returns a Registry rooted at prefix. The directory is created by
// EnsurePrefix, not by Open.
func Open(prefix string) (*Registry, error) {
	if strings.TrimSpace(prefix) == "" {
		return nil, fmt.Errorf("globalworkflow.Open: empty prefix")
	}
	abs, err := filepath.Abs(prefix)
	if err != nil {
		return nil, fmt.Errorf("globalworkflow.Open: %w", err)
	}
	return &Registry{prefix: abs}, nil
}

// Default returns a Registry rooted at the conventional location:
//
//	$AUTOSK_WORKFLOWS                 (if set)
//	$XDG_DATA_HOME/autosk/workflows   (if XDG_DATA_HOME set)
//	$HOME/.autosk/workflows           (otherwise)
func Default() (*Registry, error) {
	if p := os.Getenv(EnvWorkflows); p != "" {
		return Open(p)
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return Open(filepath.Join(x, "autosk", "workflows"))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("globalworkflow.Default: %w", err)
	}
	return Open(filepath.Join(home, ".autosk", "workflows"))
}

// Prefix returns the absolute prefix directory.
func (r *Registry) Prefix() string { return r.prefix }

// EnsurePrefix creates the prefix dir and initial registry.json if missing.
func (r *Registry) EnsurePrefix() error {
	if err := os.MkdirAll(r.definitionsDir(), 0o750); err != nil {
		return fmt.Errorf("mkdir definitions: %w", err)
	}
	if _, err := os.Stat(r.registryPath()); errors.Is(err, os.ErrNotExist) {
		return writeJSON(r.registryPath(), registryFile{
			SchemaVersion: SchemaVersion,
			Workflows:     map[string]Entry{},
		})
	} else if err != nil {
		return fmt.Errorf("stat registry.json: %w", err)
	}
	return nil
}

// StoreDefinition stores def's canonical JSON and upserts its registry row.
// The returned entry is enabled unless opts.Enabled explicitly points to false.
func (r *Registry) StoreDefinition(def workflow.Definition, opts StoreOptions) (Entry, error) {
	if err := validateName(def.Name); err != nil {
		return Entry{}, err
	}
	if err := r.EnsurePrefix(); err != nil {
		return Entry{}, err
	}
	body, err := workflow.CanonicalDefinitionJSON(def)
	if err != nil {
		return Entry{}, err
	}
	hash, err := workflow.HashDefinition(def)
	if err != nil {
		return Entry{}, err
	}
	defFile := filepath.Join("definitions", hash+".json")
	if err := os.WriteFile(filepath.Join(r.prefix, defFile), append(body, '\n'), 0o600); err != nil {
		return Entry{}, fmt.Errorf("write workflow definition: %w", err)
	}

	f, err := r.readRegistry()
	if err != nil {
		return Entry{}, err
	}
	now := time.Now().UTC()
	entry := f.Workflows[def.Name]
	if entry.InstalledAt.IsZero() {
		entry.InstalledAt = now
	}
	entry.DefinitionHash = hash
	entry.DefinitionFile = filepath.ToSlash(defFile)
	entry.Revision = strings.TrimSpace(opts.Revision)
	entry.SourceType = strings.TrimSpace(opts.SourceType)
	entry.Source = strings.TrimSpace(opts.Source)
	entry.SourceMetadata = cloneMetadata(opts.SourceMetadata)
	entry.Enabled = true
	if opts.Enabled != nil {
		entry.Enabled = *opts.Enabled
	}
	entry.UpdatedAt = now
	f.Workflows[def.Name] = entry
	if err := r.writeRegistry(f); err != nil {
		return Entry{}, err
	}
	entry.Name = def.Name
	return entry, nil
}

// Get returns one registry entry. ErrNotInstalled if absent.
func (r *Registry) Get(name string) (Entry, error) {
	f, err := r.readRegistry()
	if err != nil {
		return Entry{}, err
	}
	entry, ok := f.Workflows[name]
	if !ok {
		return Entry{}, fmt.Errorf("%w: %s", ErrNotInstalled, name)
	}
	entry.Name = name
	return entry, nil
}

// List returns registered workflows sorted by name. Disabled entries are
// hidden unless includeDisabled is true.
func (r *Registry) List(includeDisabled bool) ([]Entry, error) {
	f, err := r.readRegistry()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(f.Workflows))
	for name, entry := range f.Workflows {
		if !includeDisabled && !entry.Enabled {
			continue
		}
		entry.Name = name
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// LoadDefinition parses the stored canonical definition for name.
func (r *Registry) LoadDefinition(name string) (workflow.Definition, error) {
	entry, err := r.Get(name)
	if err != nil {
		return workflow.Definition{}, err
	}
	path, err := r.resolveEntryFile(entry.DefinitionFile)
	if err != nil {
		return workflow.Definition{}, err
	}
	def, err := workflow.ParseFile(path)
	if err != nil {
		return workflow.Definition{}, fmt.Errorf("parse stored workflow %s: %w", name, err)
	}
	if def.Name != name {
		return workflow.Definition{}, fmt.Errorf("stored workflow %s has definition name %q", name, def.Name)
	}
	hash, err := workflow.HashDefinition(def)
	if err != nil {
		return workflow.Definition{}, fmt.Errorf("hash stored workflow %s: %w", name, err)
	}
	if hash != entry.DefinitionHash {
		return workflow.Definition{}, fmt.Errorf("stored workflow %s hash mismatch: registry=%s file=%s", name, entry.DefinitionHash, hash)
	}
	return def, nil
}

// Enable marks an installed workflow active in the registry.
func (r *Registry) Enable(name string) (Entry, error) { return r.setEnabled(name, true) }

// Disable marks an installed workflow inactive in the registry.
func (r *Registry) Disable(name string) (Entry, error) { return r.setEnabled(name, false) }

// Remove deletes the registry row and removes its stored definition file when
// no other registry entry references the same file.
func (r *Registry) Remove(name string) error {
	f, err := r.readRegistry()
	if err != nil {
		return err
	}
	entry, ok := f.Workflows[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotInstalled, name)
	}
	delete(f.Workflows, name)
	if err := r.writeRegistry(f); err != nil {
		return err
	}
	if !definitionFileReferenced(f, entry.DefinitionFile) {
		path, err := r.resolveEntryFile(entry.DefinitionFile)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stored workflow definition: %w", err)
		}
	}
	return nil
}

func (r *Registry) setEnabled(name string, enabled bool) (Entry, error) {
	f, err := r.readRegistry()
	if err != nil {
		return Entry{}, err
	}
	entry, ok := f.Workflows[name]
	if !ok {
		return Entry{}, fmt.Errorf("%w: %s", ErrNotInstalled, name)
	}
	entry.Enabled = enabled
	entry.UpdatedAt = time.Now().UTC()
	f.Workflows[name] = entry
	if err := r.writeRegistry(f); err != nil {
		return Entry{}, err
	}
	entry.Name = name
	return entry, nil
}

func (r *Registry) registryPath() string { return filepath.Join(r.prefix, "registry.json") }
func (r *Registry) definitionsDir() string {
	return filepath.Join(r.prefix, "definitions")
}

type registryFile struct {
	SchemaVersion int              `json:"schema_version"`
	Workflows     map[string]Entry `json:"workflows"`
}

func (r *Registry) readRegistry() (registryFile, error) {
	b, err := os.ReadFile(r.registryPath())
	if errors.Is(err, os.ErrNotExist) {
		return registryFile{SchemaVersion: SchemaVersion, Workflows: map[string]Entry{}}, nil
	}
	if err != nil {
		return registryFile{}, fmt.Errorf("read registry.json: %w", err)
	}
	var f registryFile
	if err := json.Unmarshal(b, &f); err != nil {
		return registryFile{}, fmt.Errorf("parse registry.json: %w", err)
	}
	if f.SchemaVersion == 0 {
		f.SchemaVersion = SchemaVersion
	}
	if f.SchemaVersion != SchemaVersion {
		return registryFile{}, fmt.Errorf("registry.json schema_version=%d (this binary expects %d)", f.SchemaVersion, SchemaVersion)
	}
	if f.Workflows == nil {
		f.Workflows = map[string]Entry{}
	}
	return f, nil
}

func (r *Registry) writeRegistry(f registryFile) error {
	if f.SchemaVersion == 0 {
		f.SchemaVersion = SchemaVersion
	}
	if f.Workflows == nil {
		f.Workflows = map[string]Entry{}
	}
	return writeJSON(r.registryPath(), f)
}

func (r *Registry) resolveEntryFile(rel string) (string, error) {
	if strings.TrimSpace(rel) == "" {
		return "", errors.New("global workflow registry entry has empty definition_file")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("global workflow registry entry has absolute definition_file: %s", rel)
	}
	abs, err := filepath.Abs(filepath.Join(r.prefix, filepath.FromSlash(rel)))
	if err != nil {
		return "", err
	}
	root, err := filepath.Abs(r.prefix)
	if err != nil {
		return "", err
	}
	inside, err := pathInside(root, abs)
	if err != nil {
		return "", err
	}
	if !inside {
		return "", fmt.Errorf("global workflow definition_file escapes prefix: %s", rel)
	}
	return abs, nil
}

func definitionFileReferenced(f registryFile, rel string) bool {
	for _, entry := range f.Workflows {
		if entry.DefinitionFile == rel {
			return true
		}
	}
	return false
}

func validateName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("%w: empty", ErrInvalidName)
	}
	if strings.ContainsAny(name, "\x00") {
		return fmt.Errorf("%w: contains NUL", ErrInvalidName)
	}
	if workflow.HasReservedRevisionSuffix(name) {
		return fmt.Errorf("%w: uses reserved revision suffix marker %q", ErrInvalidName, workflow.RevisionSuffixMarker)
	}
	return nil
}

func cloneMetadata(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("mkdir for %s: %w", path, err)
	}
	tmp := path + ".tmp"
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func pathInside(root, candidate string) (bool, error) {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false, err
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !strings.HasPrefix(rel, "../"), nil
}
