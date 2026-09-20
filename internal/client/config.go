package client

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func WriteRuntimeConfig(paths Paths, enrollment Enrollment, localUser, herdrPath string, port int) error {
	if !userRE.MatchString(localUser) {
		return fmt.Errorf("local SSH user %q is unsafe or unsupported", localUser)
	}
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid local SSH port")
	}
	controller, err := NormalizePublicKey(enrollment.ControllerPublicKey)
	if err != nil {
		return err
	}
	authorized := "no-port-forwarding,no-agent-forwarding,no-X11-forwarding,no-pty " + controller + " familiar-fleet-controller\n"
	if err := atomicWrite(paths.AuthorizedKeys, []byte(authorized), 0600); err != nil {
		return err
	}

	hostKey, err := NormalizePublicKey(enrollment.TunnelHostKey)
	if err != nil {
		return fmt.Errorf("rendezvous host key: %w", err)
	}
	knownName := enrollment.TunnelHost
	if enrollment.TunnelSSHPort != 22 {
		knownName = "[" + knownName + "]:" + strconv.Itoa(enrollment.TunnelSSHPort)
	}
	if err := atomicWrite(paths.KnownHosts, []byte(knownName+" "+hostKey+"\n"), 0600); err != nil {
		return err
	}

	if strings.ContainsAny(herdrPath, "\r\n") {
		return fmt.Errorf("invalid herdr path")
	}
	wrapper := "#!/bin/sh\nexec " + shellQuote(herdrPath) + " \"$@\"\n"
	if err := atomicWrite(paths.HerdrWrapper, []byte(wrapper), 0700); err != nil {
		return err
	}
	// sshd invokes the user's shell for remote commands, and login policy may
	// replace a configured PATH. Force all controller commands through a tiny
	// bridge that puts this state-owned pinned Herdr wrapper first.
	bridge := "#!/bin/sh\n" +
		"PATH=" + shellQuote(paths.Dir+":/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin") + "\n" +
		"export PATH\n" +
		"test -n \"${SSH_ORIGINAL_COMMAND:-}\" || exit 64\n" +
		"exec /bin/sh -c \"$SSH_ORIGINAL_COMMAND\"\n"
	if err := atomicWrite(paths.SSHBridge, []byte(bridge), 0700); err != nil {
		return err
	}

	q := sshdQuote
	config := strings.Join([]string{
		"AddressFamily inet",
		"ListenAddress 127.0.0.1",
		"Port " + strconv.Itoa(port),
		"HostKey " + q(paths.HostKey),
		"PidFile " + q(paths.SSHDPid),
		"AuthorizedKeysFile " + q(paths.AuthorizedKeys),
		"AllowUsers " + localUser,
		"AuthenticationMethods publickey",
		"PubkeyAuthentication yes",
		"PasswordAuthentication no",
		"KbdInteractiveAuthentication no",
		"ChallengeResponseAuthentication no",
		"PermitEmptyPasswords no",
		"UsePAM no",
		"PermitRootLogin no",
		"StrictModes yes",
		"AllowAgentForwarding no",
		"AllowTcpForwarding no",
		"GatewayPorts no",
		"X11Forwarding no",
		"PermitTunnel no",
		"PermitTTY no",
		"PermitUserEnvironment no",
		"PermitUserRC no",
		"ForceCommand " + q(paths.SSHBridge),
		"LogLevel VERBOSE",
		"Subsystem sftp internal-sftp",
		"", // final newline
	}, "\n")
	return atomicWrite(paths.SSHDConfig, []byte(config), 0600)
}

func sshdQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func TunnelArgs(paths Paths, e Enrollment) []string {
	return []string{
		"-F", os.DevNull,
		"-N", "-T",
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "IdentityFile=" + paths.TunnelKey,
		"-o", "UserKnownHostsFile=" + paths.KnownHosts,
		"-o", "GlobalKnownHostsFile=" + os.DevNull,
		"-o", "StrictHostKeyChecking=yes",
		"-o", "CheckHostIP=no",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "ConnectTimeout=15",
		"-o", "ConnectionAttempts=1",
		"-o", "RequestTTY=no",
		"-o", "ForwardAgent=no",
		"-o", "ClearAllForwardings=no",
		"-p", strconv.Itoa(e.TunnelSSHPort),
		"-R", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%%LOCAL_PORT%%", e.Port),
		"--", e.TunnelUser + "@" + e.TunnelHost,
	}
}

func TunnelArgsForPort(paths Paths, e Enrollment, localPort int) []string {
	args := TunnelArgs(paths, e)
	for i := range args {
		args[i] = strings.ReplaceAll(args[i], "%LOCAL_PORT%", strconv.Itoa(localPort))
	}
	return args
}
