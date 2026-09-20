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

After connecting, `familiar-fleet` activates the Familiar runtime your deployment
enrolled this node to. That happens automatically, so there is no second setup
step:

```sh
./familiar-fleet
```

The callback `sshd` and reconnecting tunnel run in the background while the visible
Herdr TUI attaches to the named `familiar-fleet` session with the terminal's
stdin/stdout/stderr. Tunnel, `sshd`, and Herdr server operational output goes to the
state-owned log rather than corrupting the TUI.

**Leaving the TUI does not end your work.** It stops this invocation's tunnel and
callback `sshd`, but the named Herdr server, its session, and any running agents
stay alive. Run `familiar-fleet` again to bring the tunnel back and reattach to the
same session; end it deliberately with:

```sh
./familiar-fleet stop
```

Running without completed state prints a short instruction to run `connect`.
Options, including `--state-dir`, must precede the command.

## The enrolled runtime

Familiar decides which runtime a node runs. Enrollment therefore carries a runtime
descriptor, and `familiar-fleet` trues the node up to it automatically:

```json
{"runtime": {"schema": 1, "installable": "github:gisikw/familiar/<40-hex-commit>#familiar-worker-runtime"}}
```

Validation is strict in both the enrollment response and saved state. The schema
must be exactly `1`, and the installable must name **one exact, immutable Familiar
commit**. Branch and tag references such as `main` or `refs/heads/...` are rejected:
a moving reference would let two identical true-ups silently produce different
runtimes, which is precisely the property this descriptor exists to prevent.

True-up runs:

- after a successful `connect`, so a freshly connected node is immediately ready;
- on every normal interactive start; and
- on every `familiar-fleet herdr` start,

always *before* the Herdr environment is prepared. It builds, validates,
smoke-tests, GC-roots, and activates the enrolled runtime.

True-up is cheap when it is already satisfied. Each activation records the
installable and the store path it realized, so a descriptor that matches a recorded
target which is still a valid runtime skips the build and smoke test entirely.
Correctness still wins: if the recorded target no longer validates, it is rebuilt
rather than trusted.

`tunnel`, `stop`, `version`, and the `runtime` admin commands deliberately do not
true up, because they carry no runtime.

If your state predates this contract, it fails with one concise message naming the
state file to remove and the `connect` command to re-run. This POC intentionally
carries no compatibility shim for pre-descriptor state.

## Familiar runtime activation (admin/debug seam)

A *runtime* is an immutable Nix output—normally the flake installable
`github:gisikw/familiar/<commit>#familiar-worker-runtime`—whose `bin/` provides the
tools Herdr panes must resolve first, above all `pi`. `familiar-fleet` keeps two
stable, node-local pointers under the state directory:

```text
<state>/runtime/current   -> /nix/store/…-familiar-worker-runtime   (in use)
<state>/runtime/previous  -> /nix/store/…-familiar-worker-runtime   (last good)
<state>/runtime/gcroots/… -> /nix/store/…-familiar-worker-runtime   (Nix roots)
```

```sh
familiar-fleet runtime apply github:gisikw/familiar/<commit>#familiar-worker-runtime
familiar-fleet runtime apply /nix/store/<hash>-familiar-worker-runtime   # already built
familiar-fleet runtime status
familiar-fleet runtime rollback
```

`apply` runs `nix build --no-link --print-out-paths` for a flake installable (or
accepts an existing absolute store path), validates that the output is a directory
with an executable `bin/pi`, smoke-runs `bin/pi --version`, then atomically repoints
`current` (temporary symlink + `rename(2)`), first moving the old `current` to
`previous`. It is idempotent: re-applying the active target changes nothing. A
failed build, validation, smoke test, or GC-root registration leaves both pointers
untouched. The active and previous outputs are registered as indirect Nix GC roots
(only real `/nix/store` outputs can be collected, so only those get roots); older
roots are pruned after successful activation. `rollback` swaps the two pointers
and refuses if `previous` is missing or unusable. `status` reports both pointers and
exits non-zero when nothing usable is active.

