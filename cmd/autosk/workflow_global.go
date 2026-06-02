package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"autosk/internal/agent/pkgregistry"
	"autosk/internal/globalworkflow"
	"autosk/internal/render"
	"autosk/internal/timeformat"
	"autosk/internal/workflow"
)

func newWorkflowGlobalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "global",
		Short: "Manage the global workflow registry",
		Long:  "Register, inspect, and sync workflow definitions outside any single project.",
	}
	cmd.AddCommand(
		newWorkflowGlobalAddCmd(),
		newWorkflowGlobalListCmd(),
		newWorkflowGlobalShowCmd(),
		newWorkflowGlobalRemoveCmd(),
		newWorkflowGlobalEnableCmd(),
		newWorkflowGlobalDisableCmd(),
		newWorkflowGlobalSyncCmd(),
		newWorkflowGlobalAdoptCmd(),
		newWorkflowGlobalInstallCmd(),
		newWorkflowGlobalUpdateCmd(),
	)
	return cmd
}

// ----------------------------------------------------------------------
// add <file>
// ----------------------------------------------------------------------

func parseAndValidateWorkflowFile(ctx context.Context, path string) (workflow.Definition, error) {
	def, err := workflow.ParseFile(path)
	if err != nil {
		return workflow.Definition{}, err
	}
	if err := workflow.Validate(ctx, def, nil, workflow.ValidateOpts{}); err != nil {
		return workflow.Definition{}, err
	}
	return def, nil
}

func newWorkflowGlobalAddCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "add <file>",
		Short: "Add a file-backed workflow to the global registry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			def, err := parseAndValidateWorkflowFile(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			hash, err := workflow.HashDefinition(def)
			if err != nil {
				return err
			}
			abs, _ := filepath.Abs(args[0])

			if dryRun {
				if flagQuiet {
					return nil
				}
				if flagJSON {
					return json.NewEncoder(os.Stdout).Encode(map[string]any{
						"dry_run":      true,
						"action":       "add",
						"name":         def.Name,
						"hash":         hash,
						"source":       abs,
						"would_mutate": true,
					})
				}
				fmt.Printf("dry-run: would add global workflow %s (hash=%s, source=%s)\n", def.Name, hash, abs)
				return nil
			}

			reg, err := globalworkflow.Default()
			if err != nil {
				return err
			}
			if err := reg.EnsurePrefix(); err != nil {
				return err
			}
			var enabledOpt *bool
			if existing, err := reg.Get(def.Name); err == nil {
				enabledOpt = &existing.Enabled
			}
			entry, err := reg.StoreDefinition(def, globalworkflow.StoreOptions{
				SourceType: "file",
				Source:     abs,
				Enabled:    enabledOpt,
			})
			if err != nil {
				return err
			}
			return emitGlobalWorkflow(entry)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview without writing the registry")
	return cmd
}

// ----------------------------------------------------------------------
// list [--all]
// ----------------------------------------------------------------------

func newWorkflowGlobalListCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List global workflows",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := globalworkflow.Default()
			if err != nil {
				return err
			}
			entries, err := reg.List(all)
			if err != nil {
				return err
			}
			return emitGlobalWorkflows(entries)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include disabled workflows")
	return cmd
}

// ----------------------------------------------------------------------
// show <name>
// ----------------------------------------------------------------------

func newWorkflowGlobalShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "Show a global workflow with its provenance and definition",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := globalworkflow.Default()
			if err != nil {
				return err
			}
			entry, err := reg.Get(args[0])
			if err != nil {
				if errors.Is(err, globalworkflow.ErrNotInstalled) {
					return fmt.Errorf("global workflow not found: %s", args[0])
				}
				return err
			}
			def, err := reg.LoadDefinition(args[0])
			if err != nil {
				return err
			}
			return emitGlobalWorkflowWithDef(entry, def)
		},
	}
	return cmd
}

// ----------------------------------------------------------------------
// remove <name> [--force]
// ----------------------------------------------------------------------

func newWorkflowGlobalRemoveCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a global workflow",
		Long:  "Refuses to remove an enabled workflow unless --force is passed.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			reg, err := globalworkflow.Default()
			if err != nil {
				return err
			}
			entry, err := reg.Get(name)
			if err != nil {
				if errors.Is(err, globalworkflow.ErrNotInstalled) {
					return fmt.Errorf("global workflow not found: %s", name)
				}
				return err
			}
			if entry.Enabled && !force {
				return fmt.Errorf("global workflow %s is enabled; pass --force to remove", name)
			}
			if err := reg.Remove(name); err != nil {
				return err
			}
			if flagQuiet {
				return nil
			}
			if flagJSON {
				return json.NewEncoder(os.Stdout).Encode(map[string]any{
					"name":    name,
					"removed": true,
				})
			}
			fmt.Printf("removed global workflow: %s\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "remove even if the workflow is enabled")
	return cmd
}

// ----------------------------------------------------------------------
// enable <name>
// ----------------------------------------------------------------------

func newWorkflowGlobalEnableCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "enable <name>",
		Short: "Enable a global workflow",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := globalworkflow.Default()
			if err != nil {
				return err
			}
			entry, err := reg.Enable(args[0])
			if err != nil {
				if errors.Is(err, globalworkflow.ErrNotInstalled) {
					return fmt.Errorf("global workflow not found: %s", args[0])
				}
				return err
			}
			return emitGlobalWorkflow(entry)
		},
	}
	return cmd
}

