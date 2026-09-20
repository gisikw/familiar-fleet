package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Runtime struct {
	Paths            Paths
	Enrollment       Enrollment
	LocalUser        string
	Herdr, SSH, SSHD string
	Stdout, Stderr   io.Writer
	Logger           *log.Logger
}

type child struct {
	cmd  *exec.Cmd
	done chan error
	once sync.Once
}

func startChild(path string, args []string, stdout, stderr io.Writer) (*child, error) {
	return startChildWithAttrs(path, args, stdout, stderr, &syscall.SysProcAttr{Setpgid: true})
}

func startDetachedChild(path string, args []string, stdout, stderr io.Writer) (*child, error) {
	// Herdr identifies a server daemon by it being its own session leader. This
	// is also what Herdr's own auto-spawn path does on macOS and Linux.
	return startChildWithAttrs(path, args, stdout, stderr, &syscall.SysProcAttr{Setsid: true})
}

func startChildWithAttrs(path string, args []string, stdout, stderr io.Writer, attrs *syscall.SysProcAttr) (*child, error) {
	cmd := exec.Command(path, args...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = stdout, stderr, nil
	cmd.SysProcAttr = attrs
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &child{cmd: cmd, done: make(chan error, 1)}
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
		_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-c.done:
		case <-time.After(grace):
			_ = syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
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
		c, err := startChild(r.SSHD, []string{"-D", "-e", "-f", r.Paths.SSHDConfig}, r.Stdout, r.Stderr)
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

func (r Runtime) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	sshd, localPort, err := r.startSSHD(runCtx)
	if err != nil {
		return err
	}
	defer sshd.stop(5 * time.Second)
	r.Logger.Printf("local callback sshd listening on 127.0.0.1:%d", localPort)

	herdr, err := startDetachedChild(r.Herdr, []string{"--session", "familiar-fleet", "server"}, r.Stdout, r.Stderr)
	if err != nil {
		return fmt.Errorf("start herdr: %w", err)
	}
	defer herdr.stop(10 * time.Second)
	r.Logger.Printf("Herdr session familiar-fleet started")

	tunnelDone := make(chan struct{})
	go func() { defer close(tunnelDone); r.tunnelLoop(runCtx, localPort) }()
	var result error
	select {
	case err := <-herdr.done:
		result = fmt.Errorf("Herdr exited: %w", exitError(err))
	case err := <-sshd.done:
		result = fmt.Errorf("local sshd exited unexpectedly: %w", exitError(err))
	case <-ctx.Done():
	}
	cancel()
	herdr.stop(10 * time.Second)
	sshd.stop(5 * time.Second)
	<-tunnelDone
	return result
}

func (r Runtime) tunnelLoop(ctx context.Context, localPort int) {
	attempt := 0
	for ctx.Err() == nil {
		args := TunnelArgsForPort(r.Paths, r.Enrollment, localPort)
		c, err := startChild(r.SSH, args, r.Stdout, r.Stderr)
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
	// A small positive jitter prevents a fleet-wide reconnect wave while preserving the cap.
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
