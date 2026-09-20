# familiar-fleet

`familiar-fleet` is a small foreground supervisor for a machine controlled through
[Familiar](https://github.com/gisikw/familiar-fleet) and Herdr. One command:

1. creates two independent Ed25519 key pairs and enrolls the machine when needed;
2. runs `herdr --session familiar-fleet server`;
3. runs an unprivileged OpenSSH server bound only to a high-numbered loopback port; and
4. continually maintains an outbound, host-key-pinned reverse SSH forwarding to the
   Familiar rendezvous.

It does **not** enable macOS Remote Login, listen on port 22, require root, or put a
private key on the wire. Herdr is the lifetime anchor: if Herdr exits, the utility
exits and cleans up all children. A failed tunnel reconnects with bounded backoff
without stopping Herdr. An unexpected local `sshd` exit fails the utility explicitly
so launchd/systemd can restart the complete, known-good process tree.

## Prerequisites

- macOS or Linux;
- Herdr **v0.9.1** (`herdr`), with the saved `--machine` profile reconciled by the
  Familiar server;
- OpenSSH `ssh`, `sshd`, and `ssh-keygen`; and
- Go 1.22+ to build from source.

```sh
go build -trimpath -o familiar-fleet ./cmd/familiar-fleet
# or enter a development environment (Herdr is installed separately)
nix develop
```

## First run and enrollment

Obtain a short-lived bearer token from the Familiar deployment's identity boundary,
put it in an owner-only file, and run:

```sh
install -m 600 /dev/null "$HOME/.familiar-enrollment-token"
printf '%s\n' "$TOKEN" >"$HOME/.familiar-enrollment-token"

familiar-fleet \
  --endpoint https://familiar.example.com \
  --token-file "$HOME/.familiar-enrollment-token" \
  --name asgmacbook \
  run
rm "$HOME/.familiar-enrollment-token"
```

Flags must precede the command. `run` is the default, so it may be omitted. Use
`enroll` instead to enroll and print non-secret status without starting children.
On later runs the saved enrollment is used and no token is needed.

`--token-file` is preferred because it avoids process arguments and shell history.
The file must be regular and have no group/other permissions. For managed secret
injection, `FAMILIAR_FLEET_TOKEN` is also accepted. The client sends it only as
`Authorization: Bearer …` to `POST <endpoint>/fleet`; non-loopback endpoints must
use HTTPS. The current CLI does not launch a browser or implement an OAuth callback.
A deployment can add that flow in front of this explicit, short-lived token handoff
without changing enrollment or storing a refresh token.

The requested name defaults to the machine's short hostname. Endpoint and name are
first-enrollment settings, not mutable tunnel settings. The server owns the canonical
name and allocated reverse port. To enroll the same machine against a different
deployment, intentionally choose a separate `--state-dir`.

### Rendezvous host-key contract seam

The provisional API contract in the project brief has no rendezvous SSH host key,
although safe first connection requires one. This client accepts the following
backward-compatible response extension:

```json
{"tunnel_host_key":"ssh-ed25519 AAAA…"}
```

If the server omits it, pass independently verified public material with
`--rendezvous-host-key 'ssh-ed25519 AAAA…'` or
`FAMILIAR_FLEET_RENDEZVOUS_HOST_KEY`. If both are present they must match. The key
is pinned in the generated `known_hosts`; the client never falls back to
`StrictHostKeyChecking=no`. Key rotation currently requires an explicit re-enrollment
workflow (or a fresh state directory) and should be coordinated server-side.

## State and generated SSH configuration

By default state is under:

- `$XDG_STATE_HOME/familiar-fleet`, when `XDG_STATE_HOME` is set; otherwise
- `~/.local/state/familiar-fleet`.

The directory is mode `0700`; private keys, enrollment state, `authorized_keys`, and
`known_hosts` are mode `0600`. Writes are atomic. Existing state or private keys with
unsafe permissions are rejected. The two generated private keys have distinct roles:

- `tunnel_ed25519`: authenticates the outbound restricted tunnel account;
- `sshd_host_ed25519`: identifies the ephemeral local callback SSH server.

Only their public halves are enrolled. The controller public key returned by the API
is the sole key in the callback server's generated `authorized_keys`.

The local server binds IPv4 `127.0.0.1` only on an OS-selected high port (selection is
retried to handle the close/bind race). It disables passwords, keyboard-interactive,
PAM, forwarding, agent/X11 forwarding, tunnels, user rc files, and PTYs; permits only
the current local user; and has no root path. A state-owned wrapper and an explicit
noninteractive `PATH` resolve `herdr` to the exact executable discovered at startup.
The controller remains trusted to select Herdr command arguments; network exposure
and credential scope, rather than a command allow-list, are the security boundary.

The tunnel invokes `ssh` with no user or system SSH config (`-F /dev/null`), only the
dedicated identity, `BatchMode`, `IdentitiesOnly`, strict pinned host checking,
`ExitOnForwardFailure`, keepalives, and:

```
-R 127.0.0.1:<server-allocated-port>:127.0.0.1:<ephemeral-local-sshd-port>
```

## Supervision

Enroll interactively once before installing a service. Do not put an enrollment token
in a service file.

### macOS launchd

Copy `packaging/com.familiar.fleet.plist.example` to
`~/Library/LaunchAgents/com.familiar.fleet.plist`, replace all placeholders with
absolute paths, then:

```sh
launchctl bootstrap "gui/$(id -u)" ~/Library/LaunchAgents/com.familiar.fleet.plist
launchctl kickstart -k "gui/$(id -u)/com.familiar.fleet"
```

launchd has a sparse `PATH`, so absolute executable paths are recommended. If the
OpenSSH tools are not discoverable, add `--ssh /usr/bin/ssh`,
`--sshd /usr/sbin/sshd`, and `--ssh-keygen /usr/bin/ssh-keygen` to the plist. Remote
Login may remain disabled.

### Linux systemd user service

Install the binary and the example unit, after enrollment:

```sh
install -Dm755 familiar-fleet "$HOME/.local/bin/familiar-fleet"
install -Dm644 packaging/familiar-fleet.service \
  "$HOME/.config/systemd/user/familiar-fleet.service"
systemctl --user daemon-reload
systemctl --user enable --now familiar-fleet.service
```

Adjust `ExecStart` with absolute tool paths when the user manager's `PATH` does not
contain Herdr/OpenSSH. The hardening assumes the default state location already
exists; customize `ReadWritePaths` if using `XDG_STATE_HOME` or `--state-dir`.

## Threat model and limitations

The design protects private keys from the enrollment API, avoids a LAN-facing SSH
listener, isolates tunnel and host identities, pins the rendezvous, and treats API
fields as untrusted input. Values are validated and passed as `exec` argument arrays,
not interpolated into a shell. The only generated shell file quotes the already
resolved Herdr path and forwards arguments unchanged. Child process groups receive
TERM and then KILL on bounded shutdown.

It does not defend against a compromised local account, compromised controller key,
malicious Herdr binary, compromised HTTPS identity boundary, or compromised
rendezvous. The controller key can execute Herdr commands as the enrolled local user.
The rendezvous account must independently enforce a tunnel-only authorized-key policy
and restrict the allocated listen port; client flags cannot replace server policy.
The callback currently uses IPv4 loopback. There is no automated host-key rotation,
browser OAuth, remote unenrollment, or daemon installation command. Logs contain
node labels, ports, and process errors, but intentionally never tokens or private key
contents.

## Development

```sh
gofmt -w .
go vet ./...
go test -race ./...
go build ./cmd/familiar-fleet
```

Tests cover strict state permissions and validation, enrollment HTTP behavior,
generated SSH configuration (including `sshd -t` and an unprivileged loopback start
when OpenSSH is available), tunnel arguments, and bounded reconnect backoff. They do
not require privileged ports or live remote SSH infrastructure. CI runs the suite on
Linux and macOS.