// ----------------------------------------------------------------------
// disable <name>
// ----------------------------------------------------------------------

func newWorkflowGlobalDisableCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "disable <name>",
		Short: "Disable a global workflow",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := globalworkflow.Default()
			if err != nil {
				return err
			}
			entry, err := reg.Disable(args[0])
			if err != nil {
				if errors.Is(err, globalworkflow.ErrNotInstalled) {
					return fmt.Errorf("global workflow not found: %s", args[0])
				}
				return err
			}
			return emitGlobalWorkflow(entry)
		},
	}
	return cmd
}

// ----------------------------------------------------------------------
// sync [--force] [--dry-run]
// ----------------------------------------------------------------------

func newWorkflowGlobalSyncCmd() *cobra.Command {
	var (
		force  bool
		dryRun bool
	)
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Materialize enabled global workflows into the current project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := globalworkflow.Default()
			if err != nil {
				return err
			}
			wf, dl, closeFn, err := workflowStoreFromCmdWithSkipAutoInitGlobalSync(cmd.Context(), !dryRun)
			if err != nil {
				return err
			}
			defer closeFn()

			report, err := globalworkflow.SyncGlobalWorkflows(cmd.Context(), reg, wf, globalworkflow.SyncOptions{
				DryRun: dryRun,
				Force:  force,
				InstallAgents: func(ctx context.Context, def workflow.Definition, entry globalworkflow.Entry) ([]pkgregistry.Entry, error) {
					versions := map[string]string{}
					if entry.SourceType == "package" && entry.Source != "" {
						if v, ok := entry.SourceMetadata["version"].(string); ok && v != "" {
							versions[entry.Source] = v
						}
					}
					return withAutoInstallJSONSilenced(ctx, def, wf.Agents(), dl, versions)
				},
			})
			if !dryRun && report.Mutated() {
				_ = dl.DoltCommit(cmd.Context(), "workflow sync global")
			}
			if emitErr := emitWorkflowSyncReport(report); emitErr != nil {
				return emitErr
			}
			if err != nil {
				return renderWorkflowSyncError(err, report)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "replace changed global-managed workflows when safe")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview sync without installing agents or writing the project DB")
	return cmd
}

// ----------------------------------------------------------------------
// adopt <name> [--dry-run]
// ----------------------------------------------------------------------

func newWorkflowGlobalAdoptCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "adopt <name>",
		Short: "Adopt a local project workflow into the global registry",
		Long:  "Reads the named workflow from the current project and stores its canonical definition in the global registry.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			wf, _, closeFn, err := workflowStoreFromCmd(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer closeFn()

			w, err := wf.GetByName(cmd.Context(), name)
			if err != nil {
				if errors.Is(err, workflow.ErrNotFound) {
					return fmt.Errorf("workflow not found: %s", name)
				}
				return err
			}
			def := workflowToDefinition(w)
			hash, err := workflow.HashDefinition(def)
			if err != nil {
				return err
			}

			if dryRun {
				if flagQuiet {
					return nil
				}
				if flagJSON {
					return json.NewEncoder(os.Stdout).Encode(map[string]any{
						"dry_run":      true,
						"action":       "adopt",
						"name":         name,
						"hash":         hash,
						"would_mutate": true,
					})
				}
				fmt.Printf("dry-run: would adopt global workflow %s (hash=%s)\n", name, hash)
				return nil
			}

			reg, err := globalworkflow.Default()
			if err != nil {
				return err
			}
			if err := reg.EnsurePrefix(); err != nil {
				return err
			}
			var enabledOpt *bool
			if existing, err := reg.Get(def.Name); err == nil {
				enabledOpt = &existing.Enabled
			}
			entry, err := reg.StoreDefinition(def, globalworkflow.StoreOptions{
				SourceType: "adopted",
				Source:     name,
				Enabled:    enabledOpt,
			})
			if err != nil {
				return err
			}
			return emitGlobalWorkflow(entry)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview without writing the registry")
	return cmd
}

// ----------------------------------------------------------------------
// install <npm-name-or-path> [--workflow] [--version] [--no-install] [--dry-run]
// ----------------------------------------------------------------------

