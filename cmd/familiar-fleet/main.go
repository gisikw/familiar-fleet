package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"os/user"
	"strings"
	"syscall"

	"github.com/gisikw/familiar-fleet/internal/client"
)

var version = "dev"

type options struct {
	endpoint, name, tokenFile, hostKey, stateDir string
	herdr, ssh, sshd, keygen                     string
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

func run(args []string) error {
	fs := flag.NewFlagSet("familiar-fleet", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	o := options{}
	fs.StringVar(&o.endpoint, "endpoint", os.Getenv("FAMILIAR_FLEET_ENDPOINT"), "Familiar deployment base URL (or FAMILIAR_FLEET_ENDPOINT)")
	fs.StringVar(&o.name, "name", "", "requested machine label (first enrollment only; defaults to hostname)")
	fs.StringVar(&o.tokenFile, "token-file", "", "owner-only file containing enrollment bearer token")
	fs.StringVar(&o.hostKey, "rendezvous-host-key", os.Getenv("FAMILIAR_FLEET_RENDEZVOUS_HOST_KEY"), "trusted OpenSSH rendezvous host public key")
	fs.StringVar(&o.stateDir, "state-dir", "", "state directory override")
	fs.StringVar(&o.herdr, "herdr", "herdr", "Herdr executable")
	fs.StringVar(&o.ssh, "ssh", "ssh", "OpenSSH client executable")
	fs.StringVar(&o.sshd, "sshd", "sshd", "OpenSSH server executable")
	fs.StringVar(&o.keygen, "ssh-keygen", "ssh-keygen", "ssh-keygen executable")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: familiar-fleet [options] [run|enroll|version]\n\nrun is the default. Options:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	command := "run"
	if fs.NArg() > 0 {
		command = fs.Arg(0)
	}
	if fs.NArg() > 1 {
		return errors.New("too many arguments")
	}
	if command == "version" {
		fmt.Println(version)
		return nil
	}
	if command != "run" && command != "enroll" {
		fs.Usage()
		return fmt.Errorf("unknown command %q", command)
	}

	paths, err := client.StatePaths(o.stateDir)
	if err != nil {
		return err
	}
	if err := client.EnsureStateDir(paths.Dir); err != nil {
		return fmt.Errorf("prepare state: %w", err)
	}
	keygen, err := client.FindBinary(o.keygen)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	tunnelPublic, err := client.EnsureKey(ctx, keygen, paths.TunnelKey, "familiar-fleet-tunnel")
	if err != nil {
		return fmt.Errorf("prepare tunnel identity: %w", err)
	}
	hostPublic, err := client.EnsureKey(ctx, keygen, paths.HostKey, "familiar-fleet-local-sshd")
	if err != nil {
		return fmt.Errorf("prepare local SSH host identity: %w", err)
	}

	state, err := client.LoadState(paths.State)
	if err != nil {
		return err
	}
	if state == nil {
		if o.endpoint == "" {
			return errors.New("first enrollment requires --endpoint")
		}
		if err := client.ValidateEndpoint(o.endpoint); err != nil {
			return err
		}
		current, err := user.Current()
		if err != nil {
			return fmt.Errorf("determine local user: %w", err)
		}
		name := o.name
		if name == "" {
			name, err = os.Hostname()
			if err != nil {
				return fmt.Errorf("determine hostname: %w", err)
			}
			name = strings.SplitN(name, ".", 2)[0]
		}
		token, err := client.ReadToken(o.tokenFile)
		if err != nil {
			return fmt.Errorf("read enrollment token: %w", err)
		}
		e := client.Enroller{Endpoint: o.endpoint, Token: token}
		enrollment, err := e.Enroll(ctx, client.EnrollmentRequest{Host: name, TunnelPublicKey: tunnelPublic, SSHHostPublicKey: hostPublic, SSHUser: current.Username})
		if err != nil {
			return err
		}
		if err := applyHostKey(&enrollment, o.hostKey); err != nil {
			return err
		}
		state = &client.State{Endpoint: strings.TrimRight(o.endpoint, "/"), SSHUser: current.Username, Enrollment: enrollment}
		if err := client.SaveState(paths.State, *state); err != nil {
			return fmt.Errorf("save enrollment: %w", err)
		}
		fmt.Fprintf(os.Stderr, "enrolled as %s (node %s; reverse port %d)\n", enrollment.Host, enrollment.NodeID, enrollment.Port)
	} else {
		if o.endpoint != "" && strings.TrimRight(o.endpoint, "/") != state.Endpoint {
			return errors.New("configured endpoint differs from saved enrollment; use a separate --state-dir to enroll elsewhere")
		}
		if o.name != "" {
			return errors.New("--name cannot change an existing server-owned enrollment")
		}
		if o.hostKey != "" {
			provided, err := client.NormalizePublicKey(o.hostKey)
			if err != nil {
				return fmt.Errorf("rendezvous host key: %w", err)
			}
			saved, _ := client.NormalizePublicKey(state.Enrollment.TunnelHostKey)
			if provided != saved {
				return errors.New("provided rendezvous host key does not match the pinned key")
			}
		}
	}
	if command == "enroll" {
		fmt.Fprintf(os.Stdout, "enrolled: host=%s node_id=%s reverse_port=%d rendezvous=%s:%d\n", state.Enrollment.Host, state.Enrollment.NodeID, state.Enrollment.Port, state.Enrollment.TunnelHost, state.Enrollment.TunnelSSHPort)
		return nil
	}

	current, err := user.Current()
	if err != nil {
		return err
	}
	if current.Username != state.SSHUser {
		return fmt.Errorf("enrollment belongs to local user %q, but this process runs as %q", state.SSHUser, current.Username)
	}
	herdr, err := client.FindBinary(o.herdr)
	if err != nil {
		return err
	}
	ssh, err := client.FindBinary(o.ssh)
	if err != nil {
		return err
	}
	sshd, err := client.FindBinary(o.sshd)
	if err != nil {
		return err
	}
	logger := log.New(os.Stderr, "familiar-fleet: ", log.LstdFlags)
	runtime := client.Runtime{Paths: paths, Enrollment: state.Enrollment, LocalUser: state.SSHUser, Herdr: herdr, SSH: ssh, SSHD: sshd, Stdout: os.Stdout, Stderr: os.Stderr, Logger: logger}
	logger.Printf("starting node %s (%s)", state.Enrollment.Host, state.Enrollment.NodeID)
	return runtime.Run(ctx)
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
