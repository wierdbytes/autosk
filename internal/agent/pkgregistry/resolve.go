package pkgregistry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PackageConfig is the resolved view of a single installed agent
// package: registry metadata + autosk.agent fields + absolute paths.
//
// All paths in the returned config are absolute. first_message_file is
// inlined into FirstMessage at resolve time.
//
// `FirstMessage` is the text the executor prepends to the very first
// user turn it sends to the spawned pi child. It is NOT a system-role
// prompt — pi has its own system prompt; this is just the leading
// block of the first user message.
type PackageConfig struct {
	// Name is the npm package name (= agent name in agents.name).
	Name string
	// Version is the installed semver.
	Version string
	// InstallDir is the absolute path under <prefix>/node_modules/.
	InstallDir string

	// Runner is the absolute path to the runner module when the package
	// declares one; empty for standard (pi-spawn) agents.
	Runner string

	// --- standard-agent fields (relevant when Runner == "") ---

	Model        string
	Thinking     string
	FirstMessage string
	ExtraArgs    []string
	// PiExtensions / PiSkills are absolute paths inside InstallDir.
	PiExtensions []string
	PiSkills     []string
}

// WorkflowConfig is one workflow bundle declared by an installed
// package's autosk.workflows manifest block. File is absolute and has
// already been checked to stay inside InstallDir.
type WorkflowConfig struct {
	PackageName    string
	PackageVersion string
	InstallDir     string
	Name           string
	File           string
}

// packageManifest mirrors the bits of package.json we read.
type packageManifest struct {
	Name    string          `json:"name"`
	Version string          `json:"version"`
	Autosk  *autoskManifest `json:"autosk,omitempty"`
}

type autoskManifest struct {
	Agent     *agentManifest     `json:"agent,omitempty"`
	Workflows []workflowManifest `json:"workflows,omitempty"`
}

type workflowManifest struct {
	Name string `json:"name"`
	File string `json:"file"`
}

type agentManifest struct {
	Runner           string   `json:"runner,omitempty"`
	Model            string   `json:"model,omitempty"`
	Thinking         string   `json:"thinking,omitempty"`
	FirstMessage     string   `json:"first_message,omitempty"`
	FirstMessageFile string   `json:"first_message_file,omitempty"`
	ExtraArgs        []string `json:"extra_args,omitempty"`
	PiExtensions     []string `json:"pi_extensions,omitempty"`
	PiSkills         []string `json:"pi_skills,omitempty"`
}

// validThinking mirrors the table from internal/agent/config.go (kept
// in sync with pi's thinking levels).
var validThinking = map[string]struct{}{
	"":        {},
	"off":     {},
	"minimal": {},
	"low":     {},
	"medium":  {},
	"high":    {},
	"xhigh":   {},
}

// Resolve loads the install-time configuration for one registered
// package. Returns ErrNotInstalled if the package isn't in
// registry.json, ErrPackageMalformed if the on-disk package.json is
// missing or has an invalid autosk.agent block.
//
// Resolve does NOT check the runtime bootstrapper — that's
// EnsureRuntime's job and is only relevant for custom (runner) agents.
func (r *Registry) Resolve(name string) (PackageConfig, error) {
	if name == "" {
		return PackageConfig{}, fmt.Errorf("pkgregistry.Resolve: empty name")
	}
	if name == HumanAgentName {
		return PackageConfig{}, fmt.Errorf("%w: %s (the human agent has no package config)", ErrNotInstalled, name)
	}
	entry, installDir, m, err := r.loadManifest(name)
	if err != nil {
		return PackageConfig{}, err
	}
	if m.Autosk == nil || m.Autosk.Agent == nil {
		return PackageConfig{}, fmt.Errorf("%w: %s missing \"autosk.agent\" block in package.json",
			ErrPackageMalformed, name)
	}
	return resolveAgentConfig(name, entry.Version, installDir, m.Autosk.Agent)
}