func newWorkflowGlobalInstallCmd() *cobra.Command {
	var (
		workflowName string
		version      string
		noInstall    bool
		dryRun       bool
	)
	cmd := &cobra.Command{
		Use:   "install <npm-name-or-path>",
		Short: "Install a package-backed workflow into the global registry",
		Long:  "Installs a package and registers one of its declared workflows in the global registry.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			arg := strings.TrimSpace(args[0])
			if noInstall && version != "" {
				return errors.New("--version cannot be used with --no-install")
			}

			name, spec, err := resolveInstallSpec(arg, version)
			if err != nil {
				return err
			}
			isLocal := isLocalPath(arg)
			if isLocal && noInstall {
				return errors.New("--no-install cannot be used with local paths")
			}

			pkgReg, err := openPackagesRegistry()
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			if dryRun {
				if isLocal {
					wfs, err := resolveWorkflowsFromLocalPath(spec, workflowName)
					if err != nil {
						return err
					}
					selected, err := selectPackageWorkflow(wfs, workflowName)
					if err != nil {
						return err
					}
					def, err := parseAndValidateWorkflowFile(cmd.Context(), selected.File)
					if err != nil {
						return err
					}
					if def.Name != selected.Name {
						return fmt.Errorf("workflow manifest name %q does not match workflow file name %q", selected.Name, def.Name)
					}
					hash, _ := workflow.HashDefinition(def)
					if flagQuiet {
						return nil
					}
					if flagJSON {
						return json.NewEncoder(os.Stdout).Encode(map[string]any{
							"dry_run":         true,
							"action":          "install",
							"package_name":    name,
							"package_version": selected.PackageVersion,
							"workflow_name":   def.Name,
							"workflow_file":   selected.File,
							"hash":            hash,
							"would_mutate":    true,
						})
					}
					fmt.Printf("dry-run: would install global workflow %s from %s@%s (hash=%s)\n",
						def.Name, name, selected.PackageVersion, hash)
					return nil
				}

				// Remote package dry-run.
				if err := pkgregistry.ValidatePkgName(name); err != nil {
					return err
				}
				existing, err := pkgReg.Get(name)
				if noInstall {
					if err != nil {
						if errors.Is(err, pkgregistry.ErrNotInstalled) {
							return fmt.Errorf("package not installed: %s (remove --no-install to install it)", name)
						}
						return err
					}
					wfs, err := pkgReg.ResolveWorkflows(name)
					if err != nil {
						return err
					}
					selected, err := selectPackageWorkflow(wfs, workflowName)
					if err != nil {
						return err
					}
					def, err := parseAndValidateWorkflowFile(cmd.Context(), selected.File)
					if err != nil {
						return err
					}
					if def.Name != selected.Name {
						return fmt.Errorf("workflow manifest name %q does not match workflow file name %q", selected.Name, def.Name)
					}
					hash, err := workflow.HashDefinition(def)
					if err != nil {
						return err
					}
					if flagQuiet {
						return nil
					}
					if flagJSON {
						return json.NewEncoder(os.Stdout).Encode(map[string]any{
							"dry_run":         true,
							"action":          "install",
							"package_name":    name,
							"package_version": existing.Version,
							"workflow_name":   def.Name,
							"workflow_file":   selected.File,
							"hash":            hash,
							"would_mutate":    true,
						})
					}
					fmt.Printf("dry-run: would install global workflow %s from %s@%s (hash=%s)\n",
						def.Name, name, existing.Version, hash)
					return nil
				}
				if err == nil {
					if version != "" && existing.Version != version {
						if flagQuiet {
							return nil
						}
						if flagJSON {
							return json.NewEncoder(os.Stdout).Encode(map[string]any{
								"dry_run":         true,
								"action":          "install",
								"package_name":    name,
								"package_version": version,
								"note":            fmt.Sprintf("package installed at %s; dry-run cannot preview %s without installing", existing.Version, version),
								"would_mutate":    true,
							})
						}
						fmt.Printf("dry-run: package %s is installed at %s; cannot preview %s without installing\n", name, existing.Version, version)
						return nil
					}
					// No explicit version — stale-installed guard.
					if version == "" {
						if flagQuiet {
							return nil
						}
						if flagJSON {
							return json.NewEncoder(os.Stdout).Encode(map[string]any{
								"dry_run":         true,
								"action":          "install",
								"package_name":    name,
								"package_version": existing.Version,
								"note":            "package already installed; dry-run previews the installed version",
								"would_mutate":    true,
							})
						}
						fmt.Printf("dry-run: package %s is already installed at %s; pass --version to preview a different version\n", name, existing.Version)
						return nil
					}
					// version matches installed — fall through to hash preview
					wfs, err := pkgReg.ResolveWorkflows(name)
					if err != nil {
						return err
					}
					selected, err := selectPackageWorkflow(wfs, workflowName)
					if err != nil {
						return err
					}
					def, err := parseAndValidateWorkflowFile(cmd.Context(), selected.File)
					if err != nil {
						return err
					}
					if def.Name != selected.Name {
						return fmt.Errorf("workflow manifest name %q does not match workflow file name %q", selected.Name, def.Name)
					}
					hash, err := workflow.HashDefinition(def)
					if err != nil {
						return err
					}
					if flagQuiet {
						return nil
					}
					if flagJSON {
						return json.NewEncoder(os.Stdout).Encode(map[string]any{
							"dry_run":         true,
							"action":          "install",
							"package_name":    name,
							"package_version": existing.Version,
							"workflow_name":   def.Name,
							"workflow_file":   selected.File,
							"hash":            hash,
							"would_mutate":    true,
						})
					}
					fmt.Printf("dry-run: would install global workflow %s from %s@%s (hash=%s)\n",
						def.Name, name, existing.Version, hash)
					return nil
				}
				if errors.Is(err, pkgregistry.ErrNotInstalled) {
					// Not installed at all.
					if flagQuiet {
						return nil
					}
					if flagJSON {
						return json.NewEncoder(os.Stdout).Encode(map[string]any{
							"dry_run":         true,
							"action":          "install",
							"package_name":    name,
							"package_version": version,
							"note":            "package not installed; dry-run cannot resolve workflow without installing",
							"would_mutate":    true,
						})
					}
					fmt.Printf("dry-run: package %s is not installed; pass --no-install to preview a local path\n", name)
					return nil
				}
				return err
			}

			var entry pkgregistry.Entry
			if noInstall {
				entry, err = pkgReg.Get(name)
				if err != nil {
					if errors.Is(err, pkgregistry.ErrNotInstalled) {
						return fmt.Errorf("package not installed: %s (remove --no-install to install it)", name)
					}
					return err
				}
			} else {
				if err := pkgReg.EnsurePrefix(); err != nil {
					return err
				}
				err = withInstallStdoutSilenced(func() error {
					var ierr error
					entry, ierr = pkgReg.InstallWorkflowSpec(ctx, name, spec)
					return ierr
				})
				if err != nil {
					return err
				}
			}

			wfs, err := pkgReg.ResolveWorkflows(entry.Name)
			if err != nil {
				return err
			}
			selected, err := selectPackageWorkflow(wfs, workflowName)
			if err != nil {
				return err
			}
			def, err := parseAndValidateWorkflowFile(cmd.Context(), selected.File)
			if err != nil {
				return err
			}
			if def.Name != selected.Name {
				return fmt.Errorf("workflow manifest name %q does not match workflow file name %q", selected.Name, def.Name)
			}

			reg, err := globalworkflow.Default()
			if err != nil {
				return err
			}
			if err := reg.EnsurePrefix(); err != nil {
				return err
			}
			var enabledOpt *bool
			if existing, err := reg.Get(def.Name); err == nil {
				enabledOpt = &existing.Enabled
			}
			gwEntry, err := reg.StoreDefinition(def, globalworkflow.StoreOptions{
				SourceType: "package",
				Source:     entry.Name,
				SourceMetadata: map[string]any{
					"version":       entry.Version,
					"workflow_name": selected.Name,
				},
				Enabled: enabledOpt,
			})
			if err != nil {
				return err
			}
			return emitGlobalWorkflow(gwEntry)
		},
	}
	cmd.Flags().StringVar(&workflowName, "workflow", "", "workflow name to install when the package declares multiple workflows")
	cmd.Flags().StringVar(&version, "version", "", "npm version spec (default: latest)")
	cmd.Flags().BoolVar(&noInstall, "no-install", false, "use an already-installed package only; do not run npm install")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview without writing the registry")
	return cmd
}

