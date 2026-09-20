package client

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeDescriptorValidation(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	good := RuntimeDescriptor{Schema: 1, Installable: "github:gisikw/familiar/" + commit + "#familiar-worker-runtime"}
	if err := good.Validate(); err != nil {
		t.Fatalf("rejected a valid descriptor: %v", err)
	}

	bad := []struct {
		name       string
		descriptor RuntimeDescriptor
		want       string
	}{
		{"schema zero with installable", RuntimeDescriptor{Installable: good.Installable}, "unsupported runtime descriptor schema 0"},
		{"schema two", RuntimeDescriptor{Schema: 2, Installable: good.Installable}, "unsupported runtime descriptor schema 2"},
		{"missing installable", RuntimeDescriptor{Schema: 1}, "omits installable"},
		{"branch ref", RuntimeDescriptor{Schema: 1, Installable: "github:gisikw/familiar/main#familiar-worker-runtime"}, "40-hex-commit"},
		{"refs heads", RuntimeDescriptor{Schema: 1, Installable: "github:gisikw/familiar/refs/heads/main#familiar-worker-runtime"}, "40-hex-commit"},
		{"short commit", RuntimeDescriptor{Schema: 1, Installable: "github:gisikw/familiar/0123456#familiar-worker-runtime"}, "40-hex-commit"},
		{"uppercase commit", RuntimeDescriptor{Schema: 1, Installable: "github:gisikw/familiar/" + strings.ToUpper(commit) + "#familiar-worker-runtime"}, "40-hex-commit"},
		{"wrong owner", RuntimeDescriptor{Schema: 1, Installable: "github:attacker/familiar/" + commit + "#familiar-worker-runtime"}, "40-hex-commit"},
		{"wrong attribute", RuntimeDescriptor{Schema: 1, Installable: "github:gisikw/familiar/" + commit + "#evil"}, "40-hex-commit"},
		{"store path", RuntimeDescriptor{Schema: 1, Installable: "/nix/store/abc-familiar-worker-runtime"}, "40-hex-commit"},
		{"trailing junk", RuntimeDescriptor{Schema: 1, Installable: "github:gisikw/familiar/" + commit + "#familiar-worker-runtime extra"}, "40-hex-commit"},
	}
	for _, tc := range bad {
		err := tc.descriptor.Validate()
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.want)
		}
	}

	if err := (RuntimeDescriptor{}).Validate(); !errors.Is(err, ErrNoRuntimeDescriptor) {
		t.Fatalf("absent descriptor should be distinguishable, got %v", err)
	}
}

func TestEnrollmentRequiresRuntimeDescriptor(t *testing.T) {
	e := validEnrollment()
	e.Runtime = RuntimeDescriptor{}
	if err := ValidateEnrollment(e); !errors.Is(err, ErrNoRuntimeDescriptor) {
		t.Fatalf("enrollment without a descriptor: %v", err)
	}
	e.Runtime = RuntimeDescriptor{Schema: 1, Installable: "github:gisikw/familiar/main#familiar-worker-runtime"}
	if err := ValidateEnrollment(e); err == nil || !strings.Contains(err.Error(), "runtime descriptor") {
		t.Fatalf("enrollment with a branch installable: %v", err)
	}
}

// Pre-descriptor state must fail with one concise migration message naming the
// file to remove and the command to re-run, not an opaque validation error.
func TestPreDescriptorStateGivesMigrationMessage(t *testing.T) {
	dir := t.TempDir()
	p, err := StatePaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	legacy := `{
  "version": 1,
  "endpoint": "https://fleet.example.com",
  "ssh_user": "alice",
  "enrollment": {
    "node_id": "node-1", "host": "mac-1", "port": 22001,
    "tunnel_host": "rendezvous.example.com", "tunnel_ssh_port": 2222,
    "tunnel_user": "tunnel", "remote_session": "familiar-fleet",
    "controller_public_key": ` + quoteJSON(testKey('c')) + `,
    "tunnel_host_key": ` + quoteJSON(testKey('h')) + `
  }
}`
	if err := os.WriteFile(p.State, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadState(p.State)
	if err == nil {
		t.Fatal("accepted pre-descriptor state")
	}
	for _, want := range []string{p.State, "familiar-fleet connect https://fleet.example.com"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("migration message %q missing %q", err, want)
		}
	}
}

func quoteJSON(s string) string { return `"` + s + `"` }

