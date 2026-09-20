package client

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a concurrency-safe sink. Production writes child output and
// supervisor logs to the same *os.File, whose writes are independently
// serialized; a test buffer needs the lock made explicit.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// fakeHerdr writes a stand-in for the Herdr CLI that models the native
// affordances this client depends on: a named session whose server keeps
// running until it is explicitly stopped, `status server --json`, and
// `server stop`. It refuses to start a second server for a live session,
// exactly as real Herdr does, so a test that forgets to reuse fails loudly.
//
// $HERDR_FAKE_STATE/<session>.running is the server's liveness marker and
// stands in for the session and its agents.
func fakeHerdrCLI(t *testing.T) (path string, stateDir string) {
	t.Helper()
	dir := t.TempDir()
	stateDir = filepath.Join(dir, "sessions")
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
session=""
if [ "$1" = "--session" ]; then session="$2"; shift 2; fi
marker="$HERDR_FAKE_STATE/$session.running"
case "$1" in
  status)
    if [ -f "$marker" ]; then
      printf '{"status":"running","running":true,"version":"9.9.9","compatible":true,"socket":"%s","session":"%s"}\n' "$marker" "$session"
    else
      printf '{"status":"not_running","running":false,"version":null,"compatible":null,"socket":"%s","session":"%s"}\n' "$marker" "$session"
    fi
    exit 0
    ;;
  server)
    if [ "$2" = "stop" ]; then
      if [ -f "$marker" ]; then rm -f "$marker"; exit 0; fi
      echo "server is not running" >&2
      exit 1
    fi
    if [ -f "$marker" ]; then echo "error: herdr server is already running" >&2; exit 1; fi
    echo started > "$marker"
    # The server outlives the foreground TUI until it is stopped.
    while [ -f "$marker" ]; do sleep 0.05; done
    exit 0
    ;;
  config) exit 0 ;;
  "")
    # The visible TUI: attaches, then the user leaves it.
    echo "$HERDR_FAKE_TUI" > "$HERDR_FAKE_STATE/tui.attached"
    exit 0
    ;;
