package client

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func unsignedIDToken(nonce string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]string{"nonce": nonce})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestConnectorBrowserOIDCAndEnrollment(t *testing.T) {
	var server *httptest.Server
	var enrollmentPosts atomic.Int32
	var expectedChallenge string
	var expectedRedirect string
	var expectedNonce string
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/base/.well-known/oauth-protected-resource":
			json.NewEncoder(w).Encode(map[string]any{"resource": server.URL + "/base", "authorization_servers": []string{server.URL + "/issuer"}})
		case "/issuer/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(OIDCDiscovery{Issuer: server.URL + "/issuer", AuthorizationEndpoint: server.URL + "/authorize", TokenEndpoint: server.URL + "/token", CodeChallengeMethods: []string{"S256"}})
		case "/token":
			if r.Method != http.MethodPost {
				t.Errorf("token method = %s", r.Method)
			}
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("client_id") != OIDCClientID || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "the-code" || r.Form.Get("redirect_uri") != expectedRedirect {
				t.Errorf("unexpected token form: %v", r.Form)
			}
			digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(digest[:]) != expectedChallenge {
				t.Error("PKCE verifier does not match authorize challenge")
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "access-secret", "refresh_token": "refresh-secret", "id_token": unsignedIDToken(expectedNonce)})
		case "/base/fleet":
			enrollmentPosts.Add(1)
			if got := r.Header.Get("Authorization"); got != "Bearer access-secret" {
				t.Errorf("enrollment authorization = %q", got)
			}
			var request EnrollmentRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if request.Host != "macbook" {
				t.Errorf("host = %q", request.Host)
			}
			json.NewEncoder(w).Encode(validEnrollment())
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	paths, _ := StatePaths(t.TempDir())
	browserDone := make(chan error, 1)
	browser := func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		q := u.Query()
		for key, want := range map[string]string{"response_type": "code", "client_id": OIDCClientID, "scope": OIDCScope, "code_challenge_method": "S256"} {
			if q.Get(key) != want {
				t.Errorf("authorize %s = %q, want %q", key, q.Get(key), want)
			}
		}
		expectedChallenge, expectedRedirect, expectedNonce = q.Get("code_challenge"), q.Get("redirect_uri"), q.Get("nonce")
		if q.Get("state") == "" || expectedNonce == "" || expectedChallenge == "" {
			t.Error("authorize URL omitted state, nonce, or challenge")
		}
		go func() {
			callback := expectedRedirect + "?code=the-code&state=" + url.QueryEscape(q.Get("state"))
			resp, err := http.Get(callback)
			if err != nil {
				browserDone <- err
				return
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if !strings.Contains(string(body), "Connect this machine") || !strings.Contains(string(body), `value="default-host"`) {
				browserDone <- &testError{"missing enrollment form/default in callback page"}
				return
			}
			form := url.Values{"name": {"macbook"}, "state": {q.Get("state")}}
			resp, err = http.PostForm(strings.TrimSuffix(expectedRedirect, "/callback")+"/enroll", form)
			if err != nil {
				browserDone <- err
				return
			}
			body, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			if !strings.Contains(string(body), "Connected") || !strings.Contains(string(body), "you can close this tab") {
				browserDone <- &testError{"missing success page"}
				return
			}
			browserDone <- nil
		}()
		return nil
	}
	request := EnrollmentRequest{TunnelPublicKey: testKey('t'), SSHHostPublicKey: testKey('s'), SSHUser: "alice"}
	connector := Connector{Client: server.Client(), Browser: browser, Listener: listener, Timeout: 5 * time.Second}
	result, err := connector.Connect(context.Background(), server.URL+"/base/", "default-host", request, func(enrollment Enrollment) error {
		return SaveState(paths.State, State{Endpoint: server.URL + "/base", SSHUser: "alice", Enrollment: enrollment})
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-browserDone; err != nil {
		t.Fatal(err)
	}
	if result.NodeID != "node-1" || enrollmentPosts.Load() != 1 {
		t.Fatalf("result=%#v enrollment posts=%d", result, enrollmentPosts.Load())
	}
	persisted, err := os.ReadFile(paths.State)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"access-secret", "refresh-secret", "the-code"} {
		if strings.Contains(string(persisted), secret) {
			t.Fatalf("persisted OAuth credential %q", secret)
		}
	}
}

