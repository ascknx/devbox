// Copyright 2024 Jetify Inc. and contributors. All rights reserved.
// Use of this source code is governed by the license in the LICENSE file.

// Package devbox creates isolated development environments.
package devbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/trace"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/briandowns/spinner"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.jetify.com/devbox/internal/boxcli/usererr"
	"go.jetify.com/devbox/internal/cachehash"
	"go.jetify.com/devbox/internal/cmdutil"
	"go.jetify.com/devbox/internal/conf"
	"go.jetify.com/devbox/internal/debug"
	"go.jetify.com/devbox/internal/devbox/devopt"
	"go.jetify.com/devbox/internal/devbox/envpath"
	"go.jetify.com/devbox/internal/devbox/generate"
	"go.jetify.com/devbox/internal/devconfig"
	"go.jetify.com/devbox/internal/devconfig/configfile"
	"go.jetify.com/devbox/internal/devpkg"
	"go.jetify.com/devbox/internal/devpkg/pkgtype"
	"go.jetify.com/devbox/internal/envir"
	"go.jetify.com/devbox/internal/fileutil"
	"go.jetify.com/devbox/internal/lock"
	"go.jetify.com/devbox/internal/nix"
	"go.jetify.com/devbox/internal/plugin"
	"go.jetify.com/devbox/internal/redact"
	"go.jetify.com/devbox/internal/searcher"
	"go.jetify.com/devbox/internal/services"
	"go.jetify.com/devbox/internal/shellgen"
	"go.jetify.com/devbox/internal/telemetry"
	"go.jetify.com/devbox/internal/ux"
	"go.jetify.com/devbox/nix/flake"
)

const (

	// shellHistoryFile keeps the history of commands invoked inside devbox shell
	shellHistoryFile            = ".devbox/shell_history"
	processComposeTargetVersion = "v1.5.0"
	arbitraryCmdFilename        = ".cmd"
)

type Devbox struct {
	cfg                      *devconfig.Config
	env                      map[string]string
	environment              string
	lockfile                 *lock.File
	nix                      nix.Nixer
	projectDir               string
	pluginManager            *plugin.Manager
	customProcessComposeFile string

	// This is needed because of the --quiet flag.
	stderr io.Writer
}

var legacyPackagesWarningHasBeenShown = false

func InitConfig(dir string) error {
	_, err := devconfig.Init(dir)
	return err
}

func EnsureConfig(dir string) error {
	err := InitConfig(dir)
	if err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}

func Open(opts *devopt.Opts) (*Devbox, error) {
	var cfg *devconfig.Config
	var err error
	if opts.Dir == "" {
		cfg, err = devconfig.Find(".")
		if errors.Is(err, devconfig.ErrNotFound) {
			return nil, usererr.New("no devbox.json found in the current directory (or any parent directories). Did you run `devbox init` yet?")
		}
	} else {
		cfg, err = devconfig.Open(opts.Dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil, usererr.New("the devbox config path %q does not exist.", opts.Dir)
		}
		if errors.Is(err, devconfig.ErrNotFound) {
			return nil, usererr.New("no devbox.json found in %q. Did you run `devbox init` yet?", opts.Dir)
		}
	}
	if err != nil {
		return nil, usererr.WithUserMessage(err, "Error loading devbox.json.")
	}

	environment, err := validateEnvironment(opts.Environment)
	if err != nil {
		return nil, err
	}

	box := &Devbox{
		cfg:                      cfg,
		env:                      opts.Env,
		environment:              environment,
		nix:                      &nix.NixInstance{},
		projectDir:               filepath.Dir(cfg.Root.AbsRootPath),
		pluginManager:            plugin.NewManager(),
		stderr:                   opts.Stderr,
		customProcessComposeFile: opts.CustomProcessComposeFile,
	}

	lock, err := lock.GetFile(box)
	if err != nil {
		return nil, err
	}

	if err := cfg.LoadRecursive(lock); err != nil {
		return nil, err
	}

	// if lockfile has any allow insecure, we need to set the env var to ensure
	// all nix commands work.
	if err := box.moveAllowInsecureFromLockfile(box.stderr, lock, cfg); err != nil {
		ux.Fwarningf(
			box.stderr,
			"Failed to move allow_insecure from devbox.lock to devbox.json. An insecure package may "+
				"not work until you invoke `devbox add <pkg> --allow-insecure=<packages>` again: %s\n",
			err,
		)
		// continue on, since we do not want to block user.
	}

	box.pluginManager.ApplyOptions(
		plugin.WithDevbox(box),
		plugin.WithLockfile(lock),
	)
	box.lockfile = lock

	if !opts.IgnoreWarnings &&
		!legacyPackagesWarningHasBeenShown &&
		// HasDeprecatedPackages required nix to be installed. Since not all
		// commands require nix to be installed, only show this warning for commands
		// that ensure nix.
		// This warning can probably be removed soon.
		nix.Ensured() &&
		box.HasDeprecatedPackages() {
		legacyPackagesWarningHasBeenShown = true
		globalPath, err := GlobalDataPath()
		if err != nil {
			return nil, err
		}
		ux.Fwarningf(
			os.Stderr, // Always stderr. box.writer should probably always be err.
			"Your devbox.json at %s contains packages in legacy format. "+
				"Please run `devbox %supdate` to update your devbox.json.\n",
			box.projectDir,
			lo.Ternary(box.projectDir == globalPath, "global ", ""),
		)
	}

	return box, nil
}