// ResolveAgentIfPresent returns the package's autosk.agent config when
// the manifest declares one. Packages that only ship workflows are not
// malformed for this helper; they return ok=false and nil error.
func (r *Registry) ResolveAgentIfPresent(name string) (cfg PackageConfig, ok bool, err error) {
	entry, installDir, m, err := r.loadManifest(name)
	if err != nil {
		return PackageConfig{}, false, err
	}
	if m.Autosk == nil || m.Autosk.Agent == nil {
		return PackageConfig{}, false, nil
	}
	cfg, err = resolveAgentConfig(name, entry.Version, installDir, m.Autosk.Agent)
	if err != nil {
		return PackageConfig{}, false, err
	}
	return cfg, true, nil
}

// ResolveWorkflows loads and validates the autosk.workflows entries for
// an installed package. It checks that each workflow name is unique and
// that every file path stays inside the package and exists on disk.
func (r *Registry) ResolveWorkflows(name string) ([]WorkflowConfig, error) {
	entry, installDir, m, err := r.loadManifest(name)
	if err != nil {
		return nil, err
	}
	if m.Autosk == nil || len(m.Autosk.Workflows) == 0 {
		return nil, fmt.Errorf("%w: %s missing non-empty \"autosk.workflows\" array in package.json",
			ErrPackageMalformed, name)
	}
	seen := make(map[string]struct{}, len(m.Autosk.Workflows))
	out := make([]WorkflowConfig, 0, len(m.Autosk.Workflows))
	for i, wf := range m.Autosk.Workflows {
		wf.Name = strings.TrimSpace(wf.Name)
		wf.File = strings.TrimSpace(wf.File)
		if wf.Name == "" {
			return nil, fmt.Errorf("%w: %s autosk.workflows[%d].name is required", ErrPackageMalformed, name, i)
		}
		if wf.File == "" {
			return nil, fmt.Errorf("%w: %s autosk.workflows[%d].file is required", ErrPackageMalformed, name, i)
		}
		if _, ok := seen[wf.Name]; ok {
			return nil, fmt.Errorf("%w: %s declares duplicate workflow %q", ErrPackageMalformed, name, wf.Name)
		}
		seen[wf.Name] = struct{}{}
		abs, err := resolveInsidePkg(installDir, wf.File)
		if err != nil {
			return nil, fmt.Errorf("%w: %s workflow %q file %q: %w", ErrPackageMalformed, name, wf.Name, wf.File, err)
		}
		if _, err := os.Stat(abs); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%w: %s workflow %q file missing: %s", ErrPackageMalformed, name, wf.Name, abs)
			}
			return nil, fmt.Errorf("stat workflow file %s: %w", abs, err)
		}
		out = append(out, WorkflowConfig{
			PackageName:    name,
			PackageVersion: entry.Version,
			InstallDir:     installDir,
			Name:           wf.Name,
			File:           abs,
		})
	}
	return out, nil
}

func (r *Registry) loadManifest(name string) (Entry, string, packageManifest, error) {
	entry, err := r.Get(name)
	if err != nil {
		return Entry{}, "", packageManifest{}, err
	}
	installDir := r.PackageInstallDir(name)
	manifestPath := filepath.Join(installDir, "package.json")

	b, err := os.ReadFile(manifestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Entry{}, "", packageManifest{}, fmt.Errorf("%w: %s is registered but %s does not exist (try installing %s again)",
				ErrPackageMalformed, name, manifestPath, name)
		}
		return Entry{}, "", packageManifest{}, fmt.Errorf("read %s: %w", manifestPath, err)
	}
	var m packageManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Entry{}, "", packageManifest{}, fmt.Errorf("%w: parse %s: %w", ErrPackageMalformed, manifestPath, err)
	}
	if m.Name != name {
		return Entry{}, "", packageManifest{}, fmt.Errorf("%w: registered name %q does not match package.json name %q",
			ErrPackageMalformed, name, m.Name)
	}
	return entry, installDir, m, nil
}