type testError struct{ message string }

func (e *testError) Error() string { return e.message }

func TestConnectorRejectsStateBeforeTokenExchange(t *testing.T) {
	var server *httptest.Server
	var tokenPosts atomic.Int32
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource":
			json.NewEncoder(w).Encode(map[string]any{"authorization_servers": []string{server.URL}})
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(OIDCDiscovery{Issuer: server.URL, AuthorizationEndpoint: server.URL + "/authorize", TokenEndpoint: server.URL + "/token"})
		case "/token":
			tokenPosts.Add(1)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	listener, _ := net.Listen("tcp4", "127.0.0.1:0")
	browser := func(raw string) error {
		u, _ := url.Parse(raw)
		go func() {
			resp, _ := http.Get(u.Query().Get("redirect_uri") + "?code=x&state=wrong")
			if resp != nil {
				resp.Body.Close()
			}
		}()
		return nil
	}
	request := EnrollmentRequest{TunnelPublicKey: testKey('t'), SSHHostPublicKey: testKey('s'), SSHUser: "alice"}
	_, err := (Connector{Client: server.Client(), Browser: browser, Listener: listener, Timeout: 2 * time.Second}).Connect(context.Background(), server.URL, "macbook", request, nil)
	if err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("unexpected error: %v", err)
	}
	if tokenPosts.Load() != 0 {
		t.Fatal("state mismatch reached token endpoint")
	}
}

func TestDiscoveryAndNonceValidation(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "oauth-protected-resource") {
			json.NewEncoder(w).Encode(map[string]any{"authorization_servers": []string{serverURLFromRequest(r) + "/issuer"}})
			return
		}
		json.NewEncoder(w).Encode(OIDCDiscovery{Issuer: serverURLFromRequest(r) + "/different", AuthorizationEndpoint: serverURLFromRequest(r) + "/auth", TokenEndpoint: serverURLFromRequest(r) + "/token"})
	}))
	defer server.Close()
	if _, err := DiscoverOIDC(context.Background(), server.Client(), server.URL); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("issuer mismatch accepted: %v", err)
	}
	if err := ValidateIDTokenNonce(unsignedIDToken("wrong"), "expected"); err == nil {
		t.Fatal("nonce mismatch accepted")
	}
}

func serverURLFromRequest(r *http.Request) string { return "http://" + r.Host }

func TestMachineNameMatchesRegistryContract(t *testing.T) {
	for _, name := range []string{"macbook", "Kevin-Macbook", "a", strings.Repeat("a", 63)} {
		if err := ValidateMachineName(name); err != nil {
			t.Errorf("valid name %q rejected: %v", name, err)
		}
	}
	for _, name := range []string{"", "-macbook", "macbook-", "mac_book", "mac.book", strings.Repeat("a", 64)} {
		if err := ValidateMachineName(name); err == nil {
			t.Errorf("invalid name %q accepted", name)
		}
	}
}

func TestStateDirectoryPrecedence(t *testing.T) {
	if got, _ := ResolveStateDir("/explicit", 0, "/xdg", "/home/root"); got != "/explicit" {
		t.Fatalf("explicit = %q", got)
	}
	if got, _ := ResolveStateDir("", 0, "/xdg", "/root"); got != "/var/lib/familiar-fleet" {
		t.Fatalf("root = %q", got)
	}
	if got, _ := ResolveStateDir("", 1000, "/xdg", "/home/alice"); got != filepath.Join("/xdg", "familiar-fleet") {
		t.Fatalf("XDG = %q", got)
	}
	if got, _ := ResolveStateDir("", 1000, "", "/home/alice"); got != filepath.Join("/home/alice", ".local", "state", "familiar-fleet") {
		t.Fatalf("home = %q", got)
	}
}

func TestHerdrCommandWiring(t *testing.T) {
	if got := strings.Join(HerdrServerArgs(), " "); got != "--session familiar-fleet server" {
		t.Fatalf("server args = %q", got)
	}
	if got := strings.Join(HerdrTUIArgs(), " "); got != "--session familiar-fleet" {
		t.Fatalf("TUI args = %q", got)
	}
	if got := strings.Join(HerdrStopArgs(), " "); got != "--session familiar-fleet server stop" {
		t.Fatalf("stop args = %q", got)
	}
}