func (d *Devbox) ProjectDir() string {
	return d.projectDir
}

func (d *Devbox) Config() *devconfig.Config {
	return d.cfg
}

func (d *Devbox) ConfigHash() (string, error) {
	h, err := d.cfg.Hash()
	if err != nil {
		return "", err
	}

	buf := bytes.Buffer{}
	buf.WriteString(h)
	for _, pkg := range d.AllPackages() {
		buf.WriteString(pkg.Hash())
	}
	for _, pluginConfig := range d.cfg.IncludedPluginConfigs() {
		h, err := pluginConfig.Hash()
		if err != nil {
			return "", err
		}
		buf.WriteString(h)
	}
	return cachehash.Bytes(buf.Bytes()), nil
}

func (d *Devbox) Stdenv() flake.Ref {
	return flake.Ref{
		Type:  flake.TypeGitHub,
		Owner: "NixOS",
		Repo:  "nixpkgs",
		Ref:   "nixpkgs-unstable",
		Rev:   d.cfg.NixPkgsCommitHash(),
	}
}

func (d *Devbox) Generate(ctx context.Context) error {
	ctx, task := trace.NewTask(ctx, "devboxGenerate")
	defer task.End()

	return errors.WithStack(shellgen.GenerateForPrintEnv(ctx, d))
}

func (d *Devbox) Shell(ctx context.Context, envOpts devopt.EnvOptions) error {
	ctx, task := trace.NewTask(ctx, "devboxShell")
	defer task.End()

	envs, err := d.ensureStateIsUpToDateAndComputeEnv(ctx, envOpts)
	if err != nil {
		return err
	}

	fmt.Fprintln(d.stderr, "Starting a devbox shell...")

	// Used to determine whether we're inside a shell (e.g. to prevent shell inception)
	// TODO: This is likely obsolete but we need to decide what happens when
	// the user does shell-ception. One option is to leave the current shell and
	// join a new one (that way they are not in nested shells.)
	envs[envir.DevboxShellEnabled] = "1"

	if err = createDevboxSymlink(d); err != nil {
		return err
	}

	opts := []ShellOption{
		WithHistoryFile(filepath.Join(d.projectDir, shellHistoryFile)),
		WithProjectDir(d.projectDir),
		WithEnvVariables(envs),
		WithShellStartTime(telemetry.ShellStart()),
	}

	shell, err := d.newShell(envOpts, opts...)
	if err != nil {
		return err
	}

	return shell.Run()
}

func (d *Devbox) RunScript(ctx context.Context, envOpts devopt.EnvOptions, cmdName string, cmdArgs []string) error {
	ctx, task := trace.NewTask(ctx, "devboxRun")
	defer task.End()

	if err := shellgen.WriteScriptsToFiles(d); err != nil {
		return err
	}

	lock.SetIgnoreShellMismatch(true)

	var env map[string]string
	if d.IsEnvEnabled() {
		// Skip ensureStateIsUpToDate if we are already in a shell of this devbox-project
		env = envir.PairsToMap(os.Environ())

		// We set this to ensure that init-hooks do NOT re-run. They would have
		// run when initializing the Devbox Environment in the current shell.
		env[d.SkipInitHookEnvName()] = "true"
	} else {
		var err error
		env, err = d.ensureStateIsUpToDateAndComputeEnv(ctx, envOpts)
		if err != nil {
			return err
		}
	}

	// Used to determine whether we're inside a shell (e.g. to prevent shell inception)
	// This is temporary because StartServices() needs it but should be replaced with
	// better alternative since devbox run and devbox shell are not the same.
	env["DEVBOX_SHELL_ENABLED"] = "1"

	// wrap the arg in double-quotes, and escape any double-quotes inside it.
	//
	// TODO(gcurtis): this breaks quote-removal in parameter expansion,
	// command substitution, and arithmetic expansion:
	//
	//	$ unset x
	//	$ echo ${x:-"my file"}
	//	my file
	//	$ devbox run -- echo '${x:-"my file"}'
	//	"my file"
	for idx, arg := range cmdArgs {
		cmdArgs[idx] = strconv.Quote(arg)
	}

	var cmdWithArgs []string
	if _, ok := d.cfg.Scripts()[cmdName]; ok {
		// it's a script, so replace the command with the script file's path.
		script := shellgen.ScriptPath(d.ProjectDir(), cmdName)
		cmdWithArgs = append([]string{strconv.Quote(script)}, cmdArgs...)
	} else {
		// Arbitrary commands should also run the hooks, so we write them to a file as well. However, if the
		// command args include env variable evaluations, then they'll be evaluated _before_ the hooks run,
		// which we don't want. So, one solution is to write the entire command and its arguments into the
		// file itself, but that may not be great if the variables contain sensitive information. Instead,
		// we save the entire command (with args) into the DEVBOX_RUN_CMD var, and then the script evals it.
		scriptBody, err := shellgen.ScriptBody(d, "eval $DEVBOX_RUN_CMD\n")
		if err != nil {
			return err
		}
		err = shellgen.WriteScriptFile(d, arbitraryCmdFilename, scriptBody)
		if err != nil {
			return err
		}
		script := shellgen.ScriptPath(d.ProjectDir(), arbitraryCmdFilename)
		cmdWithArgs = []string{strconv.Quote(script)}
		env["DEVBOX_RUN_CMD"] = strings.Join(append([]string{cmdName}, cmdArgs...), " ")
	}

	return nix.RunScript(d.projectDir, strings.Join(cmdWithArgs, " "), env)
}

