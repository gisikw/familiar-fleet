package client

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func testPaths(t *testing.T) Paths {
	t.Helper()
	p, err := StatePaths(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureStateDir(p.Dir); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeRuntimeBin creates <tmp>/runtime/current/bin/pi so the launcher's
// runtime check passes and PATH resolution can be observed.
func fakeRuntimeBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "runtime", "current", "bin")
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "pi"), []byte("#!/bin/sh\necho familiar-pi\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// ambientBin creates a decoy pi that user rc files put first on PATH, to
// prove the Familiar assertion still wins.
func ambientBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "ambient")
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "pi"), []byte("#!/bin/sh\necho ambient-pi\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestClassifyShell(t *testing.T) {
	cases := map[string]ShellFamily{
		"/bin/bash": ShellBash, "/usr/local/bin/zsh": ShellZsh, "/opt/homebrew/bin/fish": ShellFish,
		"/bin/sh": ShellPOSIX, "/bin/dash": ShellPOSIX, "/usr/bin/ksh": ShellPOSIX, "/bin/mksh": ShellPOSIX,
		"/usr/bin/nu": ShellUnknown, "/bin/tcsh": ShellUnknown,
	}
	for shell, want := range cases {
		if got := ClassifyShell(shell); got != want {
			t.Errorf("%s: got %s want %s", shell, got, want)
		}
	}
}

