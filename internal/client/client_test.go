package client

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testKey(seed byte) string {
	blob := append([]byte{0, 0, 0, 11}, []byte("ssh-ed25519")...)
	blob = append(blob, 0, 0, 0, 32)
	blob = append(blob, make([]byte, 32)...)
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob) + " test-" + string(rune(seed))
}

func validEnrollment() Enrollment {
	return Enrollment{NodeID: "node-1", Host: "mac-1", Port: 22001, TunnelHost: "rendezvous.example.com", TunnelSSHPort: 2222, TunnelUser: "tunnel", RemoteSession: "familiar-fleet", ControllerPublicKey: testKey('c'), TunnelHostKey: testKey('h')}
}

func TestStateRoundTripAndPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := EnsureStateDir(dir); err != nil {
		t.Fatal(err)
	}
	p, err := StatePaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := State{Endpoint: "https://fleet.example.com", SSHUser: "alice", Enrollment: validEnrollment()}
	if err := SaveState(p.State, s); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p.State)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("state mode = %o", got)
	}
	loaded, err := LoadState(p.State)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Enrollment.NodeID != "node-1" {
		t.Fatalf("bad state: %#v", loaded)
	}
	if err := os.Chmod(p.State, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(p.State); err == nil {
		t.Fatal("accepted insecure state permissions")
	}
}

func TestEnrollmentAPI(t *testing.T) {
	var got EnrollmentRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/base/fleet" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization not set")
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content type not set")
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(validEnrollment())
	}))
	defer server.Close()
	e := Enroller{Endpoint: server.URL + "/base", Token: "secret"}
	result, err := e.Enroll(context.Background(), EnrollmentRequest{Host: "work-mac", TunnelPublicKey: testKey('t'), SSHHostPublicKey: testKey('s'), SSHUser: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Host != "work-mac" || result.Port != 22001 {
		t.Fatalf("request=%#v result=%#v", got, result)
	}
}

func TestEnrollmentRejectsBadResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"node_id":"x","unknown":true}`)
	}))
	defer server.Close()
	request := EnrollmentRequest{Host: "work-mac", TunnelPublicKey: testKey('t'), SSHHostPublicKey: testKey('s'), SSHUser: "alice"}
	_, err := (Enroller{Endpoint: server.URL, Token: "x"}).Enroll(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRuntimeConfigIsLockedDown(t *testing.T) {
	p, err := StatePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := validEnrollment()
	if err := WriteRuntimeConfig(p, e, "alice", "/opt/herdr bin/herdr", 49152); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p.SSHDConfig)
	if err != nil {
		t.Fatal(err)
	}
	config := string(b)
	for _, want := range []string{"ListenAddress 127.0.0.1", "AuthenticationMethods publickey", "PasswordAuthentication no", "AllowTcpForwarding no", "PermitTTY no", "AllowUsers alice", "Port 49152", "ForceCommand " + sshdQuote(p.SSHBridge)} {
		if !strings.Contains(config, want) {
			t.Errorf("config missing %q", want)
		}
	}
	auth, _ := os.ReadFile(p.AuthorizedKeys)
	if !strings.HasPrefix(string(auth), "no-port-forwarding,no-agent-forwarding,no-X11-forwarding,no-pty ") {
		t.Errorf("unsafe authorized_keys: %s", auth)
	}
	wrapper, _ := os.ReadFile(p.HerdrWrapper)
	if !strings.Contains(string(wrapper), "exec '/opt/herdr bin/herdr'") {
		t.Errorf("wrapper does not pin binary: %s", wrapper)
	}
	bridge, _ := os.ReadFile(p.SSHBridge)
	if !strings.Contains(string(bridge), "PATH='"+p.Dir+"':${PATH:-/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin}") ||
		!strings.Contains(string(bridge), `exec /bin/sh -c "$SSH_ORIGINAL_COMMAND"`) {
		t.Errorf("SSH bridge does not force the pinned PATH: %s", bridge)
	}
}

func TestSSHDParsesGeneratedConfig(t *testing.T) {
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		t.Skip("sshd is not installed")
	}
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen is not installed")
	}
	current, err := user.Current()
	if err != nil || !userRE.MatchString(current.Username) {
		t.Skip("current username is not supported by the callback server")
	}
	p, _ := StatePaths(t.TempDir())
	if _, err := EnsureKey(context.Background(), keygen, p.HostKey, "test-host"); err != nil {
		t.Fatal(err)
	}
	if err := WriteRuntimeConfig(p, validEnrollment(), current.Username, "/bin/true", 49152); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(sshd, "-t", "-f", p.SSHDConfig).CombinedOutput(); err != nil {
		t.Fatalf("sshd rejected generated config: %v: %s", err, output)
	}
}

func TestUnprivilegedSSHDStartsOnLoopback(t *testing.T) {
	if os.Getenv("CI") != "" && runtime.GOOS == "darwin" {
		// GitHub's macOS image may forbid test sshd processes independently of config validity.
		t.Skip("sshd launch is environment-sensitive on hosted macOS")
	}
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		t.Skip("sshd is not installed")
	}
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen is not installed")
	}
	current, err := user.Current()
	if err != nil || !userRE.MatchString(current.Username) {
		t.Skip("current username is not supported by the callback server")
	}
	p, _ := StatePaths(t.TempDir())
	if _, err := EnsureKey(context.Background(), keygen, p.HostKey, "test-host"); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	r := Runtime{Paths: p, Enrollment: validEnrollment(), LocalUser: current.Username, Herdr: "/bin/true", SSHD: sshd, Stdout: &output, Stderr: &output}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	child, port, err := r.startSSHD(ctx)
	if err != nil {
		t.Fatalf("start unprivileged sshd: %v\n%s", err, output.String())
	}
	if port < 1024 {
		t.Fatalf("sshd selected privileged port %d", port)
	}
	child.stop(time.Second)
}

func TestTunnelArguments(t *testing.T) {
	p, _ := StatePaths(t.TempDir())
	args := strings.Join(TunnelArgsForPort(p, validEnrollment(), 45678), " ")
	for _, want := range []string{"-F " + os.DevNull, "StrictHostKeyChecking=yes", "ExitOnForwardFailure=yes", "IdentitiesOnly=yes", "127.0.0.1:22001:127.0.0.1:45678", "tunnel@rendezvous.example.com"} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}
	if strings.Contains(args, "StrictHostKeyChecking=no") {
		t.Fatal("host checking disabled")
	}
}

func TestValidation(t *testing.T) {
	e := validEnrollment()
	cases := []func(*Enrollment){
		func(e *Enrollment) { e.Port = 0 },
		func(e *Enrollment) { e.TunnelHost = "bad host" },
		func(e *Enrollment) { e.TunnelUser = "-oProxyCommand=bad" },
		func(e *Enrollment) { e.ControllerPublicKey = "not a key" },
	}
	for i, mutate := range cases {
		bad := e
		mutate(&bad)
		if err := ValidateEnrollment(bad); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	if err := ValidateEndpoint("http://example.com"); err == nil {
		t.Fatal("accepted plaintext remote endpoint")
	}
}

func TestBackoffBounded(t *testing.T) {
	for i := 0; i < 20; i++ {
		d := BackoffForTest(i)
		if d < time.Second || d > 30*time.Second {
			t.Fatalf("attempt %d: %s", i, d)
		}
	}
}

func TestMainPlatform(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("client supports macOS and Linux")
	}
}
