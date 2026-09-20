package client

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

const (
	OIDCClientID     = "familiar-desktop"
	OIDCRedirectPort = 17421
	OIDCScope        = "openid profile email"
)

type ProtectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
}

type OIDCDiscovery struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
}

type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
}

type Connector struct {
	Client   *http.Client
	Browser  func(string) error
	Listener net.Listener // test seam; production binds the registered fixed port
	Timeout  time.Duration
}

func restrictedHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (c Connector) httpClient() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return restrictedHTTPClient()
}

func fetchJSON(ctx context.Context, client *http.Client, rawURL string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s returned %s", rawURL, resp.Status)
	}
	const max = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return err
	}
	if len(body) > max {
		return errors.New("discovery response exceeds 1 MiB")
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("decode %s: %w", rawURL, err)
	}
	return nil
}

func DiscoverOIDC(ctx context.Context, client *http.Client, endpoint string) (OIDCDiscovery, error) {
	base, err := CanonicalEndpoint(endpoint)
	if err != nil {
		return OIDCDiscovery{}, err
	}
	if client == nil {
		client = restrictedHTTPClient()
	}
	metadataURL := base + "/.well-known/oauth-protected-resource"
	var resource ProtectedResourceMetadata
	if err := fetchJSON(ctx, client, metadataURL, &resource); err != nil {
		return OIDCDiscovery{}, fmt.Errorf("protected-resource metadata: %w", err)
	}
	if len(resource.AuthorizationServers) == 0 || resource.AuthorizationServers[0] == "" {
		return OIDCDiscovery{}, errors.New("protected-resource metadata advertises no authorization server")
	}
	issuer, err := CanonicalEndpoint(resource.AuthorizationServers[0])
	if err != nil {
		return OIDCDiscovery{}, fmt.Errorf("invalid advertised issuer: %w", err)
	}
	var doc OIDCDiscovery
	if err := fetchJSON(ctx, client, issuer+"/.well-known/openid-configuration", &doc); err != nil {
		return OIDCDiscovery{}, fmt.Errorf("issuer discovery: %w", err)
	}
	discoveredIssuer, err := CanonicalEndpoint(doc.Issuer)
	if err != nil || discoveredIssuer != issuer {
		return OIDCDiscovery{}, errors.New("issuer discovery does not match the advertised issuer")
	}
	for label, value := range map[string]string{"authorization_endpoint": doc.AuthorizationEndpoint, "token_endpoint": doc.TokenEndpoint} {
		if _, err := CanonicalEndpoint(value); err != nil {
			return OIDCDiscovery{}, fmt.Errorf("issuer discovery has invalid %s: %w", label, err)
		}
	}
	if len(doc.CodeChallengeMethods) > 0 {
		supportsS256 := false
		for _, method := range doc.CodeChallengeMethods {
			if method == "S256" {
				supportsS256 = true
				break
			}
		}
		if !supportsS256 {
			return OIDCDiscovery{}, errors.New("authorization server does not advertise S256 PKCE")
		}
	}
	return doc, nil
}

func randomURLString(bytes int) (string, error) {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func NewOAuthParameters() (state, nonce, verifier, challenge string, err error) {
	if state, err = randomURLString(24); err != nil {
		return
	}
	if nonce, err = randomURLString(24); err != nil {
		return
	}
	if verifier, err = randomURLString(32); err != nil {
		return
	}
	digest := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(digest[:])
	return
}

func BuildAuthorizeURL(endpoint, redirectURI, state, nonce, challenge string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", OIDCClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", OIDCScope)
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func ExchangeCode(ctx context.Context, client *http.Client, tokenEndpoint, code, verifier, redirectURI string) (TokenResponse, error) {
	values := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
		"client_id":     {OIDCClientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return TokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return TokenResponse{}, fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return TokenResponse{}, err
	}
	if len(body) > 1<<20 {
		return TokenResponse{}, errors.New("token response exceeds 1 MiB")
	}
	var token TokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return TokenResponse{}, errors.New("token endpoint returned invalid JSON")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return TokenResponse{}, fmt.Errorf("token endpoint returned %s", resp.Status)
	}
	if token.AccessToken == "" {
		return TokenResponse{}, errors.New("token response missing access_token")
	}
	return token, nil
}

