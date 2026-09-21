package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// secretValue is a token that must never appear in any error, note, or
// environment entry produced by the preflight.
const secretValue = "super-secret-tiamat-token-value"

func writeToken(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func writeTiamatURL(t *testing.T, p Paths, contents string) {
	t.Helper()
	writeToken(t, p.TiamatURLFile, contents)
}

func TestResolveTiamatRequiresURL(t *testing.T) {
	p := testPaths(t)
	writeToken(t, p.TiamatTokenFile, secretValue)

	for _, env := range []map[string]string{
		{},
		{TiamatURLEnv: ""},
		{TiamatURLEnv: "   "},
	} {
		_, err := ResolveTiamat(p, env)
		if err == nil {
			t.Fatalf("expected missing-URL error for %v", env)
		}
		// The message must name both supported configuration seams.
		if !strings.Contains(err.Error(), TiamatURLEnv) || !strings.Contains(err.Error(), p.TiamatURLFile) {
			t.Errorf("error is not actionable: %v", err)
		}
	}
}

func TestResolveTiamatDefaultsURLToFileAndAllowsEnvOverride(t *testing.T) {
	p := testPaths(t)
	writeToken(t, p.TiamatTokenFile, secretValue)
	writeTiamatURL(t, p, "  https://from-file.internal\n")

	got, err := ResolveTiamat(p, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://from-file.internal" {
		t.Fatalf("URL = %q", got.URL)
	}

	got, err = ResolveTiamat(p, map[string]string{TiamatURLEnv: " https://override.internal "})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "https://override.internal" {
		t.Fatalf("override URL = %q", got.URL)
	}
}

func TestResolveTiamatRejectsInvalidURLFiles(t *testing.T) {
	for name, contents := range map[string]string{
		"empty":     " \n",
		"multiline": "https://one.internal\nhttps://two.internal",
	} {
		t.Run(name, func(t *testing.T) {
			p := testPaths(t)
			writeToken(t, p.TiamatTokenFile, secretValue)
			writeTiamatURL(t, p, contents)
			if _, err := ResolveTiamat(p, map[string]string{}); err == nil {
				t.Fatal("expected invalid URL file error")
			}
		})
	}
}

func TestResolveTiamatDefaultsTokenPathUnderState(t *testing.T) {
	p := testPaths(t)
	writeToken(t, p.TiamatTokenFile, secretValue)

	got, err := ResolveTiamat(p, map[string]string{TiamatURLEnv: "https://tiamat.internal"})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(p.Dir, "secrets", "tiamat.token")
	if got.TokenFile != want {
		t.Errorf("token file = %q, want deterministic %q", got.TokenFile, want)
	}
	if got.TokenFileExplicit {
		t.Error("default path must not be reported as explicit")
	}
	if got.URL != "https://tiamat.internal" {
		t.Errorf("URL = %q", got.URL)
	}
}

func TestResolveTiamatPreservesExplicitTokenPath(t *testing.T) {
	p := testPaths(t)
	explicit := filepath.Join(t.TempDir(), "elsewhere.token")
	writeToken(t, explicit, secretValue)
	// The default location deliberately does not exist: an explicit path wins.

	got, err := ResolveTiamat(p, map[string]string{
		TiamatURLEnv:       "https://tiamat.internal",
		TiamatTokenFileEnv: explicit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.TokenFile != explicit || !got.TokenFileExplicit {
		t.Fatalf("%+v", got)
	}
}

// A dummy token is legitimate: some routers behind the firewall do not
// enforce auth. Only absence, emptiness, or unreadability is fatal.
func TestResolveTiamatAcceptsDummyToken(t *testing.T) {
	p := testPaths(t)
	writeToken(t, p.TiamatTokenFile, "dummy\n")

	if _, err := ResolveTiamat(p, map[string]string{TiamatURLEnv: "https://tiamat.internal"}); err != nil {
		t.Fatalf("dummy token must be accepted: %v", err)
	}
}

func TestResolveTiamatNeverCreatesTheTokenFile(t *testing.T) {
	p := testPaths(t)

	_, err := ResolveTiamat(p, map[string]string{TiamatURLEnv: "https://tiamat.internal"})
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected missing-token error, got %v", err)
	}
	if !strings.Contains(err.Error(), p.SecretsDir) || !strings.Contains(err.Error(), "0700") {
		t.Errorf("error should document the secrets directory and mode: %v", err)
	}
	if _, statErr := os.Stat(p.TiamatTokenFile); !os.IsNotExist(statErr) {
		t.Fatal("preflight must never create the token file itself")
	}
	if _, statErr := os.Stat(p.SecretsDir); !os.IsNotExist(statErr) {
		t.Fatal("preflight must never create the secrets directory itself")
	}
}

func TestResolveTiamatRejectsEmptyToken(t *testing.T) {
	p := testPaths(t)
	writeToken(t, p.TiamatTokenFile, "")

	_, err := ResolveTiamat(p, map[string]string{TiamatURLEnv: "https://tiamat.internal"})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-token error, got %v", err)
	}
}

func TestResolveTiamatRejectsNonRegularToken(t *testing.T) {
	p := testPaths(t)
	// A directory at the token path is the common misprovisioning mistake.
	if err := os.MkdirAll(p.TiamatTokenFile, 0700); err != nil {
		t.Fatal(err)
	}

	_, err := ResolveTiamat(p, map[string]string{TiamatURLEnv: "https://tiamat.internal"})
	if err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("expected non-regular-file error, got %v", err)
	}
}

func TestResolveTiamatRejectsUnreadableToken(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permission checks")
	}
	p := testPaths(t)
	writeToken(t, p.TiamatTokenFile, secretValue)
	if err := os.Chmod(p.TiamatTokenFile, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p.TiamatTokenFile, 0600) })

	_, err := ResolveTiamat(p, map[string]string{TiamatURLEnv: "https://tiamat.internal"})
	if err == nil || !strings.Contains(err.Error(), "not readable") {
		t.Fatalf("expected unreadable-token error, got %v", err)
	}
}

// The token value must never reach an error string: errors are surfaced to
// users and written to the operational log.
func TestResolveTiamatErrorsNeverLeakTokenContents(t *testing.T) {
	p := testPaths(t)
	writeToken(t, p.TiamatTokenFile, secretValue)

	// A readable token with a missing URL still must not echo the secret.
	_, err := ResolveTiamat(p, map[string]string{})
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Fatalf("error leaked the token: %v", err)
	}
}
