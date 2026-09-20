package client

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Pane shell environment adapter.
//
// Herdr's [terminal] default_shell must be an executable, not a command line,
// so Familiar points it at a small state-owned launcher (paths.PaneShell).
// That launcher is not a shell: it execs the user's own shell in a way that
// runs the user's normal interactive startup files first and then, as the very
// last step, prepends <runtime>/current/bin to PATH so a bare `pi` typed in a
// Herdr pane (or launched by Herdr on Familiar's behalf) resolves to the
// activated Familiar runtime regardless of what the user's rc files did.
//
// Ordering is the whole point: user rc first, Familiar PATH assertion last.
//
// Per-shell mechanism (all native, no Herdr patches):
//
//	bash  --rcfile <state>/shell/bashrc, which emulates bash's own file
//	      selection (login: /etc/profile + first of ~/.bash_profile,
//	      ~/.bash_login, ~/.profile; interactive: ~/.bashrc), then asserts.
//	zsh   ZDOTDIR=<state>/shell/zdotdir whose .zshenv/.zprofile/.zshrc/.zlogin
//	      source the user's real files from their original ZDOTDIR (or $HOME),
//	      assert last, and restore ZDOTDIR so nested shells are unaffected.
//	fish  fish -C '<assert>' runs after config.fish and conf.d by design.
//	sh family (dash, ksh, mksh, ash, busybox sh, bash-as-sh)
//	      ENV=<state>/shell/posix-env, which sources the user's original $ENV
//	      first and asserts last.
//	other exec the shell with PATH pre-prepended and a one-line warning that
//	      its startup files could still override PATH.
//
// Herdr's [terminal] shell_mode is fixed to "non_login" because Herdr's login
// mode on Unix launches $SHELL with a dash argv[0], which would both make
// $SHELL point at the launcher and hand login semantics to /bin/sh. The
// launcher therefore owns the login decision itself, mirroring Herdr's "auto"
// policy: login startup on macOS, non-login startup elsewhere.

// ShellFamily classifies the user's shell for launcher generation.
type ShellFamily string

const (
	ShellBash    ShellFamily = "bash"
	ShellZsh     ShellFamily = "zsh"
	ShellFish    ShellFamily = "fish"
	ShellPOSIX   ShellFamily = "posix" // sh, dash, ksh, mksh, ash, busybox
	ShellUnknown ShellFamily = "unknown"
)

func ClassifyShell(shell string) ShellFamily {
	switch filepath.Base(shell) {
	case "bash":
		return ShellBash
	case "zsh":
		return ShellZsh
	case "fish":
		return ShellFish
	case "sh", "dash", "ksh", "ksh93", "mksh", "oksh", "ash", "busybox":
		return ShellPOSIX
	}
	return ShellUnknown
}