esac
exit 0
`
	path = filepath.Join(dir, "herdr")
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return path, stateDir
}

func sessionRuntime(t *testing.T, herdr, stateDir string, out *syncBuffer) Runtime {
	t.Helper()
	paths, err := StatePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return Runtime{
		Paths: paths, Enrollment: validEnrollment(), LocalUser: "tester",
		Herdr: herdr, HerdrEnv: append(os.Environ(), "HERDR_FAKE_STATE="+stateDir),
		Stdin: strings.NewReader(""), Stdout: out, Stderr: out,
		TUIOut: out, TUIErr: out,
		Logger: log.New(out, "test: ", 0),
	}
}

func serverRunning(stateDir string) bool {
	_, err := os.Stat(filepath.Join(stateDir, HerdrSessionName+".running"))
	return err == nil
}

func TestEnsureHerdrServerStartsThenReusesTheNamedSession(t *testing.T) {
	herdr, stateDir := fakeHerdrCLI(t)
	var out syncBuffer
	r := sessionRuntime(t, herdr, stateDir, &out)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Fresh start: nothing is running, so this invocation spawns the server.
	first, err := r.ensureHerdrServer(ctx)
	if err != nil {
		t.Fatalf("fresh start: %v\n%s", err, out.String())
	}
	if !first.spawned {
		t.Fatal("fresh start did not spawn a server")
	}
	if !serverRunning(stateDir) {
		t.Fatal("server is not running after a fresh start")
	}

	// Reuse: a second invocation must detect the healthy server instead of
	// spawning a conflicting one (the fake, like Herdr, would fail that).
	second, err := r.ensureHerdrServer(ctx)
	if err != nil {
		t.Fatalf("reuse: %v\n%s", err, out.String())
	}
	if second.spawned {
		t.Fatal("second invocation spawned a conflicting server")
	}
	if second.child != nil {
		t.Fatal("reused server must not be owned as a child")
	}
	if !strings.Contains(out.String(), "reusing running Herdr session") {
		t.Errorf("reuse was not logged:\n%s", out.String())
	}

	if err := StopHerdrSession(ctx, herdr, r.HerdrEnv, &out); err != nil {
		t.Fatal(err)
	}
	first.child.stop(5 * time.Second)
}

// Requirement: leaving the visible TUI must leave the named server, its
// session, and its agents alive so work survives and a later invocation can
// reattach.
func TestTUIExitLeavesTheHerdrSessionRunning(t *testing.T) {
	herdr, stateDir := fakeHerdrCLI(t)
	var out syncBuffer
	r := sessionRuntime(t, herdr, stateDir, &out)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	server, err := r.ensureHerdrServer(ctx)
	if err != nil {
		t.Fatalf("start: %v\n%s", err, out.String())
	}
	defer func() {
		_ = StopHerdrSession(context.Background(), herdr, r.HerdrEnv, &out)
		server.child.stop(5 * time.Second)
	}()

	// The visible TUI runs and the user leaves it.
	tui, err := startForegroundChild(r.Herdr, HerdrTUIArgs(), r.Stdin, r.TUIOut, r.TUIErr)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-tui.done:
	case <-ctx.Done():
		t.Fatal("TUI did not exit")
	}

	// The session survives the TUI: this is the agent-survival assumption.
	if !serverRunning(stateDir) {
		t.Fatalf("leaving the TUI killed the Herdr session\n%s", out.String())
	}

	// And a later invocation reattaches to it rather than starting a new one.
	again, err := r.ensureHerdrServer(ctx)
	if err != nil {
		t.Fatalf("reattach: %v\n%s", err, out.String())
	}
	if again.spawned {
		t.Fatal("reattach spawned a second server")
	}
}

func TestStopHerdrSessionIsIdempotent(t *testing.T) {
	herdr, stateDir := fakeHerdrCLI(t)
	var out syncBuffer
	r := sessionRuntime(t, herdr, stateDir, &out)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Stopping when nothing runs is a success, not an error.
	var quiet syncBuffer
	if err := StopHerdrSession(ctx, herdr, r.HerdrEnv, &quiet); err != nil {
		t.Fatalf("stop with no server: %v", err)
	}
	if !strings.Contains(quiet.String(), "is not running") {
		t.Errorf("unexpected output: %s", quiet.String())
	}

	server, err := r.ensureHerdrServer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	quiet.Reset()
	if err := StopHerdrSession(ctx, herdr, r.HerdrEnv, &quiet); err != nil {
		t.Fatalf("stop: %v\n%s", err, out.String())
	}
	if serverRunning(stateDir) {
		t.Fatal("stop left the session running")
	}
	if !strings.Contains(quiet.String(), "stopped Herdr session") {
		t.Errorf("unexpected output: %s", quiet.String())
	}
	// Repeating it stays successful.
	if err := StopHerdrSession(ctx, herdr, r.HerdrEnv, &quiet); err != nil {
		t.Fatalf("second stop: %v", err)
	}
	server.child.stop(5 * time.Second)
}

// stop must be scoped to the familiar-fleet session by name so a co-resident
// Herdr session belonging to the user is never terminated.
func TestStopDoesNotTouchUnrelatedSessions(t *testing.T) {
	herdr, stateDir := fakeHerdrCLI(t)
	var out syncBuffer
	r := sessionRuntime(t, herdr, stateDir, &out)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	other := filepath.Join(stateDir, "someone-elses-work.running")
	if err := os.WriteFile(other, []byte("started\n"), 0644); err != nil {
		t.Fatal(err)
	}
	server, err := r.ensureHerdrServer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := StopHerdrSession(ctx, herdr, r.HerdrEnv, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("stop terminated an unrelated Herdr session: %v", err)
	}
	server.child.stop(5 * time.Second)
}

func TestHerdrServerStatusHealthy(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		status HerdrServerStatus
		want   bool
	}{
		{HerdrServerStatus{Running: true, Compatible: &yes}, true},
		{HerdrServerStatus{Running: true, Compatible: nil}, true},
		{HerdrServerStatus{Running: true, Compatible: &no}, false},
		{HerdrServerStatus{Running: false, Compatible: &yes}, false},
		{HerdrServerStatus{}, false},
	}
	for i, tc := range cases {
		if got := tc.status.Healthy(); got != tc.want {
			t.Errorf("case %d: Healthy() = %v", i, got)
		}
	}
}

// An incompatible running server must not be silently reused or killed; the
// operator is told to stop it explicitly.
func TestEnsureHerdrServerRefusesIncompatibleServer(t *testing.T) {
	dir := t.TempDir()
	herdr := filepath.Join(dir, "herdr")
	script := `#!/bin/sh
printf '{"status":"running","running":true,"version":"0.0.1","compatible":false,"socket":"s","session":"familiar-fleet"}\n'
exit 0
`
	if err := os.WriteFile(herdr, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	r := sessionRuntime(t, herdr, dir, &out)
	_, err := r.ensureHerdrServer(context.Background())
	if err == nil || !strings.Contains(err.Error(), "familiar-fleet stop") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// If a child fails to start, an invocation must not leave behind a server that
// nobody asked for -- but it must never terminate one it merely reused.
func TestCleanupStopsOnlyASpawnedServer(t *testing.T) {
	herdr, stateDir := fakeHerdrCLI(t)
	var out syncBuffer
	r := sessionRuntime(t, herdr, stateDir, &out)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	spawned, err := r.ensureHerdrServer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !spawned.spawned {
		t.Fatal("expected to spawn")
	}
	if !serverRunning(stateDir) {
		t.Fatal("server did not start")
	}

	// A server this invocation spawned is cleaned up on a child failure.
	r.cleanup(spawned)
	if serverRunning(stateDir) {
		t.Fatalf("cleanup left an orphaned server\n%s", out.String())
	}

	// Now the reuse case: a server owned by somebody else must survive the
	// identical cleanup, because cleanup is gated on ownership.
	fresh, err := r.ensureHerdrServer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := r.ensureHerdrServer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if reused.spawned || reused.child != nil {
		t.Fatal("second invocation took ownership of a server it reused")
	}
	if reused.done() != nil {
		t.Fatal("reused server must expose no exit channel")
	}
	r.cleanup(reused)
	if !serverRunning(stateDir) {
		t.Fatalf("cleanup terminated a server this invocation only reused\n%s", out.String())
	}

	_ = StopHerdrSession(context.Background(), herdr, r.HerdrEnv, &out)
	fresh.child.stop(5 * time.Second)
}
