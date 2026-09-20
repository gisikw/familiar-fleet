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
	// TiamatURLEnv is the Tiamat router base URL. It has no deterministic
	// default, so the preflight requires it explicitly.
	TiamatURLEnv = "FAMILIAR_TIAMAT_URL"
	// TiamatTokenFileEnv points at the file holding the Tiamat token. When it
	// is unset, the preflight uses <state>/secrets/tiamat.token.
	TiamatTokenFileEnv = "FAMILIAR_TIAMAT_TOKEN_FILE"
)

// TiamatPreflight is the resolved, validated Tiamat configuration. It holds
// locations only: the token value is deliberately never loaded.
type TiamatPreflight struct {
	// URL is the required, non-empty FAMILIAR_TIAMAT_URL.
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
// as an opaque in-pane failure.
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
		return result, fmt.Errorf("%s is required before the Herdr server starts; set it to the Tiamat router URL (for example %s=https://tiamat.internal) in the service environment", TiamatURLEnv, TiamatURLEnv)
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