**A manual `runtime apply` is an override, not a new authority.** The enrolled
descriptor remains the source of truth, so the next normal interactive start or
`familiar-fleet herdr` start reconciles the node back to the enrolled runtime.
`apply` says so on stderr. Use these commands to debug or to pin a build
temporarily; use `connect` to change what the node is actually enrolled to run.

Because the Herdr server and every pane reference `<state>/runtime/current/bin`
rather than a store path, applying a new runtime takes effect in new panes and even
in existing pane shells immediately. **No Herdr restart is required** for ordinary
runtime changes. There is no polling daemon, release manifest, or credential
handling in this slice; `runtime apply` is a narrow, explicit, idempotent step that
another process may invoke. The one automatic behavior is convergence on the
enrolled descriptor described above.

## Herdr pane environment

Before the Familiar-owned Herdr server starts (`familiar-fleet` or
`familiar-fleet herdr`), the node is first trued up to the enrolled runtime (see
above), and then a preflight:

1. requires a valid `runtime/current`, which the true-up has just established;
2. requires `FAMILIAR_TIAMAT_URL` and a usable Tiamat token file (see below);
3. resolves the user's shell from `$SHELL` (falling back to `/bin/sh` with a logged
   note; refusing to select the Familiar launcher itself);
4. regenerates `<state>/pane-shell` and `<state>/shell/*`;
5. projects the runtime's public Pi settings into writable `<state>/runtime/pi`, with
   its extension path routed through the stable `runtime/current` pointer;
6. writes `<state>/herdr-config.toml`—the user's own Herdr `config.toml` with
   Familiar's `[terminal]` `default_shell` and `shell_mode` applied, everything else
   preserved—and validates it with `herdr config check`;
7. starts `herdr server` with `runtime/current/bin` first on `PATH`,
   `PI_CODING_AGENT_DIR` pointing at the projected profile, `HERDR_CONFIG_PATH`
   pointing at the generated file, and the resolved `FAMILIAR_TIAMAT_URL` and
   `FAMILIAR_TIAMAT_TOKEN_FILE` exported.

Steps 1 and 2 are validated before anything is generated, so a misconfigured node
fails loudly and without side effects.

### Tiamat preflight

Herdr agents talk to a Tiamat router, so both inputs must be settled before the
server starts rather than surfacing later as an opaque in-pane failure:

- **`FAMILIAR_TIAMAT_URL` is required and must be non-empty.** There is no sensible
  default, so the preflight refuses to start Herdr without it and names the variable
  in the error. Set it in the service environment.
- **The token file must already exist.** If `FAMILIAR_TIAMAT_TOKEN_FILE` is set it is
  used as-is; otherwise the path is deterministically `<state>/secrets/tiamat.token`.
  Either way the resolved path is exported to the Herdr server and every pane, so a
  normal agent start inherits it.

The file must be a **readable, regular, non-empty file**. Its contents are never
read, validated, logged, or placed in the environment—only the path is. A dummy or
placeholder token is perfectly valid, because some routers behind the firewall do
not actually require authentication; the check is that a token *file* is present,
not that the token is meaningful.

Provisioning that file is the operator's job. `familiar-fleet` never creates it,
never downloads a secret, and never persists an OAuth token: the runtime stays
public and immutable, and nothing in this path writes credentials.

```sh
mkdir -p -m 700 "$STATE/secrets"
install -m 600 /dev/null "$STATE/secrets/tiamat.token"
printf '%s' "$TIAMAT_TOKEN" > "$STATE/secrets/tiamat.token"   # a placeholder is fine
```

`<state>/secrets/` should be mode `0700` and the token file mode `0600`. The
operational log records the router URL and the token *path* only.

Environment supplied by `workspace.create` may still override semantic values where
applicable; ordinary Herdr agent starts inherit these preflighted values.

Only native Herdr configuration is used; Herdr is not patched. The TUI client keeps
reading the user's own config for keybindings and chrome.

`pane-shell` is an **environment adapter around the user's shell, not a replacement
shell**. It execs the user's own shell so their normal interactive startup runs
first—aliases, functions, prompt, plugins—and then, as the very last step, prepends
`runtime/current/bin` to `PATH` so a bare `pi` resolves to the activated runtime no
matter what the startup files did. `$SHELL` inside the pane is the user's real
shell. The launcher owns the login decision, mirroring Herdr's `auto` policy (login
startup on macOS, non-login elsewhere), and configures Herdr's `shell_mode` to
`non_login` so Herdr does not re-wrap the launcher.

