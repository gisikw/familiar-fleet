package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fakeRuntime(t *testing.T, name string, withPi bool) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if withPi {
		script := "#!/bin/sh\necho " + name + "\n"
		if err := os.WriteFile(filepath.Join(dir, "bin", "pi"), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func readLink(t *testing.T, path string) string {
	t.Helper()
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("readlink %s: %v", path, err)
	}
	return target
}

func TestActivateCreatesCurrentThenRotatesPrevious(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "runtime"))
	a := fakeRuntime(t, "a", true)
	b := fakeRuntime(t, "b", true)

	if _, err := s.Ready(); !errors.Is(err, ErrNoRuntime) {
		t.Fatalf("Ready before activation: %v", err)
	}
	changed, err := s.Activate(a)
	if err != nil || !changed {
		t.Fatalf("activate a: changed=%v err=%v", changed, err)
	}
	if got := readLink(t, s.Current()); got != a {
		t.Fatalf("current=%s want %s", got, a)
	}
	if _, err := os.Lstat(s.Previous()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("previous should not exist after first activation: %v", err)
	}

	changed, err = s.Activate(b)
	if err != nil || !changed {
		t.Fatalf("activate b: changed=%v err=%v", changed, err)
	}
	if got := readLink(t, s.Current()); got != b {
		t.Fatalf("current=%s want %s", got, b)
	}
	if got := readLink(t, s.Previous()); got != a {
		t.Fatalf("previous=%s want %s", got, a)
	}
	if got, err := s.Ready(); err != nil || got != b {
		t.Fatalf("Ready=%q err=%v", got, err)
	}
	if info, err := os.Stat(s.Dir); err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatalf("runtime dir must be owner-only: %v %v", info.Mode(), err)
	}
}

func TestActivateIsIdempotent(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "runtime"))
	a := fakeRuntime(t, "a", true)
	b := fakeRuntime(t, "b", true)
	for _, r := range []string{a, b} {
		if _, err := s.Activate(r); err != nil {
			t.Fatal(err)
		}
	}
	changed, err := s.Activate(b + string(filepath.Separator))
	if err != nil || changed {
		t.Fatalf("re-activating current: changed=%v err=%v", changed, err)
	}
	if got := readLink(t, s.Previous()); got != a {
		t.Fatalf("idempotent activation must not clobber previous: got %s", got)
	}
}

func TestActivateRejectsInvalidAndLeavesPointersUntouched(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "runtime"))
	good := fakeRuntime(t, "good", true)
	if _, err := s.Activate(good); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"missing pi":       fakeRuntime(t, "nopi", false),
		"relative":         "relative/path",
		"nonexistent":      filepath.Join(t.TempDir(), "nope"),
		"unsafe character": filepath.Join(t.TempDir(), "has'quote"),
	}
	for name, target := range cases {
		if _, err := s.Activate(target); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if got := readLink(t, s.Current()); got != good {
		t.Fatalf("current changed after failed activations: %s", got)
	}
	if _, err := os.Lstat(s.Previous()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("previous created by failed activation: %v", err)
	}
	if entries, _ := os.ReadDir(s.Dir); len(entries) != 1 {
		t.Fatalf("temporary artifacts left behind: %v", entries)
	}
}