// fakeRuntime creates a directory that passes Validate and Smoke.
func fakeRuntime(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "pi"), []byte("#!/bin/sh\necho pi 1.0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// applierForTest wires an applier whose nix/nix-store are scripts, so the
// expensive path can be observed without Nix.
func applierForTest(t *testing.T, paths Paths, target string) (RuntimeApplier, *int) {
	t.Helper()
	builds := 0
	scripts := t.TempDir()
	nix := filepath.Join(scripts, "nix")
	// A real build materializes the output; this stands in for that, so a
	// damaged target is repaired by building rather than by the test.
	script := "#!/bin/sh\nmkdir -p " + target + "/bin\n" +
		"printf '#!/bin/sh\\necho pi 1.0\\n' > " + target + "/bin/pi\n" +
		"chmod 755 " + target + "/bin/pi\n" +
		"printf '%s\\n' " + target + "\n"
	if err := os.WriteFile(nix, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	nixStore := filepath.Join(scripts, "nix-store")
	// --add-root <root> --indirect --realise <target>
	if err := os.WriteFile(nixStore, []byte("#!/bin/sh\nln -sfn \"$4\" \"$2\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	a := RuntimeApplier{
		Paths: paths, Nix: "nix", NixStore: "nix-store",
		Lookup: func(name string) (string, error) {
			if name == "nix" {
				builds++
				return nix, nil
			}
			return nixStore, nil
		},
	}
	return a, &builds
}

func TestApplyIsIdempotentAndSkipsProvenWork(t *testing.T) {
	state := t.TempDir()
	paths, err := StatePaths(state)
	if err != nil {
		t.Fatal(err)
	}
	target := fakeRuntime(t, filepath.Join(state, "rt-a"))
	applier, builds := applierForTest(t, paths, target)

	first, err := applier.Apply(context.Background(), testInstallable)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed || first.Skipped || first.Target != target {
		t.Fatalf("first apply = %#v", first)
	}
	if *builds != 1 {
		t.Fatalf("first apply resolved nix %d times", *builds)
	}

	// A matching active target must be cheap: no build, no smoke, no activate.
	second, err := applier.Apply(context.Background(), testInstallable)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Skipped || second.Changed || second.Target != target {
		t.Fatalf("second apply = %#v", second)
	}
	if *builds != 1 {
		t.Fatalf("second apply rebuilt: nix resolved %d times", *builds)
	}
}

func TestTrueUpReconcilesManualOverride(t *testing.T) {
	state := t.TempDir()
	paths, err := StatePaths(state)
	if err != nil {
		t.Fatal(err)
	}
	desired := fakeRuntime(t, filepath.Join(state, "rt-desired"))
	override := fakeRuntime(t, filepath.Join(state, "rt-override"))

	applier, _ := applierForTest(t, paths, desired)
	descriptor := RuntimeDescriptor{Schema: 1, Installable: testInstallable}
	if _, err := applier.TrueUp(context.Background(), descriptor); err != nil {
		t.Fatal(err)
	}

	// An operator applies something else by hand through the admin seam.
	manual, _ := applierForTest(t, paths, override)
	if _, err := manual.Apply(context.Background(), override); err != nil {
		t.Fatal(err)
	}
	if got := readLink(t, filepath.Join(paths.RuntimeDir, "current")); got != override {
		t.Fatalf("manual override did not take effect: %s", got)
	}

	// The next normal start must reconcile back to the enrolled authority.
	result, err := applier.TrueUp(context.Background(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if result.Skipped {
		t.Fatal("true-up skipped work after a manual override")
	}
	if got := readLink(t, filepath.Join(paths.RuntimeDir, "current")); got != desired {
		t.Fatalf("runtime/current = %s, want the enrolled %s", got, desired)
	}
}

func TestTrueUpRejectsUnpinnedDescriptor(t *testing.T) {
	paths, err := StatePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	applier := RuntimeApplier{Paths: paths, Nix: "nix", NixStore: "nix-store",
		Lookup: func(string) (string, error) { return "", errors.New("nix must not be needed") }}
	_, err = applier.TrueUp(context.Background(), RuntimeDescriptor{Schema: 1, Installable: "github:gisikw/familiar/main#familiar-worker-runtime"})
	if err == nil || !strings.Contains(err.Error(), "40-hex-commit") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// A recorded runtime whose target has gone away must not be trusted: the
// skip is only valid while the realized output is still usable.
func TestTrueUpRebuildsWhenRecordedTargetIsGone(t *testing.T) {
	state := t.TempDir()
	paths, err := StatePaths(state)
	if err != nil {
		t.Fatal(err)
	}
	target := fakeRuntime(t, filepath.Join(state, "rt-a"))
	applier, builds := applierForTest(t, paths, target)
	if _, err := applier.Apply(context.Background(), testInstallable); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(target, "bin")); err != nil {
		t.Fatal(err)
	}
	result, err := applier.Apply(context.Background(), testInstallable)
	if err != nil {
		t.Fatal(err)
	}
	if result.Skipped {
		t.Fatal("skipped despite an unusable recorded runtime")
	}
	if *builds != 2 {
		t.Fatalf("nix resolved %d times, want a rebuild", *builds)
	}
}

func readLink(t *testing.T, path string) string {
	t.Helper()
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	return target
}
