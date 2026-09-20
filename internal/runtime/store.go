// Package runtime manages node-local activation of immutable Familiar runtime
// outputs. A runtime is a directory (normally a Nix store path such as the
// output of github:gisikw/familiar/<commit>#familiar-worker-runtime) whose
// bin/ contains the tools Herdr panes must resolve first, most importantly pi.
//
// The store keeps two stable pointers under <state>/runtime:
//
//	current  -> the runtime Herdr panes and the Herdr server use
//	previous -> the runtime current pointed at before the last activation
//
// Because Herdr's PATH references the stable current pointer rather than a
// store path, activating a new runtime never requires restarting Herdr.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	CurrentName  = "current"
	PreviousName = "previous"
	// RequiredTool is the executable a runtime must provide to be activatable.
	RequiredTool = "pi"
)

// Store is the node-local runtime pointer directory.
type Store struct{ Dir string }

func New(dir string) Store { return Store{Dir: dir} }

func (s Store) Current() string  { return filepath.Join(s.Dir, CurrentName) }
func (s Store) Previous() string { return filepath.Join(s.Dir, PreviousName) }

// CurrentBin is the stable PATH entry that Herdr and pane shells reference.
func (s Store) CurrentBin() string { return filepath.Join(s.Current(), "bin") }
func (s Store) GCRoots() string    { return filepath.Join(s.Dir, "gcroots") }

// ErrNoRuntime reports that no runtime has been activated on this node.
var ErrNoRuntime = errors.New("no Familiar runtime is activated")

// Validate checks that target is an absolute directory that provides an
// executable bin/pi. It does not follow the pointer names; callers pass the
// candidate directory itself.
func Validate(target string) error {
	if !filepath.IsAbs(target) {
		return fmt.Errorf("runtime %q is not an absolute path", target)
	}
	if strings.ContainsAny(target, ":\r\n") || strings.ContainsAny(target, "'\"$`\\") {
		return fmt.Errorf("runtime path %q contains characters that are unsafe on PATH or in shell configuration", target)
	}
	info, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("runtime %s: %w", target, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("runtime %s is not a directory", target)
	}
	tool := filepath.Join(target, "bin", RequiredTool)
	toolInfo, err := os.Stat(tool)
	if err != nil {
		return fmt.Errorf("runtime %s does not provide bin/%s: %w", target, RequiredTool, err)
	}
	if !toolInfo.Mode().IsRegular() || toolInfo.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("runtime %s: bin/%s is not an executable file", target, RequiredTool)
	}
	return nil
}

// Smoke runs the required tool with --version under a short timeout so a
// runtime that cannot even start is never activated.
func Smoke(ctx context.Context, target string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tool := filepath.Join(target, "bin", RequiredTool)
	cmd := exec.CommandContext(ctx, tool, "--version")
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(target, "bin")+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s --version failed: %w: %s", tool, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Pointer reads one pointer. It returns ErrNoRuntime when the pointer does not
// exist, and an error when it exists but is not a symlink.
func (s Store) Pointer(name string) (string, error) {
	link := filepath.Join(s.Dir, name)
	info, err := os.Lstat(link)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoRuntime
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("%s exists but is not a symlink; remove it and re-run runtime apply", link)
	}
	target, err := os.Readlink(link)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(target) {
		return "", fmt.Errorf("%s points at relative path %q", link, target)
	}
	return target, nil
}

// Status describes both pointers plus validation of current.
type Status struct {
	Dir, Current, Previous  string
	CurrentErr, PreviousErr error
}

func (s Store) Status() Status {
	st := Status{Dir: s.Dir}
	st.Current, st.CurrentErr = s.Pointer(CurrentName)
	if st.CurrentErr == nil {
		st.CurrentErr = Validate(st.Current)
	}
	st.Previous, st.PreviousErr = s.Pointer(PreviousName)
	if st.PreviousErr == nil {
		st.PreviousErr = Validate(st.Previous)
	}
	return st
}

// Ready returns the validated current runtime or a clear error explaining
// how to activate one. Herdr must not start without it.
func (s Store) Ready() (string, error) {
	current, err := s.Pointer(CurrentName)
	if errors.Is(err, ErrNoRuntime) {
		return "", fmt.Errorf("%w; run: familiar-fleet runtime apply github:gisikw/familiar/<commit>#familiar-worker-runtime", ErrNoRuntime)
	}
	if err != nil {
		return "", err
	}
	if err := Validate(current); err != nil {
		return "", fmt.Errorf("activated runtime is unusable (%w); run: familiar-fleet runtime rollback, or runtime apply a known-good installable", err)
	}
	return current, nil
}