// Install ensures that all the packages in the config are installed
// but does not run init hooks. It is used to power devbox install cli command.
func (d *Devbox) Install(ctx context.Context) error {
	ctx, task := trace.NewTask(ctx, "devboxInstall")
	defer task.End()

	return d.ensureStateIsUpToDate(ctx, ensure)
}

func (d *Devbox) ListScripts() []string {
	scripts := d.cfg.Scripts()
	keys := make([]string, len(scripts))
	i := 0
	for k := range scripts {
		keys[i] = k
		i++
	}

	slices.Sort(keys)

	return keys
}

// EnvExports returns a string of the env-vars that would need to be applied
// to define a Devbox environment. The string is of the form `export KEY=VALUE` for each
// env-var that needs to be applied.
func (d *Devbox) EnvExports(ctx context.Context, opts devopt.EnvExportsOpts) (string, error) {
	ctx, task := trace.NewTask(ctx, "devboxEnvExports")
	defer task.End()

	var envs map[string]string
	var err error

	envs, err = d.ensureStateIsUpToDateAndComputeEnv(ctx, opts.EnvOptions)
	if err != nil {
		return "", err
	}

	envStr := exportify(envs)

	if opts.RunHooks {
		hooksStr := ". \"" + shellgen.ScriptPath(d.ProjectDir(), shellgen.HooksFilename) + "\""
		envStr = fmt.Sprintf("%s\n%s;\n", envStr, hooksStr)
	}

	if !opts.NoRefreshAlias {
		envStr += "\n" + d.refreshAlias()
	}

	return envStr, nil
}

func (d *Devbox) EnvVars(ctx context.Context) ([]string, error) {
	ctx, task := trace.NewTask(ctx, "devboxEnvVars")
	defer task.End()
	// this only returns env variables for the shell environment excluding hooks
	envs, err := d.ensureStateIsUpToDateAndComputeEnv(ctx, devopt.EnvOptions{})
	if err != nil {
		return nil, err
	}
	return envir.MapToPairs(envs), nil
}

func (d *Devbox) shellEnvHashKey() string {
	// Don't make this a const so we don't use it by itself accidentally
	return "__DEVBOX_SHELLENV_HASH_" + d.ProjectDirHash()
}

func (d *Devbox) Info(ctx context.Context, pkg string, markdown bool) (string, error) {
	ctx, task := trace.NewTask(ctx, "devboxInfo")
	defer task.End()

	name, version, isVersioned := searcher.ParseVersionedPackage(pkg)
	if !isVersioned {
		name = pkg
		version = "latest"
	}

	packageVersion, err := searcher.Client().Resolve(name, version)
	if err != nil {
		if !errors.Is(err, searcher.ErrNotFound) {
			return "", usererr.WithUserMessage(err, "Package %q not found\n", pkg)
		}

		packageVersion = nil
		// fallthrough to below
	}

	if packageVersion == nil {
		return "", usererr.WithUserMessage(err, "Package %q not found\n", pkg)
	}

	// we should only have one result
	info := fmt.Sprintf(
		"%s%s %s\n%s\n",
		lo.Ternary(markdown, "## ", ""),
		packageVersion.Name,
		packageVersion.Version,
		packageVersion.Summary,
	)
	readme, err := plugin.Readme(
		ctx,
		devpkg.PackageFromStringWithDefaults(pkg, d.lockfile),
		d.projectDir,
		markdown,
	)
	if err != nil {
		return "", err
	}
	return info + readme, nil
}

// GenerateDevcontainer generates devcontainer.json and Dockerfile for vscode run-in-container
// and GitHub Codespaces
func (d *Devbox) GenerateDevcontainer(ctx context.Context, generateOpts devopt.GenerateOpts) error {
	ctx, task := trace.NewTask(ctx, "devboxGenerateDevcontainer")
	defer task.End()

	// construct path to devcontainer directory
	devContainerPath := filepath.Join(d.projectDir, ".devcontainer/")
	devContainerJSONPath := filepath.Join(devContainerPath, "devcontainer.json")
	dockerfilePath := filepath.Join(devContainerPath, "Dockerfile")

	// check if devcontainer.json or Dockerfile exist
	filesExist := fileutil.Exists(devContainerJSONPath) || fileutil.Exists(dockerfilePath)
	if !generateOpts.Force && filesExist {
		return usererr.New(
			"Files devcontainer.json or Dockerfile are already present in .devcontainer/. " +
				"Remove the files or use --force to overwrite them.",
		)
	}

	// create directory
	err := os.MkdirAll(devContainerPath, os.ModePerm)
	if err != nil {
		return redact.Errorf("error creating dev container directory in <project>/%s: %w",
			redact.Safe(filepath.Base(devContainerPath)), err)
	}

	// Setup generate parameters
	gen := &generate.Options{
		Path:           devContainerPath,
		RootUser:       generateOpts.RootUser,
		IsDevcontainer: true,
		Pkgs:           d.AllPackageNamesIncludingRemovedTriggerPackages(),
		LocalFlakeDirs: d.getLocalFlakesDirs(),
	}

	// generate dockerfile
	err = gen.CreateDockerfile(ctx, generate.CreateDockerfileOptions{})
	if err != nil {
		return redact.Errorf("error generating dev container Dockerfile in <project>/%s: %w",
			redact.Safe(filepath.Base(devContainerPath)), err)
	}
	// generate devcontainer.json
	err = gen.CreateDevcontainer(ctx)
	if err != nil {
		return redact.Errorf("error generating devcontainer.json in <project>/%s: %w",
			redact.Safe(filepath.Base(devContainerPath)), err)
	}
	return nil
}