// ResolveUserShell picks the shell the pane launcher should adapt around.
// It honours the parent's $SHELL, refuses to select a Familiar launcher
// (which would recurse), and otherwise falls back to /bin/sh with a note.
func ResolveUserShell(envShell string, paths Paths) (shell string, note string, err error) {
	shell = strings.TrimSpace(envShell)
	if shell == "" {
		return "/bin/sh", "SHELL is not set; panes will use /bin/sh", nil
	}
	if !filepath.IsAbs(shell) {
		return "", "", fmt.Errorf("SHELL=%q must be an absolute path", shell)
	}
	if strings.ContainsAny(shell, "\r\n'") {
		return "", "", fmt.Errorf("SHELL=%q contains unsupported characters", shell)
	}
	if shell == paths.PaneShell || filepath.Dir(shell) == paths.Dir {
		return "", "", fmt.Errorf("SHELL=%q is the Familiar pane launcher; set SHELL to your real shell before starting familiar-fleet", shell)
	}
	info, err := os.Stat(shell)
	if err != nil {
		return "", "", fmt.Errorf("SHELL=%q: %w", shell, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", "", fmt.Errorf("SHELL=%q is not an executable file", shell)
	}
	return shell, "", nil
}

// PaneShellSpec is everything needed to generate the launcher and rc files.
type PaneShellSpec struct {
	// UserShell is the absolute path of the shell to adapt around.
	UserShell string
	// RuntimeBin is the stable <state>/runtime/current/bin directory.
	RuntimeBin string
	// Login requests login-shell startup semantics from the user's shell.
	Login bool
}

// DefaultLogin mirrors Herdr's shell_mode="auto": login shells on macOS only.
func DefaultLogin() bool { return runtime.GOOS == "darwin" }

// PathAssertionSh is the POSIX-sh snippet that puts RuntimeBin first on PATH
// exactly once. It is the final statement of every generated startup file.
func pathAssertionSh(bin string) string {
	q := shellQuote(bin)
	return strings.Join([]string{
		`# Familiar: runtime/current/bin must win over anything the startup files above did.`,
		`case ":$PATH:" in`,
		`  ` + shellQuote(":"+bin+":") + `*) ;;`,
		`  *) PATH=` + q + `${PATH:+:$PATH} ;;`,
		`esac`,
		`export PATH`,
		`hash -r 2>/dev/null`,
	}, "\n")
}

func pathAssertionFish(bin string) string {
	q := shellQuote(bin)
	return `if test (count $PATH) -eq 0 -o "$PATH[1]" != ` + q + `; set -gx PATH ` + q + ` $PATH; end`
}

// GeneratePaneShell renders every file the launcher needs. Keys are relative
// to paths.Dir; values are contents. The launcher itself is executable.
func GeneratePaneShell(paths Paths, spec PaneShellSpec) (map[string]string, error) {
	if !filepath.IsAbs(spec.UserShell) || strings.ContainsAny(spec.UserShell, "\r\n'") {
		return nil, fmt.Errorf("invalid user shell %q", spec.UserShell)
	}
	if !filepath.IsAbs(spec.RuntimeBin) || strings.ContainsAny(spec.RuntimeBin, ":\r\n'") {
		return nil, fmt.Errorf("invalid runtime bin %q", spec.RuntimeBin)
	}
	shellDir := filepath.Join(paths.Dir, "shell")
	zdot := filepath.Join(shellDir, "zdotdir")
	files := map[string]string{}
	bin, sh := spec.RuntimeBin, spec.UserShell
	family := ClassifyShell(sh)
	login := spec.Login

	// --- launcher --------------------------------------------------------
	var launch []string
	launch = append(launch,
		"#!/bin/sh",
		"# Generated by familiar-fleet. Environment adapter around the user's shell,",
		"# not a replacement shell. Regenerated every time the Herdr server starts.",
		"FAMILIAR_RUNTIME_BIN="+shellQuote(bin),
		"export FAMILIAR_RUNTIME_BIN",
		"SHELL="+shellQuote(sh),
		"export SHELL",
		"if [ ! -x \"$FAMILIAR_RUNTIME_BIN/pi\" ]; then",
		"  echo 'familiar-fleet: warning: no activated Familiar runtime at '\"$FAMILIAR_RUNTIME_BIN\"'; run: familiar-fleet runtime apply <installable>' >&2",
		"fi",
	)
	switch family {
	case ShellBash:
		files["shell/bashrc"] = bashrc(bin)
		if login {
			launch = append(launch, "FAMILIAR_LOGIN=1", "export FAMILIAR_LOGIN")
		}
		// --rcfile applies only to interactive shells; bash still reads the
		// system-wide bashrc first. Login emulation happens inside the rcfile.
		launch = append(launch, "exec "+shellQuote(sh)+" --rcfile "+shellQuote(filepath.Join(shellDir, "bashrc"))+" -i \"$@\"")
	case ShellZsh:
		files["shell/zdotdir/.zshenv"] = zshenv(zdot)
		files["shell/zdotdir/.zprofile"] = zprofile()
		files["shell/zdotdir/.zshrc"] = zshrc(bin)
		files["shell/zdotdir/.zlogin"] = zlogin(bin)
		launch = append(launch,
			// Remember the user's ZDOTDIR before overriding it. Set-but-empty is
			// treated as unset, matching zsh's own fallback to $HOME.
			"if [ -n \"${ZDOTDIR:-}\" ] && [ \"$ZDOTDIR\" != "+shellQuote(zdot)+" ]; then",
			"  FAMILIAR_USER_ZDOTDIR=\"$ZDOTDIR\"",
			"elif [ \"${ZDOTDIR:-}\" != "+shellQuote(zdot)+" ] || [ -z \"${FAMILIAR_USER_ZDOTDIR:-}\" ]; then",
			"  FAMILIAR_USER_ZDOTDIR=\"${HOME:-/}\"",
			"fi",
			"export FAMILIAR_USER_ZDOTDIR",
			"ZDOTDIR="+shellQuote(zdot),
			"export ZDOTDIR",
		)
		if login {
			launch = append(launch, "exec "+shellQuote(sh)+" -l \"$@\"")
		} else {
			launch = append(launch, "exec "+shellQuote(sh)+" \"$@\"")
		}
	case ShellFish:
		flag := ""
		if login {
			flag = " -l"
		}
		// -C runs after config.fish and conf.d/*.fish but before the prompt.
		launch = append(launch, "exec "+shellQuote(sh)+flag+" -C "+shellQuote(pathAssertionFish(bin))+" \"$@\"")
	case ShellPOSIX:
		files["shell/posix-env"] = posixEnv(bin)
		launch = append(launch,
			"if [ -n \"${ENV:-}\" ] && [ \"$ENV\" != "+shellQuote(filepath.Join(shellDir, "posix-env"))+" ]; then",
			"  FAMILIAR_USER_ENV=\"$ENV\"",
			"  export FAMILIAR_USER_ENV",
			"fi",
			"ENV="+shellQuote(filepath.Join(shellDir, "posix-env")),
			"export ENV",
		)
		if login {
			launch = append(launch, "exec "+shellQuote(sh)+" -l \"$@\"")
		} else {
			launch = append(launch, "exec "+shellQuote(sh)+" \"$@\"")
		}
	default:
		launch = append(launch,
			"echo 'familiar-fleet: warning: unsupported shell '"+shellQuote(sh)+"'; PATH is pre-set and its startup files may override the Familiar runtime' >&2",
			pathAssertionSh(bin),
		)
		if login {
			launch = append(launch, "exec "+shellQuote(sh)+" -l \"$@\"")
		} else {
			launch = append(launch, "exec "+shellQuote(sh)+" \"$@\"")
		}
	}
	files["pane-shell"] = strings.Join(launch, "\n") + "\n"
	return files, nil
}

func bashrc(bin string) string {
	return strings.Join([]string{
		"# Generated by familiar-fleet. Sourced via `bash --rcfile` so the user's own",
		"# startup files run first (emulating bash's login/interactive selection);",
		"# the Familiar PATH assertion is the final statement.",
		"if [ -n \"${FAMILIAR_LOGIN:-}\" ]; then",
		"  unset FAMILIAR_LOGIN",
		"  [ -r /etc/profile ] && . /etc/profile",
		"  if [ -r \"$HOME/.bash_profile\" ]; then . \"$HOME/.bash_profile\"",
		"  elif [ -r \"$HOME/.bash_login\" ]; then . \"$HOME/.bash_login\"",
		"  elif [ -r \"$HOME/.profile\" ]; then . \"$HOME/.profile\"",
		"  fi",
		"  # Login bash does not read ~/.bashrc itself, but most ~/.bash_profile files",
		"  # source it. Do not source it here a second time.",
		"else",
		"  [ -r \"$HOME/.bashrc\" ] && . \"$HOME/.bashrc\"",
		"fi",
		pathAssertionSh(bin),
	}, "\n") + "\n"
}

// zshenv is read for every zsh invocation, so it defines the state used by the
// later files and sources the user's .zshenv (which may itself change ZDOTDIR).
func zshenv(zdot string) string {
	return strings.Join([]string{
		"# Generated by familiar-fleet. ZDOTDIR shim: user's files run first,",
		"# Familiar's PATH assertion runs last (in .zshrc or, for login shells, .zlogin).",
		"typeset -g _familiar_zdot=" + shellQuote(zdot),
		"typeset -g _familiar_user_zdot=\"${FAMILIAR_USER_ZDOTDIR:-${HOME:-/}}\"",
		"unset FAMILIAR_USER_ZDOTDIR",
		"# Expose the user's ZDOTDIR while their files run so they see what they expect.",
		"if [[ \"$_familiar_user_zdot\" == \"${HOME:-/}\" ]]; then unset ZDOTDIR; else export ZDOTDIR=\"$_familiar_user_zdot\"; fi",
		"[[ -r \"$_familiar_user_zdot/.zshenv\" ]] && source \"$_familiar_user_zdot/.zshenv\"",
		"# Their .zshenv may have moved ZDOTDIR; follow it for the remaining files.",
		"[[ -n \"${ZDOTDIR:-}\" && \"$ZDOTDIR\" != \"$_familiar_zdot\" ]] && _familiar_user_zdot=\"$ZDOTDIR\"",
		"export ZDOTDIR=\"$_familiar_zdot\"",
		"",
	}, "\n")
}

func zprofile() string {
	return strings.Join([]string{
		"# Generated by familiar-fleet. Login shells only.",
		"if [[ \"$_familiar_user_zdot\" == \"${HOME:-/}\" ]]; then unset ZDOTDIR; else export ZDOTDIR=\"$_familiar_user_zdot\"; fi",
		"[[ -r \"$_familiar_user_zdot/.zprofile\" ]] && source \"$_familiar_user_zdot/.zprofile\"",
		"export ZDOTDIR=\"$_familiar_zdot\"",
		"",
	}, "\n")
}

func zshFinalize(bin string) string {
	return strings.Join([]string{
		"# Restore the user's ZDOTDIR so nested shells and .zlogout behave normally.",
		"if [[ \"$_familiar_user_zdot\" == \"${HOME:-/}\" ]]; then unset ZDOTDIR; else export ZDOTDIR=\"$_familiar_user_zdot\"; fi",
		"unset _familiar_zdot _familiar_user_zdot",
		pathAssertionSh(bin),
	}, "\n")
}

func zshrc(bin string) string {
	return strings.Join([]string{
		"# Generated by familiar-fleet. Interactive shells.",
		"if [[ \"$_familiar_user_zdot\" == \"${HOME:-/}\" ]]; then unset ZDOTDIR; else export ZDOTDIR=\"$_familiar_user_zdot\"; fi",
		"[[ -r \"$_familiar_user_zdot/.zshrc\" ]] && source \"$_familiar_user_zdot/.zshrc\"",
		"if [[ -o login ]]; then",
		"  # .zlogin still has to run (and may touch PATH); it finalizes instead.",
		"  export ZDOTDIR=\"$_familiar_zdot\"",
		"else",
		zshFinalize(bin),
		"fi",
		"",
	}, "\n")
}

func zlogin(bin string) string {
	return strings.Join([]string{
		"# Generated by familiar-fleet. Login shells, after .zshrc.",
		"if [[ \"$_familiar_user_zdot\" == \"${HOME:-/}\" ]]; then unset ZDOTDIR; else export ZDOTDIR=\"$_familiar_user_zdot\"; fi",
		"[[ -r \"$_familiar_user_zdot/.zlogin\" ]] && source \"$_familiar_user_zdot/.zlogin\"",
		zshFinalize(bin),
		"",
	}, "\n")
}

func posixEnv(bin string) string {
	return strings.Join([]string{
		"# Generated by familiar-fleet. Sourced by interactive POSIX shells via $ENV.",
		"# The user's original $ENV runs first; the Familiar PATH assertion is last.",
		"if [ -n \"${FAMILIAR_USER_ENV:-}\" ]; then",
		"  ENV=\"$FAMILIAR_USER_ENV\"; export ENV",
		"  unset FAMILIAR_USER_ENV",
		"  [ -r \"$ENV\" ] && . \"$ENV\"",
		"else",
		"  unset ENV",
		"fi",
		pathAssertionSh(bin),
	}, "\n") + "\n"
}

// WritePaneShell installs the launcher and its rc files under paths.Dir,
// removing stale files from a previous shell family so generation is exact.
func WritePaneShell(paths Paths, spec PaneShellSpec) error {
	files, err := GeneratePaneShell(paths, spec)
	if err != nil {
		return err
	}
	shellDir := filepath.Join(paths.Dir, "shell")
	if err := os.RemoveAll(shellDir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(shellDir, "zdotdir"), 0700); err != nil {
		return err
	}
	for rel, content := range files {
		mode := os.FileMode(0600)
		if rel == "pane-shell" {
			mode = 0700
		}
		if err := atomicWrite(filepath.Join(paths.Dir, rel), []byte(content), mode); err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}
	}
	return nil
}

