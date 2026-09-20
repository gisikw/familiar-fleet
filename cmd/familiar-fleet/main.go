package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gisikw/familiar-fleet/internal/client"
	fleetruntime "github.com/gisikw/familiar-fleet/internal/runtime"
)

var version = "dev"

type options struct {
	endpoint, name, tokenFile, hostKey, stateDir string
	herdr, ssh, sshd, keygen, nix, nixStore      string
}

type invocation struct {
	options options
	command string
	args    []string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "familiar-fleet:", err)
		os.Exit(1)
	}
}

func parseInvocation(args []string, output io.Writer) (invocation, error) {
	fs := flag.NewFlagSet("familiar-fleet", flag.ContinueOnError)
	fs.SetOutput(output)
	o := options{}
	fs.StringVar(&o.endpoint, "endpoint", os.Getenv("FAMILIAR_FLEET_ENDPOINT"), "deployment URL (debug enroll compatibility only)")
	fs.StringVar(&o.name, "name", "", "machine name (debug enroll compatibility only)")
	fs.StringVar(&o.tokenFile, "token-file", "", "owner-only bearer-token file (debug enroll compatibility only)")
	fs.StringVar(&o.hostKey, "rendezvous-host-key", os.Getenv("FAMILIAR_FLEET_RENDEZVOUS_HOST_KEY"), "trusted OpenSSH rendezvous host public key")
	fs.StringVar(&o.stateDir, "state-dir", "", "state directory override")
	fs.StringVar(&o.herdr, "herdr", "herdr", "Herdr executable")
	fs.StringVar(&o.ssh, "ssh", "ssh", "OpenSSH client executable")
	fs.StringVar(&o.sshd, "sshd", "sshd", "OpenSSH server executable")
	fs.StringVar(&o.keygen, "ssh-keygen", "ssh-keygen", "ssh-keygen executable")
	fs.StringVar(&o.nix, "nix", "nix", "nix executable (runtime apply)")
	fs.StringVar(&o.nixStore, "nix-store", "nix-store", "nix-store executable (runtime GC roots)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: familiar-fleet [options] [connect <familiar-url>|runtime <apply|rollback|status>|herdr|tunnel|version]")
		fmt.Fprintln(fs.Output(), "       familiar-fleet [options]              # tunnel + visible Herdr TUI")
		fmt.Fprintln(fs.Output(), "\nRuntime activation (required before Herdr starts):")
		fmt.Fprintln(fs.Output(), "       familiar-fleet runtime apply github:gisikw/familiar/<commit>#familiar-worker-runtime")
		fmt.Fprintln(fs.Output(), "       familiar-fleet runtime apply /nix/store/<hash>-familiar-worker-runtime")
		fmt.Fprintln(fs.Output(), "       familiar-fleet runtime rollback        # swap runtime/current and runtime/previous")
		fmt.Fprintln(fs.Output(), "       familiar-fleet runtime status")
		fmt.Fprintln(fs.Output(), "\nOptions must precede the command:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return invocation{}, err
	}
	inv := invocation{options: o, command: "interactive"}
	if fs.NArg() > 0 {
		inv.command = fs.Arg(0)
		inv.args = fs.Args()[1:]
	}
	switch inv.command {
	case "connect":
		if len(inv.args) != 1 {
			return invocation{}, errors.New("usage: familiar-fleet [options] connect <familiar-url>")
		}
	case "runtime":
		usage := errors.New("usage: familiar-fleet [options] runtime apply <installable> | runtime rollback | runtime status")
		if len(inv.args) == 0 {
			return invocation{}, usage
		}
		switch inv.args[0] {
		case "apply":
			if len(inv.args) != 2 {
				return invocation{}, usage
			}
		case "rollback", "status":
			if len(inv.args) != 1 {
				return invocation{}, usage
			}
		default:
			return invocation{}, usage
		}
	case "interactive", "run", "herdr", "tunnel", "version", "enroll":
		if len(inv.args) != 0 {
			return invocation{}, fmt.Errorf("command %s takes no arguments", inv.command)
		}
	default:
		fs.Usage()
		return invocation{}, fmt.Errorf("unknown command %q", inv.command)
	}
	return inv, nil
}

