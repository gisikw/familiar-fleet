package client

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fleetruntime "github.com/gisikw/familiar-fleet/internal/runtime"
)

func TestUserHerdrConfigPath(t *testing.T) {
	if got := UserHerdrConfigPath(map[string]string{"HERDR_CONFIG_PATH": "/x/c.toml", "HOME": "/h"}); got != "/x/c.toml" {
		t.Fatal(got)
	}
	if got := UserHerdrConfigPath(map[string]string{"XDG_CONFIG_HOME": "/xdg", "HOME": "/h"}); got != "/xdg/herdr/config.toml" {
		t.Fatal(got)
	}
	if got := UserHerdrConfigPath(map[string]string{"HOME": "/h"}); got != "/h/.config/herdr/config.toml" {
		t.Fatal(got)
	}
}

func fakeHerdr(t *testing.T, ok bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "herdr")
	script := "#!/bin/sh\n[ \"$1 $2\" = 'config check' ] || exit 9\n" +
		"printf '%s\\n' \"$HERDR_CONFIG_PATH\" >\"$0.cfg\"\nprintf '%s\\n' \"$PATH\" >\"$0.path\"\n"
	if ok {
		script += "echo 'config: ok'\n"
	} else {
		script += "echo 'config: issues found'; exit 1\n"
	}
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPrepareHerdrEnvironmentRequiresRuntime(t *testing.T) {
	p := testPaths(t)
	_, err := PrepareHerdrEnvironment(context.Background(), p, fakeHerdr(t, true), []string{"SHELL=/bin/sh", "HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"})
	if err == nil || !strings.Contains(err.Error(), "runtime apply") {
		t.Fatalf("expected clear no-runtime error, got %v", err)
	}
	if _, statErr := os.Stat(p.PaneShell); !os.IsNotExist(statErr) {
		t.Fatal("launcher must not be generated without a runtime")
	}
}

func TestPrepareHerdrEnvironmentBuildsServerEnv(t *testing.T) {
	p := testPaths(t)
	home := t.TempDir()
	// Activate a runtime.
	rt := filepath.Join(t.TempDir(), "familiar-worker-runtime")
	if err := os.MkdirAll(filepath.Join(rt, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rt, "bin", "pi"), []byte("#!/bin/sh\necho pi\n"), 0755); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(rt, "share", "familiar-worker", "profile")
	extension := filepath.Join(rt, "share", "familiar-worker", "extensions", "tiamat")
	if err := os.MkdirAll(profile, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(extension, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "settings.json"), []byte(`{"extensions":["/nix/store/old"],"defaultProjectTrust":"never"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extension, "index.ts"), []byte("export {};\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := fleetruntime.New(p.RuntimeDir).Activate(rt); err != nil {
		t.Fatal(err)
	}
	// User Herdr config that must be preserved.
	if err := os.MkdirAll(filepath.Join(home, ".config", "herdr"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "herdr", "config.toml"), []byte("[ui]\nsidebar_width = 42\n[terminal]\ndefault_shell = \"nu\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	herdr := fakeHerdr(t, true)
	// A parent HERDR_CONFIG_PATH is the user's source config; the server gets
	// the generated one instead.
	userCfg := filepath.Join(home, ".config", "herdr", "config.toml")
	environ := []string{"SHELL=/bin/sh", "HOME=" + home, "PATH=/usr/bin:/bin", "HERDR_CONFIG_PATH=" + userCfg, "FAMILIAR_RUNTIME_BIN=stale", "TERM=xterm"}
	got, err := PrepareHerdrEnvironment(context.Background(), p, herdr, environ)
	if err != nil {
		t.Fatal(err)
	}
	if got.Runtime != rt || got.UserShell != "/bin/sh" || len(got.Notes) != 0 {
		t.Fatalf("%+v", got)
	}
	env := environMap(got.Env)
	if env["PATH"] != p.RuntimeDir+"/current/bin:/usr/bin:/bin" {
		t.Errorf("PATH=%q", env["PATH"])
	}
	if env["HERDR_CONFIG_PATH"] != p.HerdrConfig || env["PI_CODING_AGENT_DIR"] != p.PiDir || env["FAMILIAR_RUNTIME_BIN"] != p.RuntimeDir+"/current/bin" || env["TERM"] != "xterm" {
		t.Errorf("env=%v", env)
	}
	for _, kv := range got.Env {
		if strings.HasPrefix(kv, "PATH=") && !strings.Contains(kv, "/current/bin:") {
			t.Errorf("duplicate/unfixed PATH entry %q", kv)
		}
	}
	cfg, err := os.ReadFile(p.HerdrConfig)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sidebar_width = 42", "default_shell = \"" + p.PaneShell + "\"", "shell_mode = \"non_login\""} {
		if !strings.Contains(string(cfg), want) {
			t.Errorf("generated config missing %q:\n%s", want, cfg)
		}
	}
	if strings.Contains(string(cfg), `"nu"`) {
		t.Errorf("user's default_shell not replaced:\n%s", cfg)
	}
	if checked, _ := os.ReadFile(herdr + ".cfg"); strings.TrimSpace(string(checked)) != p.HerdrConfig {
		t.Errorf("herdr config check ran against %q, want %q", checked, p.HerdrConfig)
	}
	if info, err := os.Stat(p.PaneShell); err != nil || info.Mode().Perm()&0100 == 0 {
		t.Fatalf("launcher missing: %v %v", info, err)
	}
	if info, err := os.Stat(p.HerdrConfig); err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatalf("generated config must be owner-only: %v %v", info, err)
	}
	projected, err := os.ReadFile(filepath.Join(p.PiDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(projected), p.RuntimeDir+"/current/share/familiar-worker/extensions/tiamat") || strings.Contains(string(projected), "/nix/store/old") {
		t.Fatalf("Pi profile does not use the stable runtime projection:\n%s", projected)
	}

	// Rejected config is fatal.
	if _, err := PrepareHerdrEnvironment(context.Background(), p, fakeHerdr(t, false), environ); err == nil || !strings.Contains(err.Error(), "config check") {
		t.Fatalf("expected config check failure, got %v", err)
	}

	// SHELL unset produces a note, not an error; SHELL pointing at the launcher is an error.
	got, err = PrepareHerdrEnvironment(context.Background(), p, herdr, []string{"HOME=" + home, "PATH=/bin"})
	if err != nil || got.UserShell != "/bin/sh" || len(got.Notes) != 1 {
		t.Fatalf("unset SHELL: %+v %v", got, err)
	}
	if _, err := PrepareHerdrEnvironment(context.Background(), p, herdr, []string{"SHELL=" + p.PaneShell, "HOME=" + home, "PATH=/bin"}); err == nil {
		t.Fatal("SHELL=launcher must be rejected (recursion)")
	}
}