func tomlString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// MergeHerdrConfig returns the user's config.toml with Familiar's
// default_shell and shell_mode applied inside its [terminal] table. Other
// [terminal] keys and every other table are preserved verbatim, so the user's
// theme, sidebar, workspaces, and agent settings still apply to the server.
// This mirrors Herdr's own line-based config editing rather than reformatting.
func MergeHerdrConfig(userConfig, paneShell string) (string, error) {
	if strings.ContainsRune(userConfig, 0) {
		return "", errors.New("user Herdr config contains NUL")
	}
	managed := []string{
		"# Managed by familiar-fleet: environment adapter around the user's shell.",
		"default_shell = " + tomlString(paneShell),
		`shell_mode = "non_login"`,
	}
	lines := strings.Split(strings.TrimRight(userConfig, "\n"), "\n")
	if userConfig == "" {
		lines = nil
	}
	var out []string
	inTerminal := false
	found := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if name, ok := tomlTableHeader(trimmed); ok {
			inTerminal = name == "terminal"
			out = append(out, line)
			if inTerminal {
				found = true
				out = append(out, managed...)
			}
			continue
		}
		key := tomlKey(trimmed)
		if inTerminal && (key == "default_shell" || key == "shell_mode") {
			continue // replaced by the managed values inserted under the header
		}
		if !inTerminal && (key == "terminal.default_shell" || key == "terminal.shell_mode") {
			continue // dotted form at document root
		}
		out = append(out, line)
	}
	if !found {
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, "[terminal]")
		out = append(out, managed...)
	}
	return strings.Join(out, "\n") + "\n", nil
}

func tomlTableHeader(trimmed string) (string, bool) {
	if !strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "[[") {
		return "", false
	}
	end := strings.Index(trimmed, "]")
	if end < 0 {
		return "", false
	}
	return strings.TrimSpace(trimmed[1:end]), true
}

func tomlKey(trimmed string) string {
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return ""
	}
	eq := strings.Index(trimmed, "=")
	if eq < 0 {
		return ""
	}
	return strings.TrimSpace(trimmed[:eq])
}
