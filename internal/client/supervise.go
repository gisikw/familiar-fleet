package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const herdrSession = "familiar-fleet"

type Runtime struct {
	Paths            Paths
	Enrollment       Enrollment
	LocalUser        string
	Herdr, SSH, SSHD string
	// HerdrEnv is the complete environment for the Familiar-owned Herdr server:
	// runtime/current/bin first on PATH and HERDR_CONFIG_PATH set to the
	// generated configuration. See PrepareHerdrEnvironment.
	HerdrEnv       []string
	Stdin          io.Reader
	Stdout, Stderr io.Writer // operational child output (normally the state log)
	Logger         *log.Logger
	TUIOut, TUIErr io.Writer
}

type child struct {
	cmd          *exec.Cmd
	done         chan error
	once         sync.Once
	processGroup bool
}

func startChild(path string, args []string, stdin io.Reader, stdout, stderr io.Writer) (*child, error) {
	return startChildWithAttrs(path, args, stdin, stdout, stderr, nil, &syscall.SysProcAttr{Setpgid: true}, true)
}

func startForegroundChild(path string, args []string, stdin io.Reader, stdout, stderr io.Writer) (*child, error) {
	// Inherit the parent's foreground process group. Putting a TUI that reads
	// the controlling terminal in a new, non-foreground group causes SIGTTIN.
	return startChildWithAttrs(path, args, stdin, stdout, stderr, nil, nil, false)
}

func startDetachedChild(path string, args []string, env []string, stdout, stderr io.Writer) (*child, error) {
	// Herdr requires its server process to be a session leader, matching its own
	// auto-spawn behavior and preserving sessions used by saved machines.
	return startChildWithAttrs(path, args, nil, stdout, stderr, env, &syscall.SysProcAttr{Setsid: true}, true)
}

func startChildWithAttrs(path string, args []string, stdin io.Reader, stdout, stderr io.Writer, env []string, attrs *syscall.SysProcAttr, processGroup bool) (*child, error) {
	cmd := exec.Command(path, args...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = stdout, stderr, stdin
	cmd.Env = env
	cmd.SysProcAttr = attrs
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &child{cmd: cmd, done: make(chan error, 1), processGroup: processGroup}
	go func() { c.done <- cmd.Wait(); close(c.done) }()
	return c, nil
}

func (c *child) stop(grace time.Duration) {
	if c == nil || c.cmd.Process == nil {
		return
	}
	c.once.Do(func() {
		select {
		case <-c.done:
			return
		default:
		}
		if c.processGroup {
			_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGTERM)
		} else {
			_ = c.cmd.Process.Signal(syscall.SIGTERM)
		}
		select {
		case <-c.done:
		case <-time.After(grace):
			if c.processGroup {
				_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
			} else {
				_ = c.cmd.Process.Kill()
			}
			<-c.done
		}
	})
}