func TestResolveUserShell(t *testing.T) {
	p := testPaths(t)
	if shell, note, err := ResolveUserShell("", p); err != nil || shell != "/bin/sh" || note == "" {
		t.Fatalf("empty SHELL: %q %q %v", shell, note, err)
	}
	if shell, note, err := ResolveUserShell(" /bin/sh ", p); err != nil || shell != "/bin/sh" || note != "" {
		t.Fatalf("/bin/sh: %q %q %v", shell, note, err)
	}
	for name, shell := range map[string]string{
		"relative":       "bash",
		"launcher":       p.PaneShell,
		"state-dir file": filepath.Join(p.Dir, "herdr"),
		"missing":        filepath.Join(t.TempDir(), "nope"),
		"quote":          "/bin/it's",
	} {
		if _, _, err := ResolveUserShell(shell, p); err == nil {
			t.Errorf("%s (%s): expected error", name, shell)
		}
	}
	notExec := filepath.Join(t.TempDir(), "notexec")
	if err := os.WriteFile(notExec, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ResolveUserShell(notExec, p); err == nil {
		t.Error("non-executable SHELL accepted")
	}
}

func TestGeneratePaneShellOrderingAndContent(t *testing.T) {
	p := testPaths(t)
	bin := "/nix/var/state/runtime/current/bin"
	for _, tc := range []struct {
		shell   string
		login   bool
		files   []string
		launch  []string
		absent  []string
		ordered [][2]string // in file: first must precede second
	}{
		{
			shell: "/bin/bash", login: false,
			files:  []string{"shell/bashrc"},
			launch: []string{"exec '/bin/bash' --rcfile '" + p.Dir + "/shell/bashrc' -i \"$@\"", "SHELL='/bin/bash'"},
			absent: []string{"FAMILIAR_LOGIN=1"},
		},
		{
			shell: "/bin/bash", login: true,
			launch: []string{"FAMILIAR_LOGIN=1"},
		},
		{
			shell: "/usr/bin/zsh", login: false,
			files:  []string{"shell/zdotdir/.zshenv", "shell/zdotdir/.zprofile", "shell/zdotdir/.zshrc", "shell/zdotdir/.zlogin"},
			launch: []string{"ZDOTDIR='" + p.Dir + "/shell/zdotdir'", "exec '/usr/bin/zsh' \"$@\""},
		},
		{
			shell: "/usr/bin/zsh", login: true,
			launch: []string{"exec '/usr/bin/zsh' -l \"$@\""},
		},
		{
			shell: "/usr/bin/fish", login: false,
			launch: []string{"exec '/usr/bin/fish' -C '", `set -gx PATH '\''` + bin + `'\'' $PATH`},
			absent: []string{" -l "},
		},
		{
			shell: "/usr/bin/fish", login: true,
			launch: []string{"exec '/usr/bin/fish' -l -C '"},
		},
		{
			shell: "/bin/dash", login: false,
			files:  []string{"shell/posix-env"},
			launch: []string{"ENV='" + p.Dir + "/shell/posix-env'", "FAMILIAR_USER_ENV=\"$ENV\"", "exec '/bin/dash' \"$@\""},
		},
		{
			shell: "/usr/bin/nu", login: false,
			launch: []string{"unsupported shell", "PATH='" + bin + "'${PATH:+:$PATH}", "exec '/usr/bin/nu' \"$@\""},
		},
	} {
		files, err := GeneratePaneShell(p, PaneShellSpec{UserShell: tc.shell, RuntimeBin: bin, Login: tc.login})
		if err != nil {
			t.Fatalf("%s login=%v: %v", tc.shell, tc.login, err)
		}
		launcher := files["pane-shell"]
		if !strings.HasPrefix(launcher, "#!/bin/sh\n") {
			t.Errorf("%s: launcher must be a /bin/sh script", tc.shell)
		}
		for _, f := range tc.files {
			if _, ok := files[f]; !ok {
				t.Errorf("%s: missing generated %s (have %v)", tc.shell, f, keys(files))
			}
		}
		for _, want := range tc.launch {
			if !strings.Contains(launcher, want) {
				t.Errorf("%s login=%v: launcher missing %q:\n%s", tc.shell, tc.login, want, launcher)
			}
		}
		for _, absent := range tc.absent {
			if strings.Contains(launcher, absent) {
				t.Errorf("%s login=%v: launcher must not contain %q", tc.shell, tc.login, absent)
			}
		}
		if !strings.Contains(launcher, "FAMILIAR_RUNTIME_BIN='"+bin+"'") {
			t.Errorf("%s: launcher does not export runtime bin", tc.shell)
		}
	}

	// Startup ordering: every rc file sources the user's file before the
	// Familiar PATH assertion, and the assertion is the last statement.
	files, _ := GeneratePaneShell(p, PaneShellSpec{UserShell: "/bin/bash", RuntimeBin: bin})
	assertOrdered(t, "bashrc", files["shell/bashrc"], `"$HOME/.bashrc"`, "Familiar: runtime/current/bin must win")
	assertOrdered(t, "bashrc", files["shell/bashrc"], `"$HOME/.bash_profile"`, `"$HOME/.bashrc"`)
	assertOrdered(t, "bashrc", files["shell/bashrc"], "/etc/profile", `"$HOME/.bash_profile"`)
	assertLast(t, "bashrc", files["shell/bashrc"], "hash -r 2>/dev/null")

	files, _ = GeneratePaneShell(p, PaneShellSpec{UserShell: "/bin/zsh", RuntimeBin: bin})
	assertOrdered(t, ".zshrc", files["shell/zdotdir/.zshrc"], `source "$_familiar_user_zdot/.zshrc"`, "Familiar: runtime/current/bin must win")
	assertOrdered(t, ".zlogin", files["shell/zdotdir/.zlogin"], `source "$_familiar_user_zdot/.zlogin"`, "Familiar: runtime/current/bin must win")
	assertOrdered(t, ".zshenv", files["shell/zdotdir/.zshenv"], `source "$_familiar_user_zdot/.zshenv"`, `export ZDOTDIR="$_familiar_zdot"`)
	if strings.Contains(files["shell/zdotdir/.zshenv"], "Familiar: runtime/current/bin must win") || strings.Contains(files["shell/zdotdir/.zprofile"], "Familiar: runtime/current/bin must win") {
		t.Error("PATH must be asserted only after .zshrc/.zlogin, never in .zshenv/.zprofile")
	}

	files, _ = GeneratePaneShell(p, PaneShellSpec{UserShell: "/bin/dash", RuntimeBin: bin})
	assertOrdered(t, "posix-env", files["shell/posix-env"], `. "$ENV"`, "Familiar: runtime/current/bin must win")
	assertLast(t, "posix-env", files["shell/posix-env"], "hash -r 2>/dev/null")
}

func TestGeneratePaneShellRejectsUnsafeInput(t *testing.T) {
	p := testPaths(t)
	for _, spec := range []PaneShellSpec{
		{UserShell: "bash", RuntimeBin: "/r/bin"},
		{UserShell: "/bin/bash", RuntimeBin: "r/bin"},
		{UserShell: "/bin/bash", RuntimeBin: "/r:x/bin"},
		{UserShell: "/bin/ba'sh", RuntimeBin: "/r/bin"},
	} {
		if _, err := GeneratePaneShell(p, spec); err == nil {
			t.Errorf("%+v accepted", spec)
		}
	}
}

func TestWritePaneShellReplacesStaleFamilyFiles(t *testing.T) {
	p := testPaths(t)
	bin := fakeRuntimeBin(t)
	if err := WritePaneShell(p, PaneShellSpec{UserShell: "/bin/sh", RuntimeBin: bin}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.Dir, "shell", "posix-env")); err != nil {
		t.Fatal(err)
	}
	if err := WritePaneShell(p, PaneShellSpec{UserShell: "/bin/bash", RuntimeBin: bin}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.Dir, "shell", "posix-env")); !os.IsNotExist(err) {
		t.Fatalf("stale posix-env not removed: %v", err)
	}
	info, err := os.Stat(p.PaneShell)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("launcher mode: %v %v", info, err)
	}
	rc, err := os.Stat(filepath.Join(p.Dir, "shell", "bashrc"))
	if err != nil || rc.Mode().Perm() != 0600 {
		t.Fatalf("rc mode: %v %v", rc, err)
	}
}