// ----------------------------------------------------------------------
// update <name> [--version] [--no-install] [--dry-run] [--force]
// ----------------------------------------------------------------------

func newWorkflowGlobalUpdateCmd() *cobra.Command {
	var (
		version   string
		noInstall bool
		dryRun    bool
		force     bool
	)
	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Update a package-backed global workflow",
		Long:  "Re-installs the backing package and refreshes the stored definition in the global registry.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if noInstall && version != "" {
				return errors.New("--version cannot be used with --no-install")
			}

			reg, err := globalworkflow.Default()
			if err != nil {
				return err
			}
			entry, err := reg.Get(name)
			if err != nil {
				if errors.Is(err, globalworkflow.ErrNotInstalled) {
					return fmt.Errorf("global workflow not found: %s", name)
				}
				return err
			}
			if entry.SourceType != "package" {
				return fmt.Errorf("global workflow %s is not package-backed (source_type=%s)", name, entry.SourceType)
			}
			// Source for package-backed is the package name.
			pkgName := entry.Source

			pkgReg, err := openPackagesRegistry()
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			var spec string
			if version != "" {
				spec = pkgName + "@" + version
			} else {
				spec = pkgName
			}

			// Extract the originally-selected workflow name from metadata so
			// multi-workflow packages can be updated correctly.
			var selectedWorkflowName string
			if entry.SourceMetadata != nil {
				if v, ok := entry.SourceMetadata["workflow_name"].(string); ok {
					selectedWorkflowName = v
				}
			}

			if dryRun {
				if noInstall {
					existingPkg, err := pkgReg.Get(pkgName)
					if err != nil {
						if errors.Is(err, pkgregistry.ErrNotInstalled) {
							return fmt.Errorf("package not installed: %s", pkgName)
						}
						return err
					}
					// version mismatch guard for --dry-run --no-install --version
					if version != "" && existingPkg.Version != version {
						if flagQuiet {
							return nil
						}
						if flagJSON {
							return json.NewEncoder(os.Stdout).Encode(map[string]any{
								"dry_run":         true,
								"action":          "update",
								"name":            name,
								"package_name":    pkgName,
								"package_version": version,
								"note":            fmt.Sprintf("package installed at %s; dry-run cannot preview %s without installing", existingPkg.Version, version),
								"would_mutate":    true,
							})
						}
						fmt.Printf("dry-run: package %s is installed at %s; cannot preview %s without installing\n", pkgName, existingPkg.Version, version)
						return nil
					}
					wfs, err := pkgReg.ResolveWorkflows(pkgName)
					if err != nil {
						return err
					}
					selected, err := selectPackageWorkflow(wfs, selectedWorkflowName)
					if err != nil {
						return err
					}
					def, err := parseAndValidateWorkflowFile(cmd.Context(), selected.File)
					if err != nil {
						return err
					}
					if def.Name != selected.Name {
						return fmt.Errorf("workflow manifest name %q does not match workflow file name %q", selected.Name, def.Name)
					}
					if def.Name != name {
						return fmt.Errorf("workflow definition name %q does not match global workflow name %q", def.Name, name)
					}
					newHash, _ := workflow.HashDefinition(def)
					oldVersion := ""
					if entry.SourceMetadata != nil {
						if v, ok := entry.SourceMetadata["version"].(string); ok {
							oldVersion = v
						}
					}
					metadataChanged := oldVersion != existingPkg.Version
					if flagQuiet {
						return nil
					}
					if flagJSON {
						return json.NewEncoder(os.Stdout).Encode(map[string]any{
							"dry_run":         true,
							"action":          "update",
							"name":            name,
							"package_name":    pkgName,
							"package_version": existingPkg.Version,
							"previous_hash":   entry.DefinitionHash,
							"new_hash":        newHash,
							"would_mutate":    newHash != entry.DefinitionHash || metadataChanged || force,
						})
					}
					if newHash == entry.DefinitionHash && !metadataChanged && !force {
						fmt.Printf("dry-run: global workflow %s already at latest hash (%s)\n", name, newHash)
					} else {
						fmt.Printf("dry-run: would update global workflow %s (%s → %s)\n", name, entry.DefinitionHash, newHash)
					}
					return nil
				}
				// dry-run without --no-install: stale-installed guard
				existingPkg, err := pkgReg.Get(pkgName)
				if err == nil {
					if version != "" && existingPkg.Version != version {
						if flagQuiet {
							return nil
						}
						if flagJSON {
							return json.NewEncoder(os.Stdout).Encode(map[string]any{
								"dry_run":         true,
								"action":          "update",
								"name":            name,
								"package_name":    pkgName,
								"package_version": version,
								"note":            fmt.Sprintf("package installed at %s; dry-run cannot preview %s without installing", existingPkg.Version, version),
								"would_mutate":    true,
							})
						}
						fmt.Printf("dry-run: package %s is installed at %s; cannot preview %s without installing\n", pkgName, existingPkg.Version, version)
						return nil
					}
					if version == "" {
						if flagQuiet {
							return nil
						}
						if flagJSON {
							return json.NewEncoder(os.Stdout).Encode(map[string]any{
								"dry_run":         true,
								"action":          "update",
								"name":            name,
								"package_name":    pkgName,
								"package_version": existingPkg.Version,
								"note":            "package already installed; dry-run previews the installed version",
								"would_mutate":    true,
							})
						}
						fmt.Printf("dry-run: package %s is already installed at %s; pass --version to preview a different version\n", pkgName, existingPkg.Version)
						return nil
					}
					// version matches — fall through to hash preview
					wfs, err := pkgReg.ResolveWorkflows(pkgName)
					if err != nil {
						return err
					}
					selected, err := selectPackageWorkflow(wfs, selectedWorkflowName)
					if err != nil {
						return err
					}
					def, err := parseAndValidateWorkflowFile(cmd.Context(), selected.File)
					if err != nil {
						return err
					}
					if def.Name != selected.Name {
						return fmt.Errorf("workflow manifest name %q does not match workflow file name %q", selected.Name, def.Name)
					}
					if def.Name != name {
						return fmt.Errorf("workflow definition name %q does not match global workflow name %q", def.Name, name)
					}
					newHash, _ := workflow.HashDefinition(def)
					oldVersion := ""
					if entry.SourceMetadata != nil {
						if v, ok := entry.SourceMetadata["version"].(string); ok {
							oldVersion = v
						}
					}
					metadataChanged := oldVersion != existingPkg.Version
					if flagQuiet {
						return nil
					}
					if flagJSON {
						return json.NewEncoder(os.Stdout).Encode(map[string]any{
							"dry_run":         true,
							"action":          "update",
							"name":            name,
							"package_name":    pkgName,
							"package_version": existingPkg.Version,
							"previous_hash":   entry.DefinitionHash,
							"new_hash":        newHash,
							"would_mutate":    newHash != entry.DefinitionHash || metadataChanged || force,
						})
					}
					if newHash == entry.DefinitionHash && !metadataChanged && !force {
						fmt.Printf("dry-run: global workflow %s already at latest hash (%s)\n", name, newHash)
					} else {
						fmt.Printf("dry-run: would update global workflow %s (%s → %s)\n", name, entry.DefinitionHash, newHash)
					}
					return nil
				}
				if errors.Is(err, pkgregistry.ErrNotInstalled) {
					// Not installed.
					if flagQuiet {
						return nil
					}
					if flagJSON {
						return json.NewEncoder(os.Stdout).Encode(map[string]any{
							"dry_run":         true,
							"action":          "update",
							"name":            name,
							"package_name":    pkgName,
							"package_version": version,
							"note":            "package not installed; dry-run cannot resolve workflow without installing",
							"would_mutate":    true,
						})
					}
					fmt.Printf("dry-run: package %s is not installed\n", pkgName)
					return nil
				}
				return err
			}

			var pkgEntry pkgregistry.Entry
			if noInstall {
				pkgEntry, err = pkgReg.Get(pkgName)
				if err != nil {
					if errors.Is(err, pkgregistry.ErrNotInstalled) {
						return fmt.Errorf("package not installed: %s", pkgName)
					}
					return err
				}
			} else {
				if err := pkgReg.EnsurePrefix(); err != nil {
					return err
				}
				err = withInstallStdoutSilenced(func() error {
					var ierr error
					pkgEntry, ierr = pkgReg.InstallWorkflowSpec(ctx, pkgName, spec)
					return ierr
				})
				if err != nil {
					return err
				}
			}

			wfs, err := pkgReg.ResolveWorkflows(pkgEntry.Name)
			if err != nil {
				return err
			}
			selected, err := selectPackageWorkflow(wfs, selectedWorkflowName)
			if err != nil {
				return err
			}
			def, err := parseAndValidateWorkflowFile(cmd.Context(), selected.File)
			if err != nil {
				return err
			}
			if def.Name != selected.Name {
				return fmt.Errorf("workflow manifest name %q does not match workflow file name %q", selected.Name, def.Name)
			}
			if def.Name != name {
				return fmt.Errorf("workflow definition name %q does not match global workflow name %q", def.Name, name)
			}
			newHash, err := workflow.HashDefinition(def)
			if err != nil {
				return err
			}

			oldVersion := ""
			if entry.SourceMetadata != nil {
				if v, ok := entry.SourceMetadata["version"].(string); ok {
					oldVersion = v
				}
			}
			metadataChanged := oldVersion != pkgEntry.Version

			if newHash == entry.DefinitionHash && !metadataChanged && !force {
				if flagQuiet {
					return nil
				}
				if flagJSON {
					return json.NewEncoder(os.Stdout).Encode(map[string]any{
						"action":          "update",
						"name":            name,
						"package_name":    pkgName,
						"package_version": pkgEntry.Version,
						"previous_hash":   entry.DefinitionHash,
						"new_hash":        newHash,
						"updated":         false,
					})
				}
				fmt.Printf("global workflow %s already at latest hash (%s)\n", name, newHash)
				return nil
			}

			if err := reg.EnsurePrefix(); err != nil {
				return err
			}
			enabled := entry.Enabled
			gwEntry, err := reg.StoreDefinition(def, globalworkflow.StoreOptions{
				Revision:       "",
				SourceType:     "package",
				Source:         pkgEntry.Name,
				SourceMetadata: map[string]any{"version": pkgEntry.Version, "workflow_name": selected.Name},
				Enabled:        &enabled,
			})
			if err != nil {
				return err
			}
			return emitGlobalWorkflow(gwEntry)
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "npm version spec (default: latest)")
	cmd.Flags().BoolVar(&noInstall, "no-install", false, "use an already-installed package only; do not run npm install")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview without writing the registry")
	cmd.Flags().BoolVar(&force, "force", false, "update even when the definition hash has not changed")
	return cmd
}