func run(args []string) error {
	inv, err := parseInvocation(args, os.Stderr)
	if err != nil {
		return err
	}
	o := inv.options
	if inv.command == "version" {
		fmt.Println(version)
		return nil
	}

	paths, err := client.StatePaths(o.stateDir)
	if err != nil {
		return err
	}
	if err := client.EnsureStateDir(paths.Dir); err != nil {
		return fmt.Errorf("prepare state: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if inv.command == "runtime" {
		return runtimeCommand(ctx, paths, o, inv.args)
	}
	state, err := client.LoadState(paths.State)
	if err != nil {
		return err
	}
	if state == nil && inv.command != "connect" && inv.command != "enroll" {
		return errors.New("not connected; run: familiar-fleet connect <familiar-url>")
	}
	if inv.command == "connect" {
		if state != nil {
			return fmt.Errorf("already connected to %s; use a separate --state-dir for another deployment", state.Endpoint)
		}
		if o.tokenFile != "" || o.name != "" {
			return errors.New("connect uses browser authentication and its name form; --token-file and --name are only for debug enroll")
		}
		return connect(ctx, paths, o, inv.args[0])
	}
	if inv.command == "enroll" {
		return debugEnroll(ctx, paths, o, state)
	}

	current, err := user.Current()
	if err != nil {
		return fmt.Errorf("determine local user: %w", err)
	}
	if current.Username != state.SSHUser {
		return fmt.Errorf("enrollment belongs to local user %q, but this process runs as %q", state.SSHUser, current.Username)
	}
	herdr, err := client.FindBinary(o.herdr)
	if err != nil {
		return err
	}
	runtime := client.Runtime{
		Paths: paths, Enrollment: state.Enrollment, LocalUser: state.SSHUser,
		Herdr: herdr, Stdin: os.Stdin, TUIOut: os.Stdout, TUIErr: os.Stderr,
	}
	if inv.command == "tunnel" {
		return runTunnel(ctx, paths, o, state, runtime)
	}

	// Preflight: the Familiar-owned Herdr server never starts without an
	// activated runtime, a generated pane launcher, and a validated config.
	herdrEnv, err := client.PrepareHerdrEnvironment(ctx, paths, herdr, os.Environ())
	if err != nil {
		return err
	}
	runtime.HerdrEnv = herdrEnv.Env
	if inv.command == "herdr" {
		runtime.Stdout, runtime.Stderr = os.Stdout, os.Stderr
		runtime.Logger = log.New(os.Stderr, "familiar-fleet: ", log.LstdFlags)
		logHerdrEnv(runtime.Logger, herdrEnv)
		return runtime.RunHerdr(ctx)
	}

	ssh, err := client.FindBinary(o.ssh)
	if err != nil {
		return err
	}
	sshd, err := client.FindBinary(o.sshd)
	if err != nil {
		return err
	}
	logFile, err := client.OpenLog(paths.Log)
	if err != nil {
		return fmt.Errorf("open operational log: %w", err)
	}
	defer logFile.Close()
	runtime.SSH, runtime.SSHD = ssh, sshd
	runtime.Stdout, runtime.Stderr = logFile, logFile
	runtime.Logger = log.New(logFile, "familiar-fleet: ", log.LstdFlags)
	runtime.Logger.Printf("starting node %s (%s)", state.Enrollment.Host, state.Enrollment.NodeID)
	logHerdrEnv(runtime.Logger, herdrEnv)
	return runtime.RunInteractive(ctx)
}

func runTunnel(ctx context.Context, paths client.Paths, o options, state *client.State, runtime client.Runtime) error {
	ssh, err := client.FindBinary(o.ssh)
	if err != nil {
		return err
	}
	sshd, err := client.FindBinary(o.sshd)
	if err != nil {
		return err
	}
	logFile, err := client.OpenLog(paths.Log)
	if err != nil {
		return fmt.Errorf("open operational log: %w", err)
	}
	defer logFile.Close()
	runtime.SSH, runtime.SSHD = ssh, sshd
	runtime.Stdout, runtime.Stderr = logFile, logFile
	runtime.Logger = log.New(logFile, "familiar-fleet: ", log.LstdFlags)
	runtime.Logger.Printf("starting node %s (%s)", state.Enrollment.Host, state.Enrollment.NodeID)
	return runtime.RunTunnel(ctx)
}

func logHerdrEnv(logger *log.Logger, env client.HerdrEnvironment) {
	logger.Printf("Familiar runtime %s; pane shell adapts around %s", env.Runtime, env.UserShell)
	// Path and URL only: the token value is never read by this process.
	logger.Printf("Tiamat router %s; token file %s", env.Tiamat.URL, env.Tiamat.TokenFile)
	for _, note := range env.Notes {
		logger.Printf("note: %s", note)
	}
}

func runtimeCommand(ctx context.Context, paths client.Paths, o options, args []string) error {
	store := fleetruntime.New(paths.RuntimeDir)
	switch args[0] {
	case "status":
		st := store.Status()
		fmt.Printf("runtime dir: %s\n", st.Dir)
		printPointer("current", st.Current, st.CurrentErr)
		printPointer("previous", st.Previous, st.PreviousErr)
		if st.CurrentErr != nil {
			return errors.New("no usable runtime is activated")
		}
		return nil
	case "rollback":
		target, err := store.Rollback()
		if err != nil {
			return err
		}
		if err := store.PruneGCRoots(); err != nil {
			return fmt.Errorf("runtime rolled back, but pruning old GC roots failed: %w", err)
		}
		fmt.Printf("runtime/current -> %s\n", target)
		return nil
	case "apply":
		nix := o.nix
		if !filepath.IsAbs(args[1]) {
			var err error
			if nix, err = client.FindBinary(o.nix); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "building %s\n", args[1])
		}
		target, err := fleetruntime.Build(ctx, nix, args[1])
		if err != nil {
			return err
		}
		if err := fleetruntime.Validate(target); err != nil {
			return err
		}
		if err := fleetruntime.Smoke(ctx, target); err != nil {
			return fmt.Errorf("runtime failed validation; not activated: %w", err)
		}
		nixStore, err := client.FindBinary(o.nixStore)
		if err != nil {
			return err
		}
		if err := store.RegisterGCRoot(ctx, nixStore, target); err != nil {
			return fmt.Errorf("retain runtime: %w", err)
		}
		changed, err := store.Activate(target)
		if err != nil {
			return err
		}
		if err := store.PruneGCRoots(); err != nil {
			return fmt.Errorf("runtime activated, but pruning old GC roots failed: %w", err)
		}
		if changed {
			fmt.Printf("activated runtime/current -> %s\n", target)
			fmt.Println("new Herdr panes use it immediately; no restart required")
		} else {
			fmt.Printf("runtime/current already -> %s\n", target)
		}
		return nil
	}
	return fmt.Errorf("unknown runtime subcommand %q", args[0])
}