func TestMergeHerdrConfig(t *testing.T) {
	launcher := "/home/u/.local/state/familiar-fleet/pane-shell"
	got, err := MergeHerdrConfig("", launcher)
	if err != nil {
		t.Fatal(err)
	}
	want := "[terminal]\n# Managed by familiar-fleet: environment adapter around the user's shell.\ndefault_shell = \"" + launcher + "\"\nshell_mode = \"non_login\"\n"
	if got != want {
		t.Fatalf("empty config:\n%s\nwant:\n%s", got, want)
	}

	user := "# my config\n[ui]\nsidebar_width = 30\n\n[terminal]\ndefault_shell = \"nu\"\nkitty_graphics = false\nshell_mode=\"login\"\n\n[worktrees]\ndirectory = \"~/wt\"\n"
	got, err = MergeHerdrConfig(user, launcher)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"sidebar_width = 30", "kitty_graphics = false", "directory = \"~/wt\"", "default_shell = \"" + launcher + "\"", "shell_mode = \"non_login\""} {
		if !strings.Contains(got, s) {
			t.Errorf("merged config missing %q:\n%s", s, got)
		}
	}
	for _, s := range []string{`"nu"`, `"login"`} {
		if strings.Contains(got, s) {
			t.Errorf("merged config kept user's %s:\n%s", s, got)
		}
	}
	if strings.Count(got, "[terminal]") != 1 {
		t.Errorf("duplicate [terminal] table:\n%s", got)
	}
	assertOrdered(t, "merged", got, "[terminal]", "default_shell = ")
	assertOrdered(t, "merged", got, "shell_mode = \"non_login\"", "kitty_graphics")
	assertOrdered(t, "merged", got, "kitty_graphics", "[worktrees]")

	// Dotted root keys are also replaced; [[array]] tables are not confused with headers.
	got, err = MergeHerdrConfig("terminal.default_shell = \"nu\"\n[[keys.commands]]\nkey = \"x\"\n", launcher)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, `"nu"`) || !strings.Contains(got, "[[keys.commands]]") || strings.Count(got, "[terminal]") != 1 {
		t.Fatalf("dotted/array handling:\n%s", got)
	}

	// The merged realistic config must be valid for the pinned Herdr if one is available.
	if herdr, err := exec.LookPath("herdr"); err == nil {
		content, _ := MergeHerdrConfig(user, launcher)
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(herdr, "config", "check")
		cmd.Env = append(os.Environ(), "HERDR_CONFIG_PATH="+path)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("herdr config check rejected merged config: %v\n%s\n%s", err, out, content)
		}
	}
}

// --- live shell tests ------------------------------------------------------
//
// Each test writes a user rc file that deliberately puts a decoy `pi` first
// on PATH, records that it ran, and (where possible) defines an alias. It then
// runs the launcher interactively and checks: the user's rc ran, the alias
// survived, and `command -v pi` resolves to the Familiar runtime.

type liveShellResult struct{ out string }

func runLauncher(t *testing.T, p Paths, shell string, login bool, home string, script string, args ...string) liveShellResult {
	t.Helper()
	bin := fakeRuntimeBin(t)
	if err := WritePaneShell(p, PaneShellSpec{UserShell: shell, RuntimeBin: bin, Login: login}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(p.PaneShell, args...)
	cmd.Stdin = strings.NewReader(script + "exit\n")
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=" + ambientPathFor(t),
		"TERM=dumb",
		"USER=tester",
		"FAMILIAR_TEST_RUNTIME_BIN=" + bin,
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: launcher failed: %v\n%s", shell, err, out)
	}
	return liveShellResult{out: string(out)}
}