// ----------------------------------------------------------------------
// rendering helpers
// ----------------------------------------------------------------------

type globalWorkflowJSON struct {
	Name           string         `json:"name"`
	DefinitionHash string         `json:"definition_hash"`
	DefinitionFile string         `json:"definition_file"`
	Revision       string         `json:"revision,omitempty"`
	SourceType     string         `json:"source_type,omitempty"`
	Source         string         `json:"source,omitempty"`
	SourceMetadata map[string]any `json:"source_metadata,omitempty"`
	Enabled        bool           `json:"enabled"`
	InstalledAt    string         `json:"installed_at"`
	UpdatedAt      string         `json:"updated_at"`
}

func entryToGlobalWorkflowJSON(e globalworkflow.Entry) globalWorkflowJSON {
	return globalWorkflowJSON{
		Name:           e.Name,
		DefinitionHash: e.DefinitionHash,
		DefinitionFile: e.DefinitionFile,
		Revision:       e.Revision,
		SourceType:     e.SourceType,
		Source:         e.Source,
		SourceMetadata: e.SourceMetadata,
		Enabled:        e.Enabled,
		InstalledAt:    e.InstalledAt.Format("2006-01-02T15:04:05Z"),
		UpdatedAt:      e.UpdatedAt.Format("2006-01-02T15:04:05Z"),
	}
}