| Shell | Mechanism |
| --- | --- |
| bash | `bash --rcfile <state>/shell/bashrc -i`; the rcfile emulates bash's own selection (login: `/etc/profile` then the first of `~/.bash_profile`, `~/.bash_login`, `~/.profile`; otherwise `~/.bashrc`), then asserts `PATH`. |
| zsh | `ZDOTDIR=<state>/shell/zdotdir`; its `.zshenv`/`.zprofile`/`.zshrc`/`.zlogin` source the user's real files from their original `ZDOTDIR` (or `$HOME`, following a `.zshenv` that relocates it), assert `PATH` after `.zshrc` (or `.zlogin` for login shells), and restore `ZDOTDIR` so nested shells and `.zlogout` behave normally. |
| fish | `fish -C '<assert>'`; `-C` runs after `config.fish` and `conf.d/` by design. |
| sh, dash, ksh, mksh, ash, busybox | `ENV=<state>/shell/posix-env`, which sources the user's original `$ENV` first and asserts last. |
| anything else | Exec with `PATH` pre-set plus a one-line warning that the shell's own startup files may override it. |

If the runtime disappears after startup (for example, the store path is collected),
the launcher prints a warning and still gives you a usable shell; the server-side
preflight is where absence is fatal.

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

### Enrollment response contract

The enrollment response must include the runtime descriptor, which the client
validates strictly and rejects the whole enrollment without:

```json
{
  "runtime": {
    "schema": 1,
    "installable": "github:gisikw/familiar/<40-hex-commit>#familiar-worker-runtime"
  }
}
```

