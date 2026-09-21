package client

import (
	"fmt"
	"os"
	"strings"
)

// Tiamat environment variables read by the Pi extension inside panes. The
// client only resolves and validates them; it never reads token contents and
// never fetches, mints, or persists a credential.
const (
	// TiamatURLEnv overrides the node-local Tiamat URL file.
	TiamatURLEnv = "FAMILIAR_TIAMAT_URL"
	// TiamatTokenFileEnv points at the file holding the Tiamat token. When it
	// is unset, the preflight uses <state>/secrets/tiamat.token.
	TiamatTokenFileEnv = "FAMILIAR_TIAMAT_TOKEN_FILE"
)

// TiamatPreflight is the resolved, validated Tiamat configuration. The token
// value is deliberately never loaded.
type TiamatPreflight struct {
	// URL is the required, non-empty router URL resolved from the environment
	// override or the node-local URL file.
	URL string
	// TokenFile is the path exported to the Herdr server and its panes.
	TokenFile string
	// TokenFileExplicit reports whether the operator set the path themselves
	// rather than accepting <state>/secrets/tiamat.token.
	TokenFileExplicit bool
}

// ResolveTiamat validates the Tiamat inputs the Familiar-owned Herdr server and
// every pane it spawns must inherit. It fails closed: a Herdr agent started
// without a usable router URL and token file would otherwise surface much later
// as an opaque in-pane failure. FAMILIAR_TIAMAT_URL overrides the node-local
// <state>/secrets/tiamat.url file, which keeps deployment configuration out of a
// user's general shell environment.
//
// The token file is required to exist as a readable, regular, non-empty file,
// but its contents are never inspected. Some routers behind the firewall do not
// enforce authentication, so a placeholder token is a legitimate deployment.
// The file is never created automatically: provisioning a secret is the
// operator's job, and writing one here would turn a read-only public runtime
// into a credential-writing one.
func ResolveTiamat(paths Paths, env map[string]string) (TiamatPreflight, error) {
	var result TiamatPreflight

	result.URL = strings.TrimSpace(env[TiamatURLEnv])
	if result.URL == "" {
		var err error
		result.URL, err = readTiamatURLFile(paths.TiamatURLFile)
		if err != nil {
			return result, fmt.Errorf("Tiamat router URL is required before the Herdr server starts: %w; write it to %s or set %s as an override", err, paths.TiamatURLFile, TiamatURLEnv)
		}
	}

	if explicit := strings.TrimSpace(env[TiamatTokenFileEnv]); explicit != "" {
		result.TokenFile, result.TokenFileExplicit = explicit, true
	} else {
		result.TokenFile = paths.TiamatTokenFile
	}

	if err := checkTokenFile(result.TokenFile, result.TokenFileExplicit, paths.SecretsDir); err != nil {
		return TiamatPreflight{}, err
	}
	return result, nil
}

func readTiamatURLFile(path string) (string, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return "", fmt.Errorf("URL file does not exist")
	}
	if err != nil {
		return "", fmt.Errorf("URL file is unusable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("URL file must be a regular file, not %s", info.Mode().Type())
	}
	if info.Size() > 4096 {
		return "", fmt.Errorf("URL file exceeds 4096 bytes")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("URL file is not readable by this user: %w", err)
	}
	value := strings.TrimSpace(string(contents))
	if value == "" {
		return "", fmt.Errorf("URL file is empty")
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("URL file must contain exactly one URL")
	}
	return value, nil
}

// checkTokenFile enforces readability without ever reading the token: it opens
// the file and reads nothing, so failures never risk the value reaching an
// error string or log line.
func checkTokenFile(path string, explicit bool, secretsDir string) error {
	hint := fmt.Sprintf("create it with `install -m 600 /dev/null %s` and write the token into it (a placeholder is fine for routers that do not require auth)", path)
	if !explicit {
		hint = fmt.Sprintf("create %s with mode 0700, then `install -m 600 /dev/null %s` and write the token into it (a placeholder is fine for routers that do not require auth), or set %s to an existing file", secretsDir, path, TiamatTokenFileEnv)
	}

	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return fmt.Errorf("Tiamat token file %s does not exist; %s", path, hint)
	}
	if err != nil {
		return fmt.Errorf("Tiamat token file %s is unusable: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("Tiamat token file %s must be a regular file, not %s; %s", path, info.Mode().Type(), hint)
	}
	if info.Size() == 0 {
		return fmt.Errorf("Tiamat token file %s is empty; write the token into it (a placeholder is fine for routers that do not require auth)", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("Tiamat token file %s is not readable by this user: %w", path, err)
	}
	return f.Close()
}