// GenerateDockerfile generates a Dockerfile that replicates the devbox shell
func (d *Devbox) GenerateDockerfile(ctx context.Context, generateOpts devopt.GenerateOpts) error {
	ctx, task := trace.NewTask(ctx, "devboxGenerateDockerfile")
	defer task.End()

	dockerfilePath := filepath.Join(d.projectDir, "Dockerfile")
	// check if Dockerfile doesn't exist
	filesExist := fileutil.Exists(dockerfilePath)
	if !generateOpts.Force && filesExist {
		return usererr.New(
			"Dockerfile is already present in the current directory. " +
				"Remove it or use --force to overwrite it.",
		)
	}

	// Setup Generate parameters
	gen := &generate.Options{
		Path:           d.projectDir,
		RootUser:       generateOpts.RootUser,
		IsDevcontainer: false,
		Pkgs:           d.AllPackageNamesIncludingRemovedTriggerPackages(),
		LocalFlakeDirs: d.getLocalFlakesDirs(),
	}

	scripts := d.cfg.Scripts()

	// generate dockerfile
	return errors.WithStack(gen.CreateDockerfile(ctx, generate.CreateDockerfileOptions{
		ForType:    generateOpts.ForType,
		HasBuild:   scripts["build"] != nil,
		HasInstall: scripts["install"] != nil,
		HasStart:   scripts["start"] != nil,
	}))
}

func PrintEnvrcContent(w io.Writer, envFlags devopt.EnvFlags) error {
	return generate.EnvrcContent(w, envFlags)
}

// GenerateEnvrcFile generates a .envrc file that makes direnv integration convenient
func (d *Devbox) GenerateEnvrcFile(ctx context.Context, force bool, envFlags devopt.EnvFlags) error {
	ctx, task := trace.NewTask(ctx, "devboxGenerateEnvrc")
	defer task.End()

	envrcfilePath := filepath.Join(d.projectDir, ".envrc")
	filesExist := fileutil.Exists(envrcfilePath)
	if !force && filesExist {
		return usererr.New(
			"A .envrc is already present in the current directory. " +
				"Remove it or use --force to overwrite it.",
		)
	}

	// generate all shell files to ensure we can refer to them in the .envrc script
	if err := d.ensureStateIsUpToDate(ctx, ensure); err != nil {
		return err
	}

	// .envrc file creation
	err := generate.CreateEnvrc(ctx, d.projectDir, envFlags)
	if err != nil {
		return errors.WithStack(err)
	}
	ux.Fsuccessf(d.stderr, "generated .envrc file\n")
	if cmdutil.Exists("direnv") {
		cmd := exec.Command("direnv", "allow")
		err := cmd.Run()
		if err != nil {
			return errors.WithStack(err)
		}
		ux.Fsuccessf(d.stderr, "ran `direnv allow`\n")
	}
	return nil
}

// saveCfg writes the config file to the devbox directory.
func (d *Devbox) saveCfg() error {
	return d.cfg.Root.SaveTo(d.ProjectDir())
}

