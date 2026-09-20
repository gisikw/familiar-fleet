package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// HerdrSessionName is the single named session this client owns. Every status,
// stop, and attach call is scoped to it by name, so unrelated Herdr sessions on
// the same machine are never inspected or terminated.
const HerdrSessionName = herdrSession

// HerdrServerStatus is the subset of `herdr status server --json` this client
// depends on. Herdr's native affordance is used as-is; Herdr is never patched.
type HerdrServerStatus struct {
	Status     string `json:"status"`
	Running    bool   `json:"running"`
	Version    string `json:"version"`
	Socket     string `json:"socket"`
	Session    string `json:"session"`
	Compatible *bool  `json:"compatible"`
}

// Healthy reports a server this client may reuse: running and, when Herdr
// expresses a protocol-compatibility verdict, compatible with the pinned CLI.
func (s HerdrServerStatus) Healthy() bool {
	return s.Running && (s.Compatible == nil || *s.Compatible)
}

// QueryHerdrServer asks Herdr about the named session. A non-zero exit or
// unparseable output is reported as "not running" rather than an error,
// matching Herdr's own behavior when no socket exists.
func QueryHerdrServer(ctx context.Context, herdr string, env []string) (HerdrServerStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, herdr, "--session", HerdrSessionName, "status", "server", "--json")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return HerdrServerStatus{}, nil
	}
	var status HerdrServerStatus
	if err := json.Unmarshal(out, &status); err != nil {
		return HerdrServerStatus{}, fmt.Errorf("parse `herdr status server --json`: %w", err)
	}
	return status, nil
}

// StopHerdrSession terminates the named Herdr server and session. It is
// idempotent syntactic sugar over Herdr's own `server stop`: a session that is
// already gone is a success, not an error. It never touches any other session.
//
// It deliberately requires no enrollment, runtime, or Tiamat preflight, so it
// remains a working recovery path when configuration is broken.
func StopHerdrSession(ctx context.Context, herdr string, env []string, out io.Writer) error {
	status, err := QueryHerdrServer(ctx, herdr, env)
	if err != nil {
		return err
	}
	if !status.Running {
		fmt.Fprintf(out, "Herdr session %s is not running\n", HerdrSessionName)
		return nil
	}
	stopCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(stopCtx, herdr, HerdrStopArgs()...)
	cmd.Env = env
	stopOutput, stopErr := cmd.CombinedOutput()

	// Trust observed state over the exit status: Herdr exits non-zero when the
	// socket has already gone away, which is exactly the race a repeated stop
	// hits and is not a failure for an idempotent command.
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, err := QueryHerdrServer(ctx, herdr, env)
		if err == nil && !status.Running {
			fmt.Fprintf(out, "stopped Herdr session %s\n", HerdrSessionName)
			return nil
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			if stopErr != nil {
				return fmt.Errorf("stop Herdr session %s: %w: %s", HerdrSessionName, stopErr, strings.TrimSpace(string(stopOutput)))
			}
			return fmt.Errorf("Herdr session %s did not stop", HerdrSessionName)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