func emitGlobalWorkflow(e globalworkflow.Entry) error {
	if flagQuiet {
		return nil
	}
	if flagJSON {
		return json.NewEncoder(os.Stdout).Encode(entryToGlobalWorkflowJSON(e))
	}
	fmt.Printf("%s  hash=%s  enabled=%t\n", render.BracketedRef("", e.Name), e.DefinitionHash, e.Enabled)
	if e.SourceType != "" {
		fmt.Printf("  source: %s", e.SourceType)
		if e.Source != "" {
			fmt.Printf(" (%s)", e.Source)
		}
		fmt.Println()
	}
	if e.Revision != "" {
		fmt.Printf("  revision: %s\n", e.Revision)
	}
	fmt.Printf("  definition_file: %s\n", e.DefinitionFile)
	fmt.Printf("  installed_at: %s\n", timeformat.FormatDateTime(e.InstalledAt))
	fmt.Printf("  updated_at: %s\n", timeformat.FormatDateTime(e.UpdatedAt))
	return nil
}

func emitGlobalWorkflows(entries []globalworkflow.Entry) error {
	if flagQuiet {
		return nil
	}
	if flagJSON {
		out := make([]globalWorkflowJSON, len(entries))
		for i, e := range entries {
			out[i] = entryToGlobalWorkflowJSON(e)
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "(no global workflows)")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "NAME\tHASH\tENABLED\tSOURCE"); err != nil {
		return err
	}
	for _, e := range entries {
		src := e.SourceType
		if e.Source != "" {
			src += " (" + e.Source + ")"
		}
		enab := "yes"
		if !e.Enabled {
			enab = "no"
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.Name, e.DefinitionHash[:min(16, len(e.DefinitionHash))], enab, src); err != nil {
			return err
		}
	}
	return w.Flush()
}