func pickPort() (int, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func (r Runtime) startSSHD(ctx context.Context) (*child, int, error) {
	var last error
	for attempt := 0; attempt < 5; attempt++ {
		port, err := pickPort()
		if err != nil {
			return nil, 0, err
		}
		if err = WriteRuntimeConfig(r.Paths, r.Enrollment, r.LocalUser, r.Herdr, port); err != nil {
			return nil, 0, err
		}
		c, err := startChild(r.SSHD, []string{"-D", "-e", "-f", r.Paths.SSHDConfig}, nil, r.Stdout, r.Stderr)
		if err != nil {
			return nil, 0, fmt.Errorf("start sshd: %w", err)
		}
		deadline := time.NewTimer(3 * time.Second)
		tick := time.NewTicker(30 * time.Millisecond)
		ready := false
		for !ready {
			select {
			case err := <-c.done:
				last = fmt.Errorf("sshd exited before becoming ready: %w", exitError(err))
				ready = true
				c = nil
			case <-tick.C:
				conn, dialErr := net.DialTimeout("tcp4", "127.0.0.1:"+strconv.Itoa(port), 100*time.Millisecond)
				if dialErr == nil {
					_ = conn.Close()
					ready = true
				}
			case <-deadline.C:
				last = errors.New("timed out waiting for sshd")
				c.stop(time.Second)
				c = nil
				ready = true
			case <-ctx.Done():
				c.stop(time.Second)
				tick.Stop()
				deadline.Stop()
				return nil, 0, ctx.Err()
			}
		}
		tick.Stop()
		deadline.Stop()
		if c != nil {
			return c, port, nil
		}
	}
	return nil, 0, last
}

func HerdrServerArgs() []string { return []string{"--session", herdrSession, "server"} }
func HerdrTUIArgs() []string    { return []string{"--session", herdrSession} }
func HerdrStopArgs() []string   { return []string{"--session", herdrSession, "server", "stop"} }

func (r Runtime) waitHerdrReady(ctx context.Context, server *child) error {
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		probe := exec.CommandContext(probeCtx, r.Herdr, "--session", herdrSession, "status", "server", "--json")
		probe.Env = r.HerdrEnv
		output, _ := probe.Output()
		cancel()
		if strings.Contains(string(output), `"running":true`) {
			return nil
		}
		select {
		case err := <-server.done:
			return fmt.Errorf("Herdr server exited before becoming ready: %w", exitError(err))
		case <-ticker.C:
		case <-deadline.C:
			return errors.New("timed out waiting for Herdr server")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (r Runtime) startInfrastructure(ctx context.Context) (*child, context.CancelFunc, <-chan struct{}, int, error) {
	runCtx, cancel := context.WithCancel(ctx)
	sshd, localPort, err := r.startSSHD(runCtx)
	if err != nil {
		cancel()
		return nil, nil, nil, 0, err
	}
	r.Logger.Printf("local callback sshd listening on 127.0.0.1:%d", localPort)
	tunnelDone := make(chan struct{})
	go func() {
		defer close(tunnelDone)
		r.tunnelLoop(runCtx, localPort)
	}()
	return sshd, cancel, tunnelDone, localPort, nil
}

func (r Runtime) stopHerdrServer() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Herdr, HerdrStopArgs()...)
	cmd.Env = r.HerdrEnv
	cmd.Stdout, cmd.Stderr = r.Stdout, r.Stderr
	if err := cmd.Run(); err != nil {
		r.Logger.Printf("stopping Herdr session: %v", err)
	}
}

// RunInteractive owns one complete invocation: detached-compatible Herdr
// server, visible TUI, callback sshd, and reconnecting tunnel. Leaving the TUI
// intentionally tears all four down.
func (r Runtime) RunInteractive(ctx context.Context) error {
	sshd, cancel, tunnelDone, _, err := r.startInfrastructure(ctx)
	if err != nil {
		return err
	}
	defer func() {
		cancel()
		sshd.stop(5 * time.Second)
		<-tunnelDone
	}()

	server, err := startDetachedChild(r.Herdr, HerdrServerArgs(), r.HerdrEnv, r.Stdout, r.Stderr)
	if err != nil {
		return fmt.Errorf("start Herdr server: %w", err)
	}
	defer server.stop(10 * time.Second)
	if err := r.waitHerdrReady(ctx, server); err != nil {
		return err
	}
	r.Logger.Printf("Herdr session %s server started", herdrSession)

	tui, err := startForegroundChild(r.Herdr, HerdrTUIArgs(), r.Stdin, r.TUIOut, r.TUIErr)
	if err != nil {
		r.stopHerdrServer()
		return fmt.Errorf("start Herdr TUI: %w", err)
	}
	var result error
	select {
	case err := <-tui.done:
		if err != nil {
			result = fmt.Errorf("Herdr TUI exited: %w", err)
		}
	case err := <-server.done:
		result = fmt.Errorf("Herdr server exited unexpectedly: %w", exitError(err))
		tui.stop(5 * time.Second)
	case err := <-sshd.done:
		result = fmt.Errorf("local sshd exited unexpectedly: %w", exitError(err))
		tui.stop(5 * time.Second)
	case <-ctx.Done():
		tui.stop(5 * time.Second)
	}
	cancel()
	sshd.stop(5 * time.Second)
	<-tunnelDone
	r.stopHerdrServer()
	server.stop(10 * time.Second)
	return result
}

// RunHerdr is the managed-service/headless Herdr component. The server runs
// with HerdrEnv (runtime/current/bin first on PATH, generated
// HERDR_CONFIG_PATH), so terminals spawned by Herdr see that environment.
func (r Runtime) RunHerdr(ctx context.Context) error {
	server, err := startDetachedChild(r.Herdr, HerdrServerArgs(), r.HerdrEnv, r.Stdout, r.Stderr)
	if err != nil {
		return fmt.Errorf("start Herdr server: %w", err)
	}
	select {
	case err := <-server.done:
		if err != nil {
			return fmt.Errorf("Herdr server exited: %w", err)
		}
		return nil
	case <-ctx.Done():
		server.stop(10 * time.Second)
		return nil
	}
}

// RunTunnel runs only the local callback sshd and reconnecting reverse tunnel.
func (r Runtime) RunTunnel(ctx context.Context) error {
	sshd, cancel, tunnelDone, _, err := r.startInfrastructure(ctx)
	if err != nil {
		return err
	}
	var result error
	select {
	case err := <-sshd.done:
		result = fmt.Errorf("local sshd exited unexpectedly: %w", exitError(err))
	case <-ctx.Done():
	}
	cancel()
	sshd.stop(5 * time.Second)
	<-tunnelDone
	return result
}

func (r Runtime) tunnelLoop(ctx context.Context, localPort int) {
	attempt := 0
	for ctx.Err() == nil {
		args := TunnelArgsForPort(r.Paths, r.Enrollment, localPort)
		c, err := startChild(r.SSH, args, nil, r.Stdout, r.Stderr)
		if err == nil {
			r.Logger.Printf("reverse tunnel process started")
			started := time.Now()
			select {
			case err = <-c.done:
			case <-ctx.Done():
				c.stop(5 * time.Second)
				return
			}
			if time.Since(started) >= 2*time.Minute {
				attempt = 0
			}
		}
		if ctx.Err() != nil {
			return
		}
		delay := backoff(attempt)
		attempt++
		r.Logger.Printf("reverse tunnel unavailable (%v); retrying in %s", exitError(err), delay.Round(time.Millisecond))
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

func backoff(attempt int) time.Duration {
	if attempt > 5 {
		attempt = 5
	}
	base := time.Second << attempt
	if base > 30*time.Second {
		base = 30 * time.Second
	}
	maxJitter := base / 5
	if base+maxJitter > 30*time.Second {
		maxJitter = 30*time.Second - base
	}
	if maxJitter <= 0 {
		return base
	}
	return base + time.Duration(rand.Int63n(int64(maxJitter)+1))
}

func BackoffForTest(attempt int) time.Duration { return backoff(attempt) }

func exitError(err error) error {
	if err == nil {
		return errors.New("exit status 0")
	}
	return err
}

func FindBinary(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("required executable %q not found: %w", name, err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve executable %q: %w", name, err)
	}
	if strings.ContainsAny(path, "\r\n") {
		return "", errors.New("executable path contains a newline")
	}
	return path, nil
}

func OpenLog(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