func TestValidateRejectsNonExecutablePi(t *testing.T) {
	dir := fakeRuntime(t, "x", true)
	if err := os.Chmod(filepath.Join(dir, "bin", "pi"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := Validate(dir); err == nil || !strings.Contains(err.Error(), "not an executable") {
		t.Fatalf("expected executable error, got %v", err)
	}
}

func TestRollbackSwapsPointers(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "runtime"))
	a := fakeRuntime(t, "a", true)
	b := fakeRuntime(t, "b", true)
	if _, err := s.Rollback(); err == nil {
		t.Fatal("rollback with no pointers must fail")
	}
	for _, r := range []string{a, b} {
		if _, err := s.Activate(r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Rollback()
	if err != nil || got != a {
		t.Fatalf("rollback=%q err=%v", got, err)
	}
	if readLink(t, s.Current()) != a || readLink(t, s.Previous()) != b {
		t.Fatalf("pointers not swapped: current=%s previous=%s", readLink(t, s.Current()), readLink(t, s.Previous()))
	}
	// Rolling back again returns to b: the two pointers alternate.
	if got, err := s.Rollback(); err != nil || got != b {
		t.Fatalf("second rollback=%q err=%v", got, err)
	}
}

func TestRollbackRefusesBrokenPrevious(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "runtime"))
	a := fakeRuntime(t, "a", true)
	b := fakeRuntime(t, "b", true)
	for _, r := range []string{a, b} {
		if _, err := s.Activate(r); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate garbage collection of the previous runtime.
	if err := os.RemoveAll(a); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rollback(); err == nil {
		t.Fatal("rollback onto a removed runtime must fail")
	}
	if got := readLink(t, s.Current()); got != b {
		t.Fatalf("current changed by refused rollback: %s", got)
	}
	st := s.Status()
	if st.CurrentErr != nil || st.PreviousErr == nil {
		t.Fatalf("status: current=%v previous=%v", st.CurrentErr, st.PreviousErr)
	}
}

func TestReadyReportsBrokenCurrent(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "runtime"))
	a := fakeRuntime(t, "a", true)
	if _, err := s.Activate(a); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(a, "bin", "pi")); err != nil {
		t.Fatal(err)
	}
	_, err := s.Ready()
	if err == nil || !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("Ready with broken current: %v", err)
	}
}

func TestPointerRejectsNonSymlink(t *testing.T) {
	s := New(t.TempDir())
	if err := os.Mkdir(s.Current(), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pointer(CurrentName); err == nil || errors.Is(err, ErrNoRuntime) {
		t.Fatalf("directory named current must be rejected: %v", err)
	}
	if _, err := s.Activate(fakeRuntime(t, "a", true)); err == nil {
		t.Fatal("activate must not replace a real directory")
	}
}

func TestBuildPassesThroughAbsolutePathsAndParsesNixOutput(t *testing.T) {
	ctx := context.Background()
	if got, err := Build(ctx, "/nonexistent/nix", "/nix/store/abc-runtime/"); err != nil || got != "/nix/store/abc-runtime" {
		t.Fatalf("absolute passthrough: %q %v", got, err)
	}
	if _, err := Build(ctx, "/nonexistent/nix", "github:x/y#z w"); err == nil {
		t.Fatal("whitespace in installable must be rejected")
	}
	if runtime.GOOS == "windows" {
		t.Skip("fake nix script")
	}
	fake := filepath.Join(t.TempDir(), "nix")
	script := "#!/bin/sh\n[ \"$3\" = build ] || exit 9\nprintf '%s\\n' \"$@\" >\"$0.args\"\necho /nix/store/fake-familiar-worker-runtime\n"
	if err := os.WriteFile(fake, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	got, err := Build(ctx, fake, "github:gisikw/familiar/abc#familiar-worker-runtime")
	if err != nil || got != "/nix/store/fake-familiar-worker-runtime" {
		t.Fatalf("build=%q err=%v", got, err)
	}
	args, _ := os.ReadFile(fake + ".args")
	for _, want := range []string{"--no-link", "--print-out-paths", "github:gisikw/familiar/abc#familiar-worker-runtime"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("nix args missing %s: %s", want, args)
		}
	}
	multi := filepath.Join(t.TempDir(), "nix")
	if err := os.WriteFile(multi, []byte("#!/bin/sh\necho /nix/store/a\necho /nix/store/b\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(ctx, multi, "github:x/y#z"); err == nil {
		t.Fatal("multiple outputs must be rejected")
	}
}

func TestSmokeRunsRequiredTool(t *testing.T) {
	ok := fakeRuntime(t, "ok", true)
	if err := Smoke(context.Background(), ok); err != nil {
		t.Fatalf("smoke ok: %v", err)
	}
	bad := fakeRuntime(t, "bad", true)
	if err := os.WriteFile(filepath.Join(bad, "bin", "pi"), []byte("#!/bin/sh\necho broken >&2\nexit 3\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := Smoke(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("smoke bad: %v", err)
	}
}
