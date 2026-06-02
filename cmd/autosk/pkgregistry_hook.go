package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"autosk/internal/agent/pkgregistry"
)

// pkgregistryNpmFactory builds the NpmRunner used by every pkgregistry
// the CLI opens. The default returns pkgregistry.ExecNpm{}; tests
// substitute a fake that materialises packages on-disk directly.
//
// All CLI sites that need pkgregistry should use openPackagesRegistry()
// rather than pkgregistry.Default() so this hook is honoured.
var pkgregistryNpmFactory = func() pkgregistry.NpmRunner { return pkgregistry.ExecNpm{} }

// openPackagesRegistry resolves the global packages prefix honouring
// $AUTOSK_PACKAGES, with the test-friendly NpmRunner hook applied.
func openPackagesRegistry() (*pkgregistry.Registry, error) {
	return pkgregistry.Default(pkgregistry.WithNpm(pkgregistryNpmFactory()))
}

// withJSONStdoutSilenced discards stdout produced by npm installs while
// a JSON command is assembling its result, so the final stdout stream is
// one machine-readable JSON document. Stderr is left untouched.
func withJSONStdoutSilenced(fn func() error) error {
	if !flagJSON {
		return fn()
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = devNull.Close() }()
	orig := os.Stdout
	os.Stdout = devNull
	defer func() { os.Stdout = orig }()
	return fn()
}

// withInstallStdoutSilenced discards stdout produced by npm installs when
// --json or --quiet is active, so the final output stream stays clean for
// machine-readable callers or quiet-mode operators.
//
// In --json mode only stdout is silenced; stderr is left untouched so
// actionable errors remain visible.
//
// In --quiet mode both stdout and stderr are captured. On success the
// captured output is discarded. On failure a concise tail of the
// captured stderr is included in the returned error so diagnostics are
// not lost.
func withInstallStdoutSilenced(fn func() error) error {
	if !flagJSON && !flagQuiet {
		return fn()
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = devNull.Close() }()
	origStdout := os.Stdout
	os.Stdout = devNull
	defer func() { os.Stdout = origStdout }()

	if !flagQuiet {
		// JSON mode only: suppress stdout, leave stderr alone.
		return fn()
	}

	// Quiet mode: capture stderr too so npm progress/warnings do not
	// leak to the terminal.
	var stderrBuf bytes.Buffer
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	origStderr := os.Stderr
	os.Stderr = w

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&stderrBuf, r)
		close(done)
	}()

	err = fn()
	_ = w.Close()
	os.Stderr = origStderr
	<-done
	_ = r.Close()

	if err != nil {
		tail := stderrBuf.String()
		const maxTail = 500
		if len(tail) > maxTail {
			tail = "..." + tail[len(tail)-maxTail:]
		}
		if tail != "" {
			return fmt.Errorf("%w\nnpm stderr: %s", err, tail)
		}
	}
	return err
}

// resolveInstallSpec translates the first argument to `autosk agent
// install` into a (name, npm-spec) pair.
//
// `arg` is either an npm registry name (`@autosk/dev`, `developer`) or
// a local file path (`./agents/developer`, `/abs/path`, `~/foo`). For
// local paths, the canonical agent name is read from the package's
// package.json; `version` is ignored.
func resolveInstallSpec(arg, version string) (name, spec string, err error) {
	if isLocalPath(arg) {
		abs, err := expandLocalPath(arg)
		if err != nil {
			return "", "", err
		}
		if st, err := os.Stat(abs); err != nil || !st.IsDir() {
			return "", "", fmt.Errorf("local install path is not a directory: %s", abs)
		}
		pkgName, err := pkgregistry.ReadPackageNameFromPath(abs)
		if err != nil {
			return "", "", err
		}
		return pkgName, abs, nil
	}
	// Registry install: spec = name[@version].
	name = arg
	spec = name
	if version != "" {
		spec = name + "@" + version
	}
	return name, spec, nil
}

// isLocalPath reports whether s looks like a filesystem path rather
// than an npm registry name. Scoped names (`@scope/name`) are NOT
// paths; we check the `@` form explicitly.
func isLocalPath(s string) bool {
	if s == "" {
		return false
	}
	switch {
	case strings.HasPrefix(s, "./"), strings.HasPrefix(s, "../"):
		return true
	case strings.HasPrefix(s, "/"):
		return true
	case strings.HasPrefix(s, "~/"), s == "~":
		return true
	}
	return false
}

func expandLocalPath(s string) (string, error) {
	if strings.HasPrefix(s, "~/") || s == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if s == "~" {
			return home, nil
		}
		s = filepath.Join(home, strings.TrimPrefix(s, "~/"))
	}
	return filepath.Abs(s)
}
