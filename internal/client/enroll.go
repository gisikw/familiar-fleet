package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func EnsureKey(ctx context.Context, keygen, privatePath, comment string) (string, error) {
	priv, privErr := os.Lstat(privatePath)
	pubPath := privatePath + ".pub"
	pub, pubErr := os.Lstat(pubPath)
	if privErr == nil || pubErr == nil {
		if privErr != nil || pubErr != nil {
			return "", errors.New("incomplete keypair; refusing to replace existing key material")
		}
		if !priv.Mode().IsRegular() || priv.Mode().Perm()&0077 != 0 {
			return "", errors.New("private key permissions are not owner-only")
		}
		if !pub.Mode().IsRegular() {
			return "", errors.New("public key is not a regular file")
		}
	} else if errors.Is(privErr, os.ErrNotExist) && errors.Is(pubErr, os.ErrNotExist) {
		cmd := exec.CommandContext(ctx, keygen, "-q", "-t", "ed25519", "-N", "", "-C", comment, "-f", privatePath)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("ssh-keygen: %w: %s", err, strings.TrimSpace(string(out)))
		}
		if err := os.Chmod(privatePath, 0600); err != nil {
			return "", err
		}
		if err := os.Chmod(pubPath, 0644); err != nil {
			return "", err
		}
	} else if privErr != nil {
		return "", privErr
	} else {
		return "", pubErr
	}
	b, err := os.ReadFile(pubPath)
	if err != nil {
		return "", err
	}
	return NormalizePublicKey(string(b))
}

type Enroller struct {
	Client   *http.Client
	Endpoint string
	Token    string
}

func (e Enroller) Enroll(ctx context.Context, request EnrollmentRequest) (Enrollment, error) {
	if err := ValidateEndpoint(e.Endpoint); err != nil {
		return Enrollment{}, err
	}
	if err := ValidateEnrollmentRequest(request); err != nil {
		return Enrollment{}, err
	}
	if e.Token == "" {
		return Enrollment{}, errors.New("enrollment requires a bearer token (use --token-file or FAMILIAR_FLEET_TOKEN)")
	}
	body, err := json.Marshal(request)
	if err != nil {
		return Enrollment{}, err
	}
	u, _ := url.Parse(strings.TrimRight(e.Endpoint, "/") + "/fleet")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return Enrollment{}, err
	}
	req.Header.Set("Authorization", "Bearer "+e.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := e.Client
	if client == nil {
		client = &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Enrollment{}, fmt.Errorf("enrollment request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(b))
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return Enrollment{}, fmt.Errorf("enrollment failed (%s): %q", resp.Status, msg)
	}
	const maxResponse = 1 << 20
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return Enrollment{}, fmt.Errorf("read enrollment response: %w", err)
	}
	if len(bodyBytes) > maxResponse {
		return Enrollment{}, errors.New("enrollment response exceeds 1 MiB")
	}
	var result Enrollment
	dec := json.NewDecoder(bytes.NewReader(bodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&result); err != nil {
		return Enrollment{}, fmt.Errorf("decode enrollment response: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Enrollment{}, errors.New("decode enrollment response: trailing JSON data")
	}
	if err := ValidateEnrollment(result); err != nil {
		return Enrollment{}, fmt.Errorf("validate enrollment response: %w", err)
	}
	return result, nil
}

func ReadToken(path string) (string, error) {
	if path == "" {
		token := strings.TrimSpace(os.Getenv("FAMILIAR_FLEET_TOKEN"))
		if strings.ContainsAny(token, "\r\n") {
			return "", errors.New("FAMILIAR_FLEET_TOKEN must not contain a newline")
		}
		return token, nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("token file %s must be a regular owner-only file", path)
	}
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(b))
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return "", errors.New("token file must contain exactly one non-empty line")
	}
	return token, nil
}
