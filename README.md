# familiar-fleet

`familiar-fleet` connects a macOS or Linux machine to Familiar through Herdr. It
runs an unprivileged callback `sshd` on IPv4 loopback and maintains an outbound,
host-key-pinned reverse SSH tunnel. It does **not** enable Remote Login, listen on
port 22, require root for desktop use, or send a private key over the network.

The repository pins Herdr **v0.9.1**. The root launcher runs the Nix package:

```sh
./familiar-fleet version
```

## Quickstart

Prerequisites are Nix with flakes enabled and OpenSSH (`ssh`, `sshd`, and
`ssh-keygen`; macOS includes these).

First, connect in a browser:

```sh
./familiar-fleet connect https://familiar.example.com
```

The client validates and canonicalizes the URL, discovers RFC 9728 protected
resource metadata and the advertised OIDC issuer, then uses authorization code +
PKCE as the registered public client `familiar-desktop`. It binds the registered
`http://127.0.0.1:17421/callback` listener before opening the system browser. After
sign-in, the browser asks for a machine name (default: the short hostname, such as
`macbook`) and submits enrollment. OAuth state and nonce are checked, the callback
and form have a five-minute timeout, and enrollment is one-shot.

No token is accepted on a command line. Access, refresh, authorization-code, PKCE,
and nonce values are never persisted. The access token is held in memory only long
enough for one `POST <familiar-url>/fleet`; a refresh token in the response is
ignored.

After connecting, run the interactive experience:

```sh
./familiar-fleet
```

The callback `sshd` and reconnecting tunnel run in the background while the visible
Herdr TUI attaches to the named `familiar-fleet` session with the terminal's
stdin/stdout/stderr. Tunnel, `sshd`, and Herdr server operational output goes to the
state-owned log rather than corrupting the TUI. Intentionally leaving the TUI stops
this invocation's tunnel and `sshd`, asks Herdr to stop the named server, and cleans
up remaining children. Herdr's server is launched as a session leader, preserving
the detached-compatible semantics used by saved machines.

Running without completed state prints a short instruction to run `connect`.
Options, including `--state-dir`, must precede the command.

## Authentication and enrollment details

The deployment must provide:

- `<familiar-url>/.well-known/oauth-protected-resource`, with an
  `authorization_servers` issuer;
- `<issuer>/.well-known/openid-configuration`, with authorization and token
  endpoints; and
- the registered public client `familiar-desktop`, authorization-code + S256 PKCE,
  and callback `http://127.0.0.1:17421/callback`.

The browser is opened with `open` on macOS or `xdg-open` on Linux. If that program
is unavailable, the error includes the authorization URL to open manually. Remote
URLs and advertised OAuth endpoints must use HTTPS; HTTP is accepted only for
loopback tests. Redirects are not followed by the production HTTP client.

For API-contract debugging only, the old explicit enrollment seam remains:

```sh
install -m 600 /dev/null ./token
printf '%s\n' "$TOKEN" > ./token
./familiar-fleet --endpoint https://familiar.example.com \
  --token-file ./token --name macbook enroll
rm ./token
```

This is secondary compatibility behavior, not an authentication fallback from
`connect`. `FAMILIAR_FLEET_TOKEN` is also retained for managed test fixtures, but
must not be placed in a service definition.

### Rendezvous host key

A safe first SSH connection requires trusted rendezvous host-key material. The
preferred enrollment response includes the backward-compatible field:

```json
{"tunnel_host_key":"ssh-ed25519 AAAA…"}
```

If it is omitted, pass an independently verified key with
`--rendezvous-host-key 'ssh-ed25519 AAAA…'` (or
`FAMILIAR_FLEET_RENDEZVOUS_HOST_KEY`) during `connect`. If both are present they
must match. The client never falls back to `StrictHostKeyChecking=no`. Rotation
currently requires coordinated re-enrollment or a deliberately separate state
directory.

## Commands and lifecycle

```text
familiar-fleet                         tunnel + visible Herdr TUI
familiar-fleet connect <familiar-url>  browser setup and enrollment
familiar-fleet herdr                   managed/headless Herdr server only
familiar-fleet tunnel                  callback sshd + reconnecting tunnel only
familiar-fleet version                 print version
```