func ValidateIDTokenNonce(idToken, expected string) error {
	if idToken == "" {
		return nil // OAuth-only providers may omit an ID token; state remains mandatory.
	}
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return errors.New("invalid id_token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return errors.New("invalid id_token payload")
	}
	var claims struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Nonce == "" {
		return errors.New("id_token missing nonce")
	}
	if claims.Nonce != expected {
		return errors.New("id_token nonce mismatch")
	}
	return nil
}

var pageTemplate = template.Must(template.New("page").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>Familiar Fleet</title><style>
body{font:16px system-ui,sans-serif;max-width:34rem;margin:12vh auto;padding:0 1.5rem;color:#222}label,input,button{display:block}input{box-sizing:border-box;width:100%;padding:.65rem;margin:.5rem 0 1rem}button{padding:.6rem 1rem}small{color:#555}.error{color:#a00}</style></head>
<body>{{if eq .Kind "form"}}<h1>Connect this machine</h1><form method="post" action="/enroll"><input type="hidden" name="state" value="{{.State}}"><label for="name">Machine name</label><input id="name" name="name" required maxlength="63" pattern="[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?" value="{{.Name}}" autofocus><button type="submit">Connect</button></form><small>One DNS label: letters, digits, and interior hyphens.</small>{{else if eq .Kind "success"}}<h1>Connected</h1><p>This machine is connected; you can close this tab.</p>{{else}}<h1>Could not connect</h1><p class="error">{{.Message}}</p>{{end}}</body></html>`))

type pageData struct {
	Kind, Name, Message, State string
}

func renderPage(w http.ResponseWriter, status int, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = pageTemplate.Execute(w, data)
}

type callbackResult struct {
	code string
	err  error
}

type enrollmentResult struct {
	enrollment Enrollment
	err        error
}

// Connect performs one browser authorization and one enrollment request. The
// finalize callback persists durable, non-token state before success is shown.
func (c Connector) Connect(ctx context.Context, endpoint, defaultName string, request EnrollmentRequest, finalize func(Enrollment) error) (Enrollment, error) {
	endpoint, err := CanonicalEndpoint(endpoint)
	if err != nil {
		return Enrollment{}, err
	}
	doc, err := DiscoverOIDC(ctx, c.httpClient(), endpoint)
	if err != nil {
		return Enrollment{}, err
	}
	listener := c.Listener
	if listener == nil {
		listener, err = net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", OIDCRedirectPort))
		if err != nil {
			return Enrollment{}, fmt.Errorf("bind OAuth callback http://127.0.0.1:%d/callback: %w", OIDCRedirectPort, err)
		}
	}
	defer listener.Close()
	address := listener.Addr().(*net.TCPAddr)
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", address.Port)
	state, nonce, verifier, challenge, err := NewOAuthParameters()
	if err != nil {
		return Enrollment{}, fmt.Errorf("generate OAuth parameters: %w", err)
	}
	authorizeURL, err := BuildAuthorizeURL(doc.AuthorizationEndpoint, redirectURI, state, nonce, challenge)
	if err != nil {
		return Enrollment{}, err
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	flowCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	callbackCh := make(chan callbackResult, 1)
	authReady := make(chan error, 1)
	enrollmentCh := make(chan enrollmentResult, 1)
	callbackPageSent := make(chan struct{}, 1)
	responseSent := make(chan struct{}, 1)
	var callbackUsed atomic.Bool
	var formUsed atomic.Bool
	var authComplete atomic.Bool
	var accessToken string

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !callbackUsed.CompareAndSwap(false, true) {
			renderPage(w, http.StatusConflict, pageData{Kind: "error", Message: "This authorization callback was already used."})
			return
		}
		defer func() { callbackPageSent <- struct{}{} }()
		q := r.URL.Query()
		var result callbackResult
		if q.Get("state") != state {
			result.err = errors.New("authorization state mismatch")
		} else if providerErr := q.Get("error"); providerErr != "" {
			result.err = fmt.Errorf("authorization was denied: %s", providerErr)
		} else if q.Get("code") == "" {
			result.err = errors.New("authorization callback omitted the code")
		} else {
			result.code = q.Get("code")
		}
		callbackCh <- result
		if result.err != nil {
			renderPage(w, http.StatusBadRequest, pageData{Kind: "error", Message: result.err.Error()})
			return
		}
		select {
		case readyErr := <-authReady:
			if readyErr != nil {
				renderPage(w, http.StatusBadGateway, pageData{Kind: "error", Message: readyErr.Error()})
				return
			}
			renderPage(w, http.StatusOK, pageData{Kind: "form", Name: defaultName, State: state})
		case <-flowCtx.Done():
			renderPage(w, http.StatusGatewayTimeout, pageData{Kind: "error", Message: "Connection setup timed out."})
		}
	})
	mux.HandleFunc("/enroll", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil {
			renderPage(w, http.StatusBadRequest, pageData{Kind: "error", Message: "Invalid form submission."})
			return
		}
		if !authComplete.Load() || r.Form.Get("state") != state {
			renderPage(w, http.StatusForbidden, pageData{Kind: "error", Message: "Invalid or expired enrollment form."})
			return
		}
		name := strings.TrimSpace(r.Form.Get("name"))
		if err := ValidateMachineName(name); err != nil {
			renderPage(w, http.StatusBadRequest, pageData{Kind: "error", Message: "Invalid machine name: " + err.Error()})
			return
		}
		if !formUsed.CompareAndSwap(false, true) {
			renderPage(w, http.StatusConflict, pageData{Kind: "error", Message: "This enrollment form was already submitted."})
			return
		}
		request.Host = name
		enrollment, enrollErr := (Enroller{Client: c.httpClient(), Endpoint: endpoint, Token: accessToken}).Enroll(flowCtx, request)
		accessToken = ""
		if enrollErr == nil && finalize != nil {
			enrollErr = finalize(enrollment)
		}
		enrollmentCh <- enrollmentResult{enrollment: enrollment, err: enrollErr}
		if enrollErr != nil {
			renderPage(w, http.StatusBadGateway, pageData{Kind: "error", Message: enrollErr.Error()})
		} else {
			renderPage(w, http.StatusOK, pageData{Kind: "success"})
		}
		responseSent <- struct{}{}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	defer server.Close()
	shutdownServer := func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), time.Second)
		_ = server.Shutdown(shutdownCtx)
		shutdownCancel()
	}

	browser := c.Browser
	if browser == nil {
		browser = OpenBrowser
	}
	if err := browser(authorizeURL); err != nil {
		return Enrollment{}, err
	}

	var callback callbackResult
	select {
	case callback = <-callbackCh:
	case <-flowCtx.Done():
		return Enrollment{}, errors.New("authorization timed out")
	case err := <-serveDone:
		return Enrollment{}, fmt.Errorf("OAuth callback server: %w", err)
	}
	if callback.err != nil {
		<-callbackPageSent
		shutdownServer()
		return Enrollment{}, callback.err
	}
	token, err := ExchangeCode(flowCtx, c.httpClient(), doc.TokenEndpoint, callback.code, verifier, redirectURI)
	if err == nil {
		err = ValidateIDTokenNonce(token.IDToken, nonce)
	}
	if err != nil {
		authReady <- err
		<-callbackPageSent
		shutdownServer()
		return Enrollment{}, err
	}
	accessToken = token.AccessToken
	// Refresh and ID tokens intentionally go out of scope without persistence.
	token = TokenResponse{}
	authComplete.Store(true)
	authReady <- nil

	select {
	case result := <-enrollmentCh:
		accessToken = ""
		select {
		case <-responseSent:
		case <-time.After(time.Second):
		}
		// Graceful shutdown lets net/http flush the success/error page before
		// the listener is closed by the return path.
		shutdownServer()
		return result.enrollment, result.err
	case <-flowCtx.Done():
		accessToken = ""
		return Enrollment{}, errors.New("machine-name form timed out")
	}
}

func OpenBrowser(rawURL string) error {
	var command string
	switch runtime.GOOS {
	case "darwin":
		command = "open"
	case "linux":
		command = "xdg-open"
	default:
		return fmt.Errorf("cannot open a browser on %s; open this URL manually: %s", runtime.GOOS, rawURL)
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return fmt.Errorf("cannot find %s to open a browser; open this URL manually: %s", command, rawURL)
	}
	cmd := exec.Command(path, rawURL)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w; open this URL manually: %s", command, err, rawURL)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