// ambientPathFor builds a PATH containing the shells' directories, basic
// utilities the rc files use, and the ambient decoy so `pi` resolves to the
// decoy before the assertion runs.
func ambientPathFor(t *testing.T) string {
	t.Helper()
	dirs := []string{ambientBin(t)}
	for _, name := range []string{"bash", "zsh", "fish", "dash", "sh", "dirname", "cut"} {
		if path, err := exec.LookPath(name); err == nil {
			dirs = append(dirs, filepath.Dir(path))
		}
	}
	dirs = append(dirs, "/usr/bin", "/bin")
	return strings.Join(dirs, ":")
}

func requireShell(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not available", name)
	}
	return path
}

func expectLiveResult(t *testing.T, shell string, r liveShellResult, extra ...string) {
	t.Helper()
	for _, want := range append([]string{"user-rc-ran", "familiar-pi", "alias-works"}, extra...) {
		if !strings.Contains(r.out, want) {
			t.Errorf("%s: output missing %q:\n%s", shell, want, r.out)
		}
	}
	if strings.Contains(r.out, "ambient-pi") {
		t.Errorf("%s: ambient pi resolved instead of Familiar runtime:\n%s", shell, r.out)
	}
	if strings.Contains(r.out, "warning") {
		t.Errorf("%s: unexpected warning:\n%s", shell, r.out)
	}
}

// probe is typed into the pane shell. Shells under test get -i where their
// interactive startup depends on it, since stdin is a pipe rather than the
// PTY Herdr would provide.
const probe = "echo path-first=$(printf '%s' \"$PATH\" | cut -d: -f1)\nhello\npi\ncommand -v pi\n"

func TestLiveBashInteractive(t *testing.T) {
	bash := requireShell(t, "bash")
	p := testPaths(t)
	home := t.TempDir()
	rc := "echo user-rc-ran\nPATH=\"$(dirname \"$(command -v pi)\"):$PATH\"\nalias hello='echo alias-works'\nexport MARK=bashrc\n"
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte(rc), 0600); err != nil {
		t.Fatal(err)
	}
	r := runLauncher(t, p, bash, false, home, probe)
	expectLiveResult(t, "bash", r, "/runtime/current/bin/pi")
	if !strings.Contains(r.out, "path-first=") || !strings.Contains(r.out, "/runtime/current/bin\n") {
		t.Errorf("bash: runtime bin not first on PATH:\n%s", r.out)
	}
}