`herdr` inherits the complete environment and `PATH` of the service that starts it,
so terminals spawned by Herdr see that same environment. `tunnel` writes operational
output to the state log and does not launch a TUI or Herdr server. These commands
are intended to be separate managed services. TERM/INT causes bounded shutdown;
child process groups receive TERM and then KILL if necessary. An unexpected local
`sshd` or Herdr server exit fails the owning command so a service manager can
restart it. Tunnel failures reconnect with bounded, jittered backoff.

`run` remains a cheap alias for the default interactive mode. `enroll` is the debug
token seam described above.

## State, configuration, and logs

State location precedence is deterministic and no alternate writable locations are
searched or merged:

1. explicit `--state-dir`;
2. for root (UID 0), `/var/lib/familiar-fleet`;
3. for normal users, `$XDG_STATE_HOME/familiar-fleet` when set;
4. otherwise `~/.local/state/familiar-fleet`.

The directory is mode `0700`. Important files are:

- `state.json`: endpoint, local username, and server-issued enrollment only;
- `familiar-fleet.log`: tunnel, callback `sshd`, and supervisor logs;
- `tunnel_ed25519`: dedicated outbound tunnel identity;
- `sshd_host_ed25519`: local callback server host identity;
- generated `authorized_keys`, `known_hosts`, `sshd_config`, and wrappers.

Private material, state, generated authorization files, and logs are owner-only.
State writes are atomic; existing unsafe permissions are rejected. OAuth tokens are
not part of state or logs.

The local server binds `127.0.0.1` on an OS-selected high port. It disables
passwords, keyboard-interactive auth, PAM, forwarding, agent/X11 forwarding,
tunnels, user rc files, and PTYs, and permits only the enrolled local user. A
state-owned bridge puts the exact Herdr executable discovered at startup first on
`PATH`. The reverse tunnel uses a dedicated identity, no user/system SSH config,
strict pinned host checking, keepalives, and:

```text
-R 127.0.0.1:<allocated-port>:127.0.0.1:<local-sshd-port>
```

## Managed services

Connect interactively once before installing services. Always choose an explicit,
durable state path in service definitions. Run the split `herdr` and `tunnel`
components rather than the interactive command.

### Linux systemd user services

Install the flake package—not the repository's `./familiar-fleet` launcher by
itself, because that launcher deliberately resolves the flake beside it:

```sh
nix profile install .#
install -Dm644 packaging/familiar-fleet.service \
  "$HOME/.config/systemd/user/familiar-fleet-tunnel.service"
install -Dm644 packaging/familiar-fleet-herdr.service \
  "$HOME/.config/systemd/user/familiar-fleet-herdr.service"
systemctl --user daemon-reload
systemctl --user enable --now familiar-fleet-herdr.service familiar-fleet-tunnel.service
```

The examples use `%h/.nix-profile/bin/familiar-fleet` and
`%h/.local/state/familiar-fleet` explicitly. Adjust absolute OpenSSH paths if the
user manager cannot find the host tools. For a system/root service, use
`--state-dir /var/lib/familiar-fleet` and ensure the enrollment belongs to the
service's local user.

### macOS launchd

First run `nix profile install .#`. Copy both plist examples to
`~/Library/LaunchAgents`, replace `REPLACE_ME`, and bootstrap both labels. They use
`/Users/REPLACE_ME/.nix-profile/bin/familiar-fleet` and an explicit
`/Users/REPLACE_ME/.local/state/familiar-fleet`. The tunnel plist supplies absolute
paths for macOS's host OpenSSH tools; Herdr remains the version pinned inside the
installed flake package. Remote Login may remain disabled.

## Threat model and limitations

The client keeps private keys local, avoids a LAN-facing listener, separates tunnel
and host identities, pins the rendezvous, validates server fields, and passes
untrusted values as `exec` argument arrays. The controller key can invoke Herdr as
the enrolled local user; the rendezvous must independently enforce a tunnel-only
account and allocated listen port. This POC does not defend against a compromised
local account, controller, Herdr binary, HTTPS identity boundary, or rendezvous. It
does not implement remote unenrollment, daemon installation, or automatic host-key
rotation.

## Development

```sh
nix develop

gofmt -w .
go test -race ./...
go vet ./...
nix flake check
nix build
git diff --check
```

Tests cover metadata and issuer discovery, authorize parameters and PKCE, callback
state and token nonce, browser form enrollment, token non-persistence, command
modes, state selection, strict state permissions, generated SSH configuration,
process arguments, loopback `sshd`, and tunnel reconnect behavior. No live identity
provider or privileged port is required.