// Activate atomically points current at target, moving the old current to
// previous. It is idempotent: activating the already-current target changes
// nothing. Every step is a symlink creation followed by rename, so a crash at
// any point leaves both pointers either untouched or fully updated.
func (s Store) Activate(target string) (changed bool, err error) {
	target = filepath.Clean(target)
	if err := Validate(target); err != nil {
		return false, err
	}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return false, err
	}
	old, err := s.Pointer(CurrentName)
	switch {
	case errors.Is(err, ErrNoRuntime):
		old = ""
	case err != nil:
		return false, err
	case old == target:
		return false, nil
	}
	if old != "" {
		if err := replaceSymlink(s.Previous(), old); err != nil {
			return false, fmt.Errorf("record previous runtime: %w", err)
		}
	}
	if err := replaceSymlink(s.Current(), target); err != nil {
		return false, fmt.Errorf("activate runtime: %w", err)
	}
	return true, nil
}

// Rollback swaps current and previous. It refuses to roll back onto an
// invalid previous runtime, leaving the current pointer untouched.
func (s Store) Rollback() (string, error) {
	previous, err := s.Pointer(PreviousName)
	if errors.Is(err, ErrNoRuntime) {
		return "", errors.New("no previous runtime to roll back to")
	}
	if err != nil {
		return "", err
	}
	if err := Validate(previous); err != nil {
		return "", fmt.Errorf("refusing rollback: %w", err)
	}
	if _, err := s.Activate(previous); err != nil {
		return "", err
	}
	return previous, nil
}

// replaceSymlink atomically makes link point at target using a temporary
// symlink and rename(2). It refuses to replace anything that is not a symlink.
func replaceSymlink(link, target string) error {
	if info, err := os.Lstat(link); err == nil && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("%s exists and is not a symlink", link)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(link), ".tmp-link-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	tmpLink := filepath.Join(tmp, "link")
	if err := os.Symlink(target, tmpLink); err != nil {
		return err
	}
	return os.Rename(tmpLink, link)
}

// RegisterGCRoot asks Nix to retain target through an indirect root beneath
// the state directory. Ordinary symlinks outside Nix's gcroots are not roots;
// without this step a routine `nix store gc` could remove current or previous.
func (s Store) RegisterGCRoot(ctx context.Context, nixStorePath, target string) error {
	target = filepath.Clean(target)
	if filepath.Dir(target) != "/nix/store" {
		return fmt.Errorf("runtime %s is not an immutable /nix/store output", target)
	}
	if err := os.MkdirAll(s.GCRoots(), 0700); err != nil {
		return err
	}
	root := filepath.Join(s.GCRoots(), filepath.Base(target))
	if existing, err := os.Readlink(root); err == nil {
		if existing == target {
			return nil
		}
		return fmt.Errorf("GC root %s points at unexpected target %s", root, existing)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect GC root %s: %w", root, err)
	}
	cmd := exec.CommandContext(ctx, nixStorePath, "--add-root", root, "--indirect", "--realise", target)
	// Modern Nix installs nix-store as a multicall symlink to `nix`. Our
	// executable resolver canonicalizes symlinks, so preserve the legacy
	// frontend selection explicitly through argv[0].
	cmd.Args[0] = "nix-store"
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("register Nix GC root for %s: %w: %s", target, err, strings.TrimSpace(string(out)))
	}
	if existing, err := os.Readlink(root); err != nil || existing != target {
		return fmt.Errorf("nix-store did not create expected GC root %s -> %s", root, target)
	}
	return nil
}

// PruneGCRoots retains roots for exactly current and previous. Nix's indirect
// auto-root entries may briefly point at removed links; Nix ignores and later
// cleans those dangling registrations.
func (s Store) PruneGCRoots() error {
	keep := map[string]bool{}
	for _, name := range []string{CurrentName, PreviousName} {
		if target, err := s.Pointer(name); err == nil {
			keep[target] = true
		} else if !errors.Is(err, ErrNoRuntime) {
			return err
		}
	}
	entries, err := os.ReadDir(s.GCRoots())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(s.GCRoots(), entry.Name())
		target, err := os.Readlink(path)
		if err != nil {
			return fmt.Errorf("GC root %s is not a readable symlink: %w", path, err)
		}
		if !keep[target] {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	}
	return nil
}

// Build realizes a Nix flake installable without creating a result link and
// returns its single output path. The caller registers the validated output as
// a GC root before activation. Installables that are already absolute
// directories are returned as-is so pre-built store paths can be activated.
func Build(ctx context.Context, nixPath, installable string) (string, error) {
	if filepath.IsAbs(installable) {
		return filepath.Clean(installable), nil
	}
	if strings.ContainsAny(installable, " \t\r\n") {
		return "", fmt.Errorf("installable %q contains whitespace", installable)
	}
	cmd := exec.CommandContext(ctx, nixPath, "--extra-experimental-features", "nix-command flakes",
		"build", "--no-link", "--print-out-paths", "--", installable)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("nix build %s: %w", installable, err)
	}
	lines := strings.Fields(string(out))
	if len(lines) != 1 {
		return "", fmt.Errorf("nix build %s produced %d output paths; expected exactly one", installable, len(lines))
	}
	return lines[0], nil
}