func TestLiveBashLoginEmulation(t *testing.T) {
	bash := requireShell(t, "bash")
	p := testPaths(t)
	home := t.TempDir()
	profile := "echo user-rc-ran\nPATH=\"$(dirname \"$(command -v pi)\"):$PATH\"\n[ -r \"$HOME/.bashrc\" ] && . \"$HOME/.bashrc\"\n"
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte(profile), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte("alias hello='echo alias-works'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := runLauncher(t, p, bash, true, home, probe)
	expectLiveResult(t, "bash -l", r)
	if strings.Count(r.out, "user-rc-ran") != 1 {
		t.Errorf("bash -l: profile sourced more than once:\n%s", r.out)
	}
}

func TestLiveZshInteractiveAndLogin(t *testing.T) {
	zsh := requireShell(t, "zsh")
	for _, login := range []bool{false, true} {
		p := testPaths(t)
		home := t.TempDir()
		files := map[string]string{
			".zshenv":   "echo zshenv-ran\n",
			".zprofile": "echo zprofile-ran\n",
			".zshrc":    "echo user-rc-ran\npath=($(dirname $(command -v pi)) $path)\nalias hello='echo alias-works'\necho zdotdir-in-rc=${ZDOTDIR:-unset}\n",
			".zlogin":   "echo zlogin-ran\npath=($(dirname $(command -v pi)) $path)\n",
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(home, name), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
		r := runLauncher(t, p, zsh, login, home, probe+"echo zdotdir-after=${ZDOTDIR:-unset}\n", "-i")
		label := "zsh"
		if login {
			label = "zsh -l"
		}
		expectLiveResult(t, label, r, "zshenv-ran", "zdotdir-in-rc=unset", "zdotdir-after=unset")
		if login != strings.Contains(r.out, "zlogin-ran") || login != strings.Contains(r.out, "zprofile-ran") {
			t.Errorf("%s: login files ran=%v:\n%s", label, !login, r.out)
		}
		// User's .zlogin re-prepends the ambient dir after .zshrc; Familiar must still win.
		if !strings.Contains(r.out, "/runtime/current/bin/pi") {
			t.Errorf("%s: pi did not resolve to runtime:\n%s", label, r.out)
		}
	}
}

func TestLiveZshCustomZDOTDIRIsPreserved(t *testing.T) {
	zsh := requireShell(t, "zsh")
	p := testPaths(t)
	home := t.TempDir()
	custom := filepath.Join(home, "cfg", "zsh")
	if err := os.MkdirAll(custom, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(custom, ".zshrc"), []byte("echo user-rc-ran\nalias hello='echo alias-works'\necho zdotdir-in-rc=$ZDOTDIR\n"), 0600); err != nil {
		t.Fatal(err)
	}
	bin := fakeRuntimeBin(t)
	if err := WritePaneShell(p, PaneShellSpec{UserShell: zsh, RuntimeBin: bin}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(p.PaneShell, "-i")
	cmd.Stdin = strings.NewReader(probe + "echo zdotdir-after=$ZDOTDIR\nexit\n")
	cmd.Env = []string{"HOME=" + home, "PATH=" + ambientPathFor(t), "TERM=dumb", "ZDOTDIR=" + custom}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	expectLiveResult(t, "zsh ZDOTDIR", liveShellResult{string(out)}, "zdotdir-in-rc="+custom, "zdotdir-after="+custom)
}

func TestLiveFish(t *testing.T) {
	fish := requireShell(t, "fish")
	p := testPaths(t)
	home := t.TempDir()
	cfg := filepath.Join(home, ".config", "fish")
	if err := os.MkdirAll(cfg, 0700); err != nil {
		t.Fatal(err)
	}
	rc := "echo user-rc-ran\nset -gx PATH (dirname (command -v pi)) $PATH\nalias hello 'echo alias-works'\n"
	if err := os.WriteFile(filepath.Join(cfg, "config.fish"), []byte(rc), 0600); err != nil {
		t.Fatal(err)
	}
	script := "echo path-first=$PATH[1]\nhello\npi\ncommand -v pi\n"
	r := runLauncher(t, p, fish, false, home, script)
	expectLiveResult(t, "fish", r, "/runtime/current/bin/pi")
}

func TestLiveDashViaENV(t *testing.T) {
	dash := requireShell(t, "dash")
	p := testPaths(t)
	home := t.TempDir()
	userEnv := filepath.Join(home, ".shrc")
	rc := "echo user-rc-ran\nPATH=\"$(dirname \"$(command -v pi)\"):$PATH\"\nalias hello='echo alias-works'\necho env-in-rc=$ENV\n"
	if err := os.WriteFile(userEnv, []byte(rc), 0600); err != nil {
		t.Fatal(err)
	}
	bin := fakeRuntimeBin(t)
	if err := WritePaneShell(p, PaneShellSpec{UserShell: dash, RuntimeBin: bin}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(p.PaneShell, "-i")
	cmd.Stdin = strings.NewReader(probe + "echo env-after=$ENV\nexit\n")
	cmd.Env = []string{"HOME=" + home, "PATH=" + ambientPathFor(t), "TERM=dumb", "ENV=" + userEnv}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	expectLiveResult(t, "dash", liveShellResult{string(out)}, "env-in-rc="+userEnv, "env-after="+userEnv, "/runtime/current/bin/pi")
}

func TestLiveLauncherWarnsWithoutRuntime(t *testing.T) {
	p := testPaths(t)
	if err := WritePaneShell(p, PaneShellSpec{UserShell: "/bin/sh", RuntimeBin: filepath.Join(t.TempDir(), "missing", "bin")}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(p.PaneShell, "-c", "echo shell-still-usable")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "warning: no activated Familiar runtime") || !strings.Contains(string(out), "shell-still-usable") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func assertOrdered(t *testing.T, name, content, first, second string) {
	t.Helper()
	i, j := strings.Index(content, first), strings.Index(content, second)
	if i < 0 || j < 0 {
		t.Fatalf("%s: missing %q (%d) or %q (%d):\n%s", name, first, i, second, j, content)
	}
	if i > j {
		t.Errorf("%s: %q must come before %q:\n%s", name, first, second, content)
	}
}

func assertLast(t *testing.T, name, content, last string) {
	t.Helper()
	trimmed := strings.TrimRight(content, "\n")
	if !strings.HasSuffix(trimmed, last) {
		t.Errorf("%s: final statement must be %q:\n%s", name, last, content)
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