func resolveAgentConfig(name, version, installDir string, am *agentManifest) (PackageConfig, error) {
	if _, ok := validThinking[am.Thinking]; !ok {
		return PackageConfig{}, fmt.Errorf("%w: %s thinking=%q (want one of off|minimal|low|medium|high|xhigh)",
			ErrPackageMalformed, name, am.Thinking)
	}
	if am.FirstMessage != "" && am.FirstMessageFile != "" {
		return PackageConfig{}, fmt.Errorf("%w: %s declares both first_message and first_message_file (pick one)",
			ErrPackageMalformed, name)
	}

	cfg := PackageConfig{
		Name:         name,
		Version:      version,
		InstallDir:   installDir,
		Model:        am.Model,
		Thinking:     am.Thinking,
		FirstMessage: am.FirstMessage,
		ExtraArgs:    append([]string(nil), am.ExtraArgs...),
	}

	if am.Runner != "" {
		runnerAbs, err := resolveInsidePkg(installDir, am.Runner)
		if err != nil {
			return PackageConfig{}, fmt.Errorf("%w: %s runner %q: %v", ErrPackageMalformed, name, am.Runner, err)
		}
		if _, err := os.Stat(runnerAbs); err != nil {
			return PackageConfig{}, fmt.Errorf("%w: %s runner missing: %s", ErrPackageMalformed, name, runnerAbs)
		}
		cfg.Runner = runnerAbs
	}
	if am.FirstMessageFile != "" {
		abs, err := resolveInsidePkg(installDir, am.FirstMessageFile)
		if err != nil {
			return PackageConfig{}, fmt.Errorf("%w: %s first_message_file %q: %v",
				ErrPackageMalformed, name, am.FirstMessageFile, err)
		}
		body, err := os.ReadFile(abs)
		if err != nil {
			return PackageConfig{}, fmt.Errorf("%w: %s read first_message_file %s: %v",
				ErrPackageMalformed, name, abs, err)
		}
		cfg.FirstMessage = string(body)
	}
	for _, ext := range am.PiExtensions {
		abs, err := resolveInsidePkg(installDir, ext)
		if err != nil {
			return PackageConfig{}, fmt.Errorf("%w: %s pi_extensions[%q]: %v",
				ErrPackageMalformed, name, ext, err)
		}
		if _, err := os.Stat(abs); err != nil {
			return PackageConfig{}, fmt.Errorf("%w: %s pi_extension missing: %s",
				ErrPackageMalformed, name, abs)
		}
		cfg.PiExtensions = append(cfg.PiExtensions, abs)
	}
	for _, sk := range am.PiSkills {
		abs, err := resolveInsidePkg(installDir, sk)
		if err != nil {
			return PackageConfig{}, fmt.Errorf("%w: %s pi_skills[%q]: %v",
				ErrPackageMalformed, name, sk, err)
		}
		if _, err := os.Stat(abs); err != nil {
			return PackageConfig{}, fmt.Errorf("%w: %s pi_skill missing: %s",
				ErrPackageMalformed, name, abs)
		}
		cfg.PiSkills = append(cfg.PiSkills, abs)
	}
	return cfg, nil
}

// resolveInsidePkg joins rel to installDir and rejects any result that
// escapes the package directory.
func resolveInsidePkg(installDir, rel string) (string, error) {
	rel = trimSlash(rel)
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute paths are not allowed: %s", rel)
	}
	abs := filepath.Join(installDir, rel)
	clean, err := filepath.Abs(abs)
	if err != nil {
		return "", err
	}
	root, err := filepath.Abs(installDir)
	if err != nil {
		return "", err
	}
	// Reject "..": resolved path must stay under installDir.
	rest, err := filepath.Rel(root, clean)
	if err != nil || rest == ".." || hasParentPrefix(rest) {
		return "", fmt.Errorf("path escapes package: %s", rel)
	}
	return clean, nil
}

func hasParentPrefix(p string) bool {
	if len(p) < 2 {
		return false
	}
	return p[0] == '.' && p[1] == '.' && (len(p) == 2 || p[2] == filepath.Separator || p[2] == '/')
}
