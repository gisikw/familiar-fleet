package client

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

	fleetruntime "github.com/gisikw/familiar-fleet/internal/runtime"
)

// HerdrEnvironment is the result of PrepareHerdrEnvironment.
type HerdrEnvironment struct {
	// Runtime is the validated runtime/current target.
	Runtime string
	// UserShell is the shell the pane launcher adapts around.
	UserShell string
	// Tiamat is the validated router URL and token file path. The token value
	// itself is never loaded, logged, or stored.
	Tiamat TiamatPreflight
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
//  2. requires a Tiamat router URL and an existing, readable token file,
//     resolving the latter to <state>/secrets/tiamat.token by default;
//  3. resolves the user's shell from the parent environment;
//  4. regenerates the pane launcher and its rc files;
//  5. projects the runtime's public Pi profile into mutable node-local state;
//  6. writes the generated Herdr config (user's config.toml plus Familiar's
//     [terminal] table) and validates it with `herdr config check`;
//  7. returns the server environment with runtime/current/bin first on PATH,
//     PI_CODING_AGENT_DIR set to that profile, HERDR_CONFIG_PATH pointing
//     at the generated file, and the resolved Tiamat URL and token path.
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

	// Validate Tiamat before anything is generated so a misconfigured node
	// fails loudly and without side effects.
	tiamat, err := ResolveTiamat(paths, env)
	if err != nil {
		return result, err
	}
	result.Tiamat = tiamat

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
	if err := writePiProfile(paths, store); err != nil {
		return result, fmt.Errorf("project Pi profile from active runtime: %w", err)
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

	// Build the server environment: same as the parent, except the runtime-owned
	// PATH, Herdr config, mutable Pi profile, and preflighted Tiamat values.
	// FAMILIAR_RUNTIME_BIN is informational for panes and callback commands.
	serverEnv := make([]string, 0, len(environ)+6)
	for _, kv := range environ {
		key := kv
		if i := strings.IndexByte(kv, '='); i > 0 {
			key = kv[:i]
		}
		switch key {
		case "PATH", "HERDR_CONFIG_PATH", "PI_CODING_AGENT_DIR", "FAMILIAR_RUNTIME_BIN", TiamatURLEnv, TiamatTokenFileEnv:
			continue
		}
		serverEnv = append(serverEnv, kv)
	}
	serverEnv = append(serverEnv,
		"PATH="+store.CurrentBin()+prefixed(env["PATH"]),
		"HERDR_CONFIG_PATH="+paths.HerdrConfig,
		"PI_CODING_AGENT_DIR="+paths.PiDir,
		"FAMILIAR_RUNTIME_BIN="+store.CurrentBin(),
		TiamatURLEnv+"="+tiamat.URL,
		TiamatTokenFileEnv+"="+tiamat.TokenFile,
	)
	result.Env = serverEnv

	if herdr != "" {
		if err := checkHerdrConfig(ctx, herdr, serverEnv); err != nil {
			return result, err
		}
	}
	return result, nil
}

// writePiProfile copies the public profile shape from the active runtime into
// writable node-local state. Its extension path uses runtime/current so an
// ordinary runtime activation updates Pi's code without regenerating the
// Herdr environment or restarting the server.
func writePiProfile(paths Paths, store fleetruntime.Store) error {
	template := filepath.Join(store.Current(), "share", "familiar-worker", "profile", "settings.json")
	data, err := os.ReadFile(template)
	if err != nil {
		return fmt.Errorf("read %s: %w", template, err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return fmt.Errorf("parse %s: %w", template, err)
	}
	extension := filepath.Join(store.Current(), "share", "familiar-worker", "extensions", "tiamat")
	if info, err := os.Stat(filepath.Join(extension, "index.ts")); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("active runtime does not provide the Tiamat extension at %s", extension)
	}
	settings["extensions"] = []string{extension}
	projected, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	projected = append(projected, '\n')
	if err := os.MkdirAll(paths.PiDir, 0700); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(paths.PiDir, "settings.json"), projected, 0600)
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