// workflowDefinitionJSON is the JSON projection of a stored workflow
// definition (not a DB Workflow row). It preserves ordered steps and
// transitions.
type workflowDefinitionJSON struct {
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	FirstStep   string             `json:"first_step"`
	Isolation   string             `json:"isolation"`
	Steps       []workflowStepJSON `json:"steps"`
}

type workflowStepJSON struct {
	Name      string                   `json:"name"`
	Agent     workflowStepAgentJSON    `json:"agent"`
	MaxVisits int                      `json:"max_visits,omitempty"`
	NextSteps []workflowTransitionJSON `json:"next_steps"`
}

type workflowStepAgentJSON struct {
	Name   string                `json:"name"`
	Params *workflow.AgentParams `json:"params,omitempty"`
}

type workflowTransitionJSON struct {
	Step       string `json:"step,omitempty"`
	TaskStatus string `json:"task_status,omitempty"`
	PromptRule string `json:"prompt_rule"`
}

func toDefinitionJSON(def workflow.Definition) workflowDefinitionJSON {
	out := workflowDefinitionJSON{
		Name:        def.Name,
		Description: def.Description,
		FirstStep:   def.FirstStep,
		Isolation:   string(def.Isolation.Normalize()),
	}
	stepNames := def.StepNames
	if len(stepNames) == 0 {
		stepNames = make([]string, 0, len(def.Steps))
		for n := range def.Steps {
			stepNames = append(stepNames, n)
		}
		sort.Strings(stepNames)
	}
	for _, n := range stepNames {
		sd := def.Steps[n]
		sj := workflowStepJSON{
			Name:      n,
			MaxVisits: sd.MaxVisits,
			Agent: workflowStepAgentJSON{
				Name:   sd.AgentName,
				Params: sd.AgentParams,
			},
		}
		for _, tr := range sd.NextSteps {
			sj.NextSteps = append(sj.NextSteps, workflowTransitionJSON{
				Step:       tr.Step,
				TaskStatus: tr.TaskStatus,
				PromptRule: tr.PromptRule,
			})
		}
		out.Steps = append(out.Steps, sj)
	}
	return out
}

func emitGlobalWorkflowWithDef(e globalworkflow.Entry, def workflow.Definition) error {
	if flagQuiet {
		return nil
	}
	if flagJSON {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"entry":      entryToGlobalWorkflowJSON(e),
			"definition": toDefinitionJSON(def),
		})
	}
	if err := emitGlobalWorkflow(e); err != nil {
		return err
	}
	fmt.Println("definition:")
	fmt.Printf("  name: %s\n", def.Name)
	if def.Description != "" {
		fmt.Printf("  description: %s\n", def.Description)
	}
	fmt.Printf("  first_step: %s\n", def.FirstStep)
	if def.Isolation != "" && def.Isolation != workflow.IsolationNone {
		fmt.Printf("  isolation: %s\n", def.Isolation)
	}
	if len(def.Steps) > 0 {
		fmt.Println("  steps:")
		stepNames := def.StepNames
		if len(stepNames) == 0 {
			stepNames = make([]string, 0, len(def.Steps))
			for n := range def.Steps {
				stepNames = append(stepNames, n)
			}
			sort.Strings(stepNames)
		}
		for _, n := range stepNames {
			sd := def.Steps[n]
			fmt.Printf("    %s\n", n)
			fmt.Printf("      agent: %s\n", sd.AgentName)
			if sd.MaxVisits > 0 {
				fmt.Printf("      max_visits: %d\n", sd.MaxVisits)
			}
			for _, tr := range sd.NextSteps {
				if tr.TaskStatus != "" {
					fmt.Printf("      → task_status=%s: %s\n", tr.TaskStatus, tr.PromptRule)
				} else {
					fmt.Printf("      → step %s: %s\n", tr.Step, tr.PromptRule)
				}
			}
		}
	}
	return nil
}

// ----------------------------------------------------------------------
// adopt / conversion helpers
// ----------------------------------------------------------------------