func (d *Devbox) Services() (services.Services, error) {
	pluginSvcs, err := plugin.GetServices(d.cfg.IncludedPluginConfigs())
	if err != nil {
		return nil, err
	}

	userSvcs := services.FromUserProcessCompose(d.projectDir, d.customProcessComposeFile)

	svcSet := lo.Assign(pluginSvcs, userSvcs)
	keys := make([]string, 0, len(svcSet))
	for k := range svcSet {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	result := services.Services{}
	for _, k := range keys {
		result[k] = svcSet[k]
	}

	return result, nil
}

func (d *Devbox) execPrintDevEnv(ctx context.Context, usePrintDevEnvCache bool) (map[string]string, error) {
	var spinny *spinner.Spinner
	if !usePrintDevEnvCache {
		spinny = spinner.New(spinner.CharSets[11], 100*time.Millisecond, spinner.WithWriter(d.stderr))
		spinny.FinalMSG = "✓ Computed the Devbox environment.\n"
		spinny.Suffix = " Computing the Devbox environment...\n"
		spinny.Start()
	}

	vaf, err := d.nix.PrintDevEnv(ctx, &nix.PrintDevEnvArgs{
		FlakeDir:             d.flakeDir(),
		PrintDevEnvCachePath: d.nixPrintDevEnvCachePath(),
		UsePrintDevEnvCache:  usePrintDevEnvCache,
	})
	if spinny != nil {
		spinny.Stop()
	}
	if err != nil {
		return nil, err
	}

	// Add environment variables from "nix print-dev-env" except for a few
	// special ones we need to ignore.
	env := map[string]string{}
	for key, val := range vaf.Variables {
		// We only care about "exported" because the var and array types seem to only be used by nix-defined
		// functions that we don't need (like genericBuild). For reference, each type translates to bash as follows:
		// var: export VAR=VAL
		// exported: export VAR=VAL
		// array: declare -a VAR=('VAL1' 'VAL2' )
		if val.Type != "exported" {
			continue
		}

		// SSL_CERT_FILE is a special-case. We only ignore it if it's
		// set to a specific value. This emulates the behavior of
		// "nix develop".
		if key == "SSL_CERT_FILE" && val.Value.(string) == "/no-cert-file.crt" {
			continue
		}

		// Certain variables get set to invalid values after Nix builds
		// the shell environment. For example, HOME=/homeless-shelter
		// and TMPDIR points to a missing directory. We want to ignore
		// those values and just use the values from the current
		// environment instead.
		if ignoreDevEnvVar[key] {
			continue
		}

		env[key] = val.Value.(string)
	}
	return env, nil
}

// computeEnv computes the set of environment variables that define a Devbox
// environment. The "devbox run" and "devbox shell" commands source these
// variables into a shell before executing a command or showing an interactive
// prompt.
//
// The process for building the environment involves layering sets of
// environment variables on top of each other, with each layer overwriting any
// duplicate keys from the previous:
//
//  1. Copy variables from the current environment except for those in
//     ignoreCurrentEnvVar, such as PWD and SHELL.
//  2. Copy variables from "nix print-dev-env" except for those in
//     ignoreDevEnvVar, such as TMPDIR and HOME.
//  3. Copy variables from Devbox plugins.
//  4. Set PATH to the concatenation of the PATHs from step 3, step 2, and
//     step 1 (in that order).
//
// The final result is a set of environment variables where Devbox plugins have
// the highest priority, then Nix environment variables, and then variables
// from the current environment. Similarly, the PATH gives Devbox plugin
// binaries the highest priority, then Nix packages, and then non-Nix
// programs.
//
// Note that the shellrc.tmpl template (which sources this environment) does
// some additional processing. The computeEnv environment won't necessarily
// represent the final "devbox run" or "devbox shell" environments.
func (d *Devbox) computeEnv(
	ctx context.Context,
	usePrintDevEnvCache bool,
	envOpts devopt.EnvOptions,
) (map[string]string, error) {
	defer debug.FunctionTimer().End()
	defer trace.StartRegion(ctx, "devboxComputeEnv").End()

	// Append variables from current env if --pure is not passed
	currentEnv := os.Environ()
	env, err := d.parseEnvAndExcludeSpecialCases(currentEnv, envOpts.Pure)
	if err != nil {
		return nil, err
	}

	// check if contents of .envrc is old and print warning
	if !usePrintDevEnvCache {
		err := d.checkOldEnvrc()
		if err != nil {
			return nil, err
		}
	}

	slog.Debug("current environment PATH", "path", env["PATH"])

	originalEnv := make(map[string]string, len(env))
	maps.Copy(originalEnv, env)

	if !envOpts.OmitNixEnv {
		nixEnv, err := d.execPrintDevEnv(ctx, usePrintDevEnvCache)
		if err != nil {
			return nil, err
		}

		for k, v := range nixEnv {
			env[k] = v
		}
	}
	slog.Debug("nix environment PATH", "path", env["PATH"])

	env["PATH"] = envpath.JoinPathLists(
		nix.ProfileBinPath(d.projectDir),
		env["PATH"],
	)

	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	// Add helpful env vars for a Devbox project
	env["DEVBOX_PROJECT_ROOT"] = d.projectDir
	env["DEVBOX_WD"] = wd
	env["DEVBOX_CONFIG_DIR"] = d.projectDir + "/devbox.d"
	env["DEVBOX_PACKAGES_DIR"] = d.projectDir + "/" + nix.ProfilePath

	// Include env variables in devbox.json
	configEnv, err := d.configEnvs(ctx, env)
	if err != nil {
		return nil, err
	}
	addEnvIfNotPreviouslySetByDevbox(env, configEnv)

	markEnvsAsSetByDevbox(configEnv)

	// devboxEnvPath starts with the initial PATH from print-dev-env, and is
	// transformed to be the "PATH of the Devbox environment"
	// TODO: The prior statement is not fully true,
	//  since env["PATH"] is written to above and so it is already no longer "PATH
	//  from print-dev-env". Consider moving devboxEnvPath higher up in this function
	//  where env["PATH"] is written to.
	devboxEnvPath := env["PATH"]
	slog.Debug("PATH after plugins and config", "path", devboxEnvPath)

	// We filter out nix store paths of devbox-packages (represented here as buildInputs).
	// Motivation: if a user removes a package from their devbox it should no longer
	// be available in their environment.
	buildInputs := strings.Split(env["buildInputs"], " ")
	var glibcPatchPath []string
	devboxEnvPath = filterPathList(devboxEnvPath, func(path string) bool {
		// TODO(gcurtis): this is a massive hack. Please get rid
		// of this and install the package to the profile.
		if strings.Contains(path, "patched-glibc") {
			glibcPatchPath = append(glibcPatchPath, path)
			return true
		}
		for _, input := range buildInputs {
			// input is of the form: /nix/store/<hash>-<package-name>-<version>
			// path is of the form: /nix/store/<hash>-<package-name>-<version>/bin
			if strings.TrimSpace(input) != "" && strings.HasPrefix(path, input) {
				slog.Debug("filtering out buildInput from PATH", "path", path, "input", input)
				return false
			}
		}
		return true
	})
	slog.Debug("PATH after filtering buildInputs", "inputs", buildInputs, "path", devboxEnvPath)

	// TODO(gcurtis): this is a massive hack. Please get rid
	// of this and install the package to the profile.
	if len(glibcPatchPath) != 0 {
		patchedPath := strings.Join(glibcPatchPath, string(filepath.ListSeparator))
		devboxEnvPath = envpath.JoinPathLists(patchedPath, devboxEnvPath)
		slog.Debug("PATH after glibc-patch hack", "path", devboxEnvPath)
	}

	runXPaths, err := d.RunXPaths(ctx)
	if err != nil {
		return nil, err
	}
	devboxEnvPath = envpath.JoinPathLists(devboxEnvPath, runXPaths)

	pathStack := envpath.Stack(env, originalEnv)
	pathStack.Push(env, d.ProjectDirHash(), devboxEnvPath, envOpts.PreservePathStack)
	env["PATH"] = pathStack.Path(env)
	slog.Debug("new path stack is", "path_stack", pathStack)

	slog.Debug("computed environment PATH", "path", env["PATH"])

	if !envOpts.Pure {
		// preserve the original XDG_DATA_DIRS by prepending to it
		env["XDG_DATA_DIRS"] = envpath.JoinPathLists(env["XDG_DATA_DIRS"], os.Getenv("XDG_DATA_DIRS"))
	}

	for k, v := range d.env {
		env[k] = v
	}

	return env, d.addHashToEnv(env)
}

// ensureStateIsUpToDateAndComputeEnv will return a map of the env-vars for the Devbox Environment
// while ensuring these reflect the current (up to date) state of the project.
func (d *Devbox) ensureStateIsUpToDateAndComputeEnv(
	ctx context.Context,
	envOpts devopt.EnvOptions,
) (map[string]string, error) {
	defer debug.FunctionTimer().End()

	upToDate, err := d.lockfile.IsUpToDateAndInstalled(isFishShell())
	if err != nil {
		return nil, err
	}
	if !upToDate {
		if envOpts.Hooks.OnStaleState != nil {
			envOpts.Hooks.OnStaleState()
		}
	}

	if !envOpts.SkipRecompute {
		// When ensureStateIsUpToDate is called with ensure=true, it always
		// returns early if the lockfile is up to date. So we don't need to check here
		if err := d.ensureStateIsUpToDate(ctx, ensure); isConnectionError(err) {
			if !fileutil.Exists(d.nixPrintDevEnvCachePath()) {
				ux.Ferrorf(
					d.stderr,
					"Error connecting to the internet and no cached environment found. Aborting.\n",
				)
				return nil, err
			}
			ux.Fwarningf(
				d.stderr,
				"Error connecting to the internet. Will attempt to use cached environment.\n",
			)
		} else if err != nil {
			// Some other non connection error, just return it.
			return nil, err
		}
	}

	// Since ensureStateIsUpToDate calls computeEnv when not up do date,
	// it's ok to use usePrintDevEnvCache=true here always. This does end up
	// doing some non-nix work twice if lockfile is not up to date.
	// TODO: Improve this to avoid extra work.
	return d.computeEnv(ctx, true /*usePrintDevEnvCache*/, envOpts)
}

func (d *Devbox) nixPrintDevEnvCachePath() string {
	return filepath.Join(d.projectDir, ".devbox/.nix-print-dev-env-cache")
}

func (d *Devbox) flakeDir() string {
	return filepath.Join(d.projectDir, ".devbox/gen/flake")
}

// AllPackageNamesIncludingRemovedTriggerPackages returns the all package names,
// including those added by plugins and also those removed by builtins.
// This has a gross name to differentiate it from AllPackages.
// Some uses cases for this are the lockfile and devbox list command.
//
// TODO: We may want to get rid of this function and have callers
// build their own list. e.g. Some callers need different representations of
// flakes  (lockfile vs devbox list)
func (d *Devbox) AllPackageNamesIncludingRemovedTriggerPackages() []string {
	result := []string{}
	for _, p := range d.cfg.Packages(true /*includeRemovedTriggerPackages*/) {
		result = append(result, p.VersionedName())
	}
	return result
}

func (d *Devbox) AllPackagesIncludingRemovedTriggerPackages() []*devpkg.Package {
	packages := d.cfg.Packages(true /*includeRemovedTriggerPackages*/)
	return devpkg.PackagesFromConfig(packages, d.lockfile)
}

// AllPackages returns the packages that are defined in devbox.json and
// recursively added by plugins.
// NOTE: This will not return packages removed by their plugin with the
// __remove_trigger_package field.
func (d *Devbox) AllPackages() []*devpkg.Package {
	packages := d.cfg.Packages(false /*includeRemovedTriggerPackages*/)
	return devpkg.PackagesFromConfig(packages, d.lockfile)
}

func (d *Devbox) TopLevelPackages() []*devpkg.Package {
	return devpkg.PackagesFromConfig(d.cfg.Root.TopLevelPackages(), d.lockfile)
}

// InstallablePackages returns the packages that are to be installed
func (d *Devbox) InstallablePackages() []*devpkg.Package {
	return lo.Filter(d.AllPackages(), func(pkg *devpkg.Package, _ int) bool {
		return pkg.IsInstallable()
	})
}

func (d *Devbox) HasDeprecatedPackages() bool {
	for _, pkg := range d.AllPackages() {
		if pkg.IsLegacy() {
			return true
		}
	}
	return false
}

func (d *Devbox) findPackageByName(name string) (*devpkg.Package, error) {
	if name == "" {
		return nil, errors.New("package name cannot be empty")
	}
	results := map[*devpkg.Package]bool{}
	for _, pkg := range d.TopLevelPackages() {
		if pkg.Raw == name || pkg.CanonicalName() == name {
			results[pkg] = true
		}
	}
	if len(results) > 1 {
		return nil, usererr.New(
			"found multiple packages with name %s: %s. Please specify version",
			name,
			lo.Keys(results),
		)
	}
	if len(results) == 0 {
		return nil, usererr.WithUserMessage(
			searcher.ErrNotFound, "no package found with name %s", name)
	}
	return lo.Keys(results)[0], nil
}

func (d *Devbox) checkOldEnvrc() error {
	envrcPath := filepath.Join(d.ProjectDir(), ".envrc")
	noUpdate, err := strconv.ParseBool(os.Getenv("DEVBOX_NO_ENVRC_UPDATE"))
	if err != nil {
		// DEVBOX_NO_ENVRC_UPDATE is either not set or invalid
		// so we consider it the same as false
		noUpdate = false
	}
	// check if user has an old version of envrc
	if fileutil.Exists(envrcPath) && !noUpdate {
		isNewEnvrc, err := fileutil.FileContains(
			envrcPath,
			"eval \"$(devbox generate direnv --print-envrc)\"",
		)
		if err != nil {
			return err
		}
		if !isNewEnvrc {
			ux.Fwarningf(
				d.stderr,
				"Your .envrc file seems to be out of date. "+
					"Run `devbox generate direnv --force` to update it.\n"+
					"Or silence this warning by setting DEVBOX_NO_ENVRC_UPDATE=1 env variable.\n",
			)
		}
	}
	return nil
}

// configEnvs takes the existing environment (nix + plugin) and adds env
// variables defined in Config. It also parses variables in config
// that are referenced by $VAR or ${VAR} and replaces them with
// their value in the existing env variables. Note, this doesn't
// allow env variables from outside the shell to be referenced so
// no leaked variables are caused by this function.
func (d *Devbox) configEnvs(
	ctx context.Context,
	existingEnv map[string]string,
) (map[string]string, error) {
	defer debug.FunctionTimer().End()
	env := map[string]string{}
	if d.cfg.IsEnvsecEnabled() {
		secrets, err := d.Secrets(ctx)
		// TODO: replace this with error.Is check once envsec exports it.
		if err != nil && !strings.Contains(err.Error(), "project not initialized") {
			return nil, err
		} else if err != nil {
			ux.Fwarningf(
				d.stderr,
				"Ignoring env_from directive. jetify cloud secrets is not "+
					"initialized. Run `devbox secrets init` to initialize it.\n",
			)
		} else {
			cloudSecrets, err := secrets.List(ctx)
			if err != nil {
				ux.Fwarningf(
					os.Stderr,
					"Error reading secrets from jetify cloud: %s\n\n",
					err,
				)
			} else {
				for _, secret := range cloudSecrets {
					env[secret.Name] = secret.Value
				}
			}
		}
	} else if d.cfg.Root.IsdotEnvEnabled() {
		// if env_from points to a .env file, parse and add it
		parsedEnvs, err := d.cfg.Root.ParseEnvsFromDotEnv()
		if err != nil {
			// it's fine to include the error ParseEnvsFromDotEnv here because
			// the error message is relevant to the user
			return nil, usererr.New(
				"failed parsing %s file. Error: %v",
				d.cfg.Root.EnvFrom,
				err,
			)
		}
		for k, v := range parsedEnvs {
			env[k] = v
		}
	} else if d.cfg.Root.EnvFrom != "" {
		return nil, usererr.New(
			"unknown env_from value: %s. Supported values are: \"%q\" or a path to a file ending in \".env\"",
			d.cfg.Root.EnvFrom,
			configfile.JetifyCloudEnvFromValue,
		)
	}
	for k, v := range d.cfg.Env() {
		env[k] = v
	}
	return conf.OSExpandEnvMap(env, existingEnv, d.ProjectDir()), nil
}

// ignoreCurrentEnvVar contains environment variables that Devbox should remove
// from the slice of [os.Environ] variables before sourcing them. These are
// variables that are set automatically by a new shell.
var ignoreCurrentEnvVar = map[string]bool{
	envir.DevboxLatestVersion: true,

	// Devbox may change the working directory of the shell, so using the
	// original PWD and OLDPWD would be wrong.
	"PWD":    true,
	"OLDPWD": true,

	// SHLVL is the number of nested shells. Copying it would give the
	// Devbox shell the same level as the parent shell.
	"SHLVL": true,

	// The parent shell isn't guaranteed to be the same as the Devbox shell.
	"SHELL": true,

	// The "_" variable is read-only, so we ignore it to avoid attempting to write it later.
	"_": true,
}

// ignoreDevEnvVar contains environment variables that Devbox should remove from
// the slice of [Devbox.PrintDevEnv] variables before sourcing them.
//
// This list comes directly from the "nix develop" source:
// https://github.com/NixOS/nix/blob/f08ad5bdbac02167f7d9f5e7f9bab57cf1c5f8c4/src/nix/develop.cc#L257-L275
var ignoreDevEnvVar = map[string]bool{
	"BASHOPTS":           true,
	"HOME":               true,
	"NIX_BUILD_TOP":      true,
	"NIX_ENFORCE_PURITY": true,
	"NIX_LOG_FD":         true,
	"NIX_REMOTE":         true,
	"PPID":               true,
	"SHELL":              true,
	"SHELLOPTS":          true,
	"TEMP":               true,
	"TEMPDIR":            true,
	"TERM":               true,
	"TMP":                true,
	"TMPDIR":             true,
	"TZ":                 true,
	"UID":                true,
}

func (d *Devbox) ProjectDirHash() string {
	return cachehash.Bytes([]byte(d.projectDir))
}

func (d *Devbox) addHashToEnv(env map[string]string) error {
	hash, err := cachehash.JSON(env)
	if err == nil {
		env[d.shellEnvHashKey()] = hash
	}
	return err
}

// parseEnvAndExcludeSpecialCases converts env as []string to map[string]string
// In case of pure shell, it leaks HOME and it leaks PATH with some modifications
func (d *Devbox) parseEnvAndExcludeSpecialCases(currentEnv []string, pure bool) (map[string]string, error) {
	env := make(map[string]string, len(currentEnv))
	for _, kv := range currentEnv {
		key, val, found := strings.Cut(kv, "=")
		if !found {
			return nil, errors.Errorf("expected \"=\" in keyval: %s", kv)
		}
		if ignoreCurrentEnvVar[key] {
			continue
		}
		// handling special cases for pure shell
		// - HOME required for devbox binary to work
		// - PATH to find the nix installation. It is cleaned for pure mode below.
		// - TERM to enable colored text in the pure shell
		if !pure || key == "HOME" || key == "PATH" || key == "TERM" {
			env[key] = val
		}
	}

	// handling special case for PATH
	if pure {
		// Setting a custom env variable to differentiate pure and regular shell
		env["DEVBOX_PURE_SHELL"] = "1"
		// Finding nix executables in path and passing it through
		// As well as adding devbox itself to PATH
		// Both are needed for devbox commands inside pure shell to work
		nixPath, err := exec.LookPath("nix")
		if err != nil {
			return nil, errors.New("could not find any nix executable in PATH. Make sure Nix is installed and in PATH, then try again")
		}
		nixPath = filepath.Dir(nixPath)
		env["PATH"] = envpath.JoinPathLists(nixPath, dotdevboxBinPath(d))
	}
	return env, nil
}

func (d *Devbox) PluginManager() *plugin.Manager {
	return d.pluginManager
}

func (d *Devbox) Lockfile() *lock.File {
	return d.lockfile
}

func (d *Devbox) RunXPaths(ctx context.Context) (string, error) {
	runxBinPath := filepath.Join(d.projectDir, ".devbox", "virtenv", "runx", "bin")
	if err := os.RemoveAll(runxBinPath); err != nil {
		return "", err
	}
	if err := os.MkdirAll(runxBinPath, 0o755); err != nil {
		return "", err
	}

	for _, pkg := range d.InstallablePackages() {
		if !pkg.IsRunX() {
			continue
		}
		lockedPkg, err := d.lockfile.Resolve(pkg.Raw)
		if err != nil {
			return "", err
		}
		paths, err := pkgtype.RunXClient().Install(ctx, lockedPkg.Resolved)
		if err != nil {
			return "", err
		}
		for _, path := range paths {
			// create symlink to all files in p
			files, err := os.ReadDir(path)
			if err != nil {
				return "", err
			}
			for _, file := range files {
				src := filepath.Join(path, file.Name())
				dst := filepath.Join(runxBinPath, file.Name())
				if err := os.Symlink(src, dst); err != nil && !errors.Is(err, os.ErrExist) {
					return "", err
				}
			}
		}
	}
	return runxBinPath, nil
}

func validateEnvironment(environment string) (string, error) {
	if environment == "" {
		return "dev", nil
	}
	if environment == "dev" || environment == "prod" || environment == "preview" {
		return environment, nil
	}
	return "", usererr.New(
		"invalid environment %q. Environment must be one of dev, prod, or preview.",
		environment,
	)
}