func printPointer(name, target string, err error) {
	switch {
	case errors.Is(err, fleetruntime.ErrNoRuntime):
		fmt.Printf("%-9s (none)\n", name+":")
	case err != nil:
		fmt.Printf("%-9s %s (INVALID: %v)\n", name+":", target, err)
	default:
		fmt.Printf("%-9s %s (ok)\n", name+":", target)
	}
}

func prepareEnrollment(ctx context.Context, paths client.Paths, o options) (client.EnrollmentRequest, string, error) {
	keygen, err := client.FindBinary(o.keygen)
	if err != nil {
		return client.EnrollmentRequest{}, "", err
	}
	tunnelPublic, err := client.EnsureKey(ctx, keygen, paths.TunnelKey, "familiar-fleet-tunnel")
	if err != nil {
		return client.EnrollmentRequest{}, "", fmt.Errorf("prepare tunnel identity: %w", err)
	}
	hostPublic, err := client.EnsureKey(ctx, keygen, paths.HostKey, "familiar-fleet-local-sshd")
	if err != nil {
		return client.EnrollmentRequest{}, "", fmt.Errorf("prepare local SSH host identity: %w", err)
	}
	current, err := user.Current()
	if err != nil {
		return client.EnrollmentRequest{}, "", fmt.Errorf("determine local user: %w", err)
	}
	return client.EnrollmentRequest{TunnelPublicKey: tunnelPublic, SSHHostPublicKey: hostPublic, SSHUser: current.Username}, current.Username, nil
}