func workflowToDefinition(w workflow.Workflow) workflow.Definition {
	def := workflow.Definition{
		Name:        w.Name,
		Description: w.Description,
		FirstStep:   "",
		Isolation:   w.Isolation,
		Steps:       map[string]workflow.StepDef{},
		StepNames:   make([]string, 0, len(w.Steps)),
	}
	// First pass: build the complete ID→name map so transitions to
	// later steps resolve correctly.
	stepIDToName := map[string]string{}
	for _, st := range w.Steps {
		stepIDToName[st.ID] = st.Name
	}
	// Second pass: convert steps and transitions.
	for _, st := range w.Steps {
		if w.FirstStepID == st.ID {
			def.FirstStep = st.Name
		}
		def.StepNames = append(def.StepNames, st.Name)
		sd := workflow.StepDef{
			AgentName:   st.AgentName,
			AgentParams: st.AgentParams,
			MaxVisits:   st.MaxVisits,
		}
		for _, tr := range st.Transitions {
			td := workflow.TransitionDef{
				PromptRule: tr.PromptRule,
			}
			if tr.IsTaskStatus() {
				td.TaskStatus = tr.TaskStatus
			} else {
				td.Step = stepIDToName[tr.NextStepID]
			}
			sd.NextSteps = append(sd.NextSteps, td)
		}
		def.Steps[st.Name] = sd
	}
	return def
}

// ----------------------------------------------------------------------
// local-path dry-run helpers
// ----------------------------------------------------------------------

func resolveWorkflowsFromLocalPath(dir, workflowName string) ([]pkgregistry.WorkflowConfig, error) {
	pjPath := filepath.Join(dir, "package.json")
	b, err := os.ReadFile(filepath.Clean(pjPath))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", pjPath, err)
	}
	var m packageManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", pjPath, err)
	}
	if err := pkgregistry.ValidatePkgName(m.Name); err != nil {
		return nil, err
	}
	if m.Version == "" {
		return nil, fmt.Errorf("%w: %s missing version field", pkgregistry.ErrPackageMalformed, pjPath)
	}
	if m.Autosk == nil || len(m.Autosk.Workflows) == 0 {
		return nil, fmt.Errorf("%w: %s missing non-empty \"autosk.workflows\" array", pkgregistry.ErrPackageMalformed, pjPath)
	}
	if m.Autosk.Agent != nil {
		if err := pkgregistry.ValidateAgentConfig(m.Name, dir, m.Autosk.Agent); err != nil {
			return nil, err
		}
	}
	seen := make(map[string]struct{}, len(m.Autosk.Workflows))
	out := make([]pkgregistry.WorkflowConfig, 0, len(m.Autosk.Workflows))
	for i, wf := range m.Autosk.Workflows {
		wf.Name = strings.TrimSpace(wf.Name)
		wf.File = strings.TrimSpace(wf.File)
		if wf.Name == "" {
			return nil, fmt.Errorf("%w: autosk.workflows[%d].name is required", pkgregistry.ErrPackageMalformed, i)
		}
		if wf.File == "" {
			return nil, fmt.Errorf("%w: autosk.workflows[%d].file is required", pkgregistry.ErrPackageMalformed, i)
		}
		if _, ok := seen[wf.Name]; ok {
			return nil, fmt.Errorf("%w: duplicate workflow %q", pkgregistry.ErrPackageMalformed, wf.Name)
		}
		seen[wf.Name] = struct{}{}
		abs, err := resolveWorkflowFileInsidePkg(dir, wf.File)
		if err != nil {
			return nil, fmt.Errorf("%w: workflow %q file %q: %w", pkgregistry.ErrPackageMalformed, wf.Name, wf.File, err)
		}
		if _, err := os.Stat(abs); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("%w: workflow %q file missing: %s", pkgregistry.ErrPackageMalformed, wf.Name, abs)
			}
			return nil, fmt.Errorf("stat workflow file %s: %w", abs, err)
		}
		out = append(out, pkgregistry.WorkflowConfig{
			PackageName:    m.Name,
			PackageVersion: m.Version,
			InstallDir:     dir,
			Name:           wf.Name,
			File:           abs,
		})
	}
	if workflowName != "" {
		for _, wc := range out {
			if wc.Name == workflowName {
				return []pkgregistry.WorkflowConfig{wc}, nil
			}
		}
		return nil, fmt.Errorf("workflow %q not found in package", workflowName)
	}
	return out, nil
}

func resolveWorkflowFileInsidePkg(installDir, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("absolute paths are not allowed: %s", rel)
	}
	rel = strings.TrimPrefix(rel, "./")
	abs := filepath.Join(installDir, rel)
	clean, err := filepath.Abs(abs)
	if err != nil {
		return "", err
	}
	root, err := filepath.Abs(installDir)
	if err != nil {
		return "", err
	}
	rest, err := filepath.Rel(root, clean)
	if err != nil || rest == ".." || strings.HasPrefix(rest, ".."+string(filepath.Separator)) || strings.HasPrefix(rest, "../") {
		return "", fmt.Errorf("path escapes package: %s", rel)
	}
	return clean, nil
}

// packageManifest mirrors the bits of package.json we read for local
// dry-run validation. Kept in sync with pkgregistry.resolve.go.
type packageManifest struct {
	Name    string          `json:"name"`
	Version string          `json:"version"`
	Autosk  *autoskManifest `json:"autosk,omitempty"`
}

type autoskManifest struct {
	Agent     *pkgregistry.AgentManifest `json:"agent,omitempty"`
	Workflows []workflowManifest         `json:"workflows,omitempty"`
}

type workflowManifest struct {
	Name string `json:"name"`
	File string `json:"file"`
}
