package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	fleetruntime "github.com/gisikw/familiar-fleet/internal/runtime"
)

// HerdrEnvironment is the result of PrepareHerdrEnvironment.
type HerdrEnvironment struct {
	// Runtime is the validated runtime/current target.
	Runtime string
	// UserShell is the shell the pane launcher adapts around.
	UserShell string
	// Notes are non-fatal observations worth logging (e.g. SHELL unset).
	Notes []string
	// Env is the complete environment for the Familiar-owned Herdr server.
	Env []string
}

// UserHerdrConfigPath mirrors Herdr's own lookup: $HERDR_CONFIG_PATH, else
// $XDG_CONFIG_HOME/herdr/config.toml, else ~/.config/herdr/config.toml.
func UserHerdrConfigPath(environ map[string]string) string {
	if p := environ["HERDR_CONFIG_PATH"]; p != "" {
		return p
	}
	if x := environ["XDG_CONFIG_HOME"]; x != "" {
		return filepath.Join(x, "herdr", "config.toml")
	}
	return filepath.Join(environ["HOME"], ".config", "herdr", "config.toml")
}

func environMap(environ []string) map[string]string {
	m := make(map[string]string, len(environ))
	for _, kv := range environ {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

// PrepareHerdrEnvironment is the preflight that must succeed before the
// Familiar-owned Herdr server starts. It:
//
//  1. requires an activated, valid runtime/current (no ambient pi fallback);
//  2. resolves the user's shell from the parent environment;
//  3. regenerates the pane launcher and its rc files;
//  4. writes the generated Herdr config (user's config.toml plus Familiar's
//     [terminal] table) and validates it with `herdr config check`;
//  5. returns the server environment with runtime/current/bin first on PATH
//     and HERDR_CONFIG_PATH pointing at the generated file.
//
// Because PATH references the stable pointer, later `runtime apply` runs
// take effect in new panes without restarting Herdr.
func PrepareHerdrEnvironment(ctx context.Context, paths Paths, herdr string, environ []string) (HerdrEnvironment, error) {
	var result HerdrEnvironment
	env := environMap(environ)

	store := fleetruntime.New(paths.RuntimeDir)
	current, err := store.Ready()
	if err != nil {
		return result, err
	}
	result.Runtime = current

	shell, note, err := ResolveUserShell(env["SHELL"], paths)
	if err != nil {
		return result, err
	}
	if note != "" {
		result.Notes = append(result.Notes, note)
	}
	result.UserShell = shell
	if ClassifyShell(shell) == ShellUnknown {
		result.Notes = append(result.Notes, fmt.Sprintf("shell %s is not natively supported; panes get PATH pre-set with a warning", shell))
	}

	spec := PaneShellSpec{UserShell: shell, RuntimeBin: store.CurrentBin(), Login: DefaultLogin()}
	if err := WritePaneShell(paths, spec); err != nil {
		return result, fmt.Errorf("write pane shell launcher: %w", err)
	}

	userConfigPath := UserHerdrConfigPath(env)
	userConfig, err := os.ReadFile(userConfigPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, fmt.Errorf("read Herdr config %s: %w", userConfigPath, err)
	}
	merged, err := MergeHerdrConfig(string(userConfig), paths.PaneShell)
	if err != nil {
		return result, err
	}
	if err := atomicWrite(paths.HerdrConfig, []byte(merged), 0600); err != nil {
		return result, fmt.Errorf("write generated Herdr config: %w", err)
	}

	// Build the server environment: same as the parent, except PATH and
	// HERDR_CONFIG_PATH. FAMILIAR_RUNTIME_BIN is informational for panes and
	// callback commands.
	serverEnv := make([]string, 0, len(environ)+3)
	for _, kv := range environ {
		key := kv
		if i := strings.IndexByte(kv, '='); i > 0 {
			key = kv[:i]
		}
		switch key {
		case "PATH", "HERDR_CONFIG_PATH", "FAMILIAR_RUNTIME_BIN":
			continue
		}
		serverEnv = append(serverEnv, kv)
	}
	serverEnv = append(serverEnv,
		"PATH="+store.CurrentBin()+prefixed(env["PATH"]),
		"HERDR_CONFIG_PATH="+paths.HerdrConfig,
		"FAMILIAR_RUNTIME_BIN="+store.CurrentBin(),
	)
	result.Env = serverEnv

	if herdr != "" {
		if err := checkHerdrConfig(ctx, herdr, serverEnv); err != nil {
			return result, err
		}
	}
	return result, nil
}

func prefixed(path string) string {
	if path == "" {
		return ""
	}
	return ":" + path
}

// checkHerdrConfig runs `herdr config check` against the generated config so
// a bad merge fails loudly instead of Herdr silently using defaults (which
// would fall back to $SHELL without the Familiar PATH assertion).
func checkHerdrConfig(ctx context.Context, herdr string, env []string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, herdr, "config", "check")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("generated Herdr config rejected by `herdr config check`: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