func shortHostname() (string, error) {
	name, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("determine hostname: %w", err)
	}
	return strings.SplitN(name, ".", 2)[0], nil
}

func connect(ctx context.Context, paths client.Paths, o options, rawEndpoint string) error {
	endpoint, err := client.CanonicalEndpoint(rawEndpoint)
	if err != nil {
		return err
	}
	request, username, err := prepareEnrollment(ctx, paths, o)
	if err != nil {
		return err
	}
	name, err := shortHostname()
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Opening your browser to authenticate and name this machine…")
	connector := client.Connector{}
	enrollment, err := connector.Connect(ctx, endpoint, name, request, func(enrollment client.Enrollment) error {
		if err := applyHostKey(&enrollment, o.hostKey); err != nil {
			return err
		}
		state := client.State{Endpoint: endpoint, SSHUser: username, Enrollment: enrollment}
		if err := client.SaveState(paths.State, state); err != nil {
			return fmt.Errorf("save enrollment: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Connected as %s (node %s). Run familiar-fleet to begin.\n", enrollment.Host, enrollment.NodeID)
	return nil
}

func debugEnroll(ctx context.Context, paths client.Paths, o options, state *client.State) error {
	if state != nil {
		fmt.Fprintf(os.Stdout, "enrolled: host=%s node_id=%s reverse_port=%d rendezvous=%s:%d\n", state.Enrollment.Host, state.Enrollment.NodeID, state.Enrollment.Port, state.Enrollment.TunnelHost, state.Enrollment.TunnelSSHPort)
		return nil
	}
	if o.endpoint == "" {
		return errors.New("debug enrollment requires --endpoint and --token-file")
	}
	endpoint, err := client.CanonicalEndpoint(o.endpoint)
	if err != nil {
		return err
	}
	request, username, err := prepareEnrollment(ctx, paths, o)
	if err != nil {
		return err
	}
	name := o.name
	if name == "" {
		name, err = shortHostname()
		if err != nil {
			return err
		}
	}
	request.Host = name
	token, err := client.ReadToken(o.tokenFile)
	if err != nil {
		return fmt.Errorf("read enrollment token: %w", err)
	}
	enrollment, err := (client.Enroller{Endpoint: endpoint, Token: token}).Enroll(ctx, request)
	token = ""
	if err != nil {
		return err
	}
	if err := applyHostKey(&enrollment, o.hostKey); err != nil {
		return err
	}
	if err := client.SaveState(paths.State, client.State{Endpoint: endpoint, SSHUser: username, Enrollment: enrollment}); err != nil {
		return fmt.Errorf("save enrollment: %w", err)
	}
	fmt.Fprintf(os.Stdout, "enrolled: host=%s node_id=%s reverse_port=%d rendezvous=%s:%d\n", enrollment.Host, enrollment.NodeID, enrollment.Port, enrollment.TunnelHost, enrollment.TunnelSSHPort)
	return nil
}

func applyHostKey(enrollment *client.Enrollment, supplied string) error {
	server := enrollment.TunnelHostKey
	if supplied == "" && server == "" {
		return errors.New("enrollment response omitted tunnel_host_key; supply the trusted key with --rendezvous-host-key")
	}
	if supplied != "" {
		key, err := client.NormalizePublicKey(supplied)
		if err != nil {
			return fmt.Errorf("rendezvous host key: %w", err)
		}
		if server != "" {
			serverKey, err := client.NormalizePublicKey(server)
			if err != nil {
				return err
			}
			if key != serverKey {
				return errors.New("server and explicitly supplied rendezvous host keys differ")
			}
		}
		enrollment.TunnelHostKey = key
	} else {
		key, err := client.NormalizePublicKey(server)
		if err != nil {
			return err
		}
		enrollment.TunnelHostKey = key
	}
	return nil
}