Unknown JSON fields are rejected, so the deployment and this client must agree on
the contract rather than silently diverging.

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
familiar-fleet                         tunnel + visible Herdr TUI (reattaches)
familiar-fleet connect <familiar-url>  browser setup, enrollment, runtime activation
familiar-fleet stop                    stop the familiar-fleet Herdr server/session
familiar-fleet runtime apply <inst>    admin override: build/validate/activate a runtime
familiar-fleet runtime rollback        swap runtime/current and runtime/previous
familiar-fleet runtime status          show both pointers and validity
familiar-fleet herdr                   managed/headless Herdr server only
familiar-fleet tunnel                  callback sshd + reconnecting tunnel only
familiar-fleet version                 print version
```

### Interactive lifecycle

A plain `familiar-fleet` does three things: it trues the node up to the enrolled
runtime, ensures the named `familiar-fleet` Herdr server is running, and starts the
tunnel infrastructure plus the visible TUI.

The Herdr server is **not** owned by the TUI. On a fresh start this invocation
spawns it as a session leader; if a healthy server for that session is already
running, the invocation reuses it. That detection is load-bearing rather than an
optimization, because Herdr refuses to start a second server for a live session.
A running server whose protocol the pinned CLI reports as incompatible is reported
as an error telling you to run `familiar-fleet stop`, never silently reused or
killed.

Leaving the TUI stops the tunnel and callback `sshd` and leaves the Herdr server,
session, and agents running. Cleanup follows ownership: if the tunnel or the TUI
fails to start, a server *this* invocation spawned is torn down, but a server it
merely reused is never terminated.

`familiar-fleet stop` ends the session explicitly and idempotently. It is scoped to
the `familiar-fleet` session by name, so other Herdr sessions on the machine are
never inspected or stopped, and stopping something already stopped succeeds. It
runs **before** the enrollment, runtime, and Tiamat preflights, so it remains a
working recovery path when configuration is broken.

Because Herdr derives its session socket from `XDG_CONFIG_HOME` (not
`HERDR_CONFIG_PATH`), run `stop` with the same `XDG_CONFIG_HOME` as the server you
mean to stop. That is automatic for ordinary interactive use, but worth checking if
a service unit sets a different one.

### Service semantics

`herdr` and the interactive command run the preflight described above and start the
Herdr server with `runtime/current/bin` first on `PATH` and the generated
`HERDR_CONFIG_PATH`; all other environment is inherited from the service that starts
it. `tunnel` writes operational output to the state log and does not launch a TUI or
Herdr server. These commands are intended to be separate managed services.

**The `herdr` supervisor owns shutdown.** Unlike the interactive command, its
lifetime *is* the session's lifetime, so TERM from the service manager stops the
named Herdr server. Leaving an orphaned server that no unit tracks would make
`systemctl --user stop` and `launchctl bootout` dishonest, and the next start would
then silently adopt a server the operator believed they had stopped. If a server is
already running when the service starts, it is adopted rather than duplicated, and
is still stopped when the service stops. Use the interactive command, not the
service, when you want a session that outlives the process supervising it.

TERM/INT causes bounded shutdown; child process groups receive TERM and then KILL if
necessary. An unexpected local `sshd` or Herdr server exit fails the owning command
so a service manager can restart it. Tunnel failures reconnect with bounded,
jittered backoff.

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
- generated `authorized_keys`, `known_hosts`, `sshd_config`, and wrappers;
- `runtime/current` and `runtime/previous`: symlinks to activated runtimes;
- `runtime/applied.json`: which installable produced the active runtime, so a
  repeated true-up to the same descriptor is provably a no-op;
- `runtime/gcroots/`: registered Nix roots retaining those two runtime outputs;
- `runtime/pi/`: writable Pi profile projected from the active runtime;
- `secrets/`: operator-provisioned node-local secrets, mode `0700`; holds
  `tiamat.token` (mode `0600`) unless `FAMILIAR_TIAMAT_TOKEN_FILE` overrides the
  path. Never created, written, or read by this client—only checked for presence;
- `pane-shell` and `shell/`: generated Herdr pane launcher and per-shell startup
  files (regenerated on every server start);
- `herdr-config.toml`: generated server configuration.

Private material, state, generated authorization files, and logs are owner-only.
State writes are atomic; existing unsafe permissions are rejected. OAuth tokens are
not part of state or logs. Because state paths are embedded in generated shell
files, the state directory may not contain a colon, single quote, or control
character.

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

Connect interactively once before installing services; `connect` also activates the
enrolled runtime, and each service start trues it up again.
Provision `<state>/secrets/tiamat.token` and set `FAMILIAR_TIAMAT_URL` in the
`herdr` unit as well; without both, the Herdr server refuses to start.
Always choose an explicit,
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
rotation. The enrolled runtime descriptor is trusted as authority from the
deployment: the client enforces that it names one exact, immutable Familiar commit,
but a compromised deployment could still enroll a node to a commit of its choosing.
The manual `runtime apply` seam trusts whatever the operator passes it, and is
reconciled back to the enrolled runtime on the next normal start. The generated pane
launcher lives in the owner-only state directory alongside the other wrappers.

Leaving agents running after the TUI exits is a deliberate availability choice with
a cost: a long-lived Herdr session and its agents survive until something stops
them, so `familiar-fleet stop` is the intended way to end that exposure.

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
process arguments, loopback `sshd`, tunnel reconnect behavior, runtime pointer
atomicity/idempotency/rollback, Herdr config merging, and pane-launcher generation.
They also cover the runtime descriptor contract (exact-commit pinning, rejected
branch refs and schemas, the pre-descriptor migration message), true-up idempotency
and reconciliation of a manual override, and the Herdr session lifecycle: fresh
start, reuse of a healthy server, TUI exit leaving the session and its agents
running, explicit idempotent `stop`, stop leaving unrelated sessions alone, refusal
to reuse an incompatible server, and cleanup limited to a server the invocation
spawned. Lifecycle tests drive a stand-in Herdr CLI that models the native
affordances (a session that outlives the TUI, `status server --json`, `server stop`,
and refusal to start a second server), so they need no real agents.
Live launcher tests run each supported shell with a user rc file that deliberately
shadows `pi`, then check that the rc ran, aliases survive, and `pi` still resolves to
the runtime; they skip when a shell is absent (the dev shell provides bash, zsh, fish,
and dash). No live identity provider or privileged port is required.
