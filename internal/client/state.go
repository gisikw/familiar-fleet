package client

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

const stateVersion = 1

var (
	labelRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)
	nodeIDRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)
	hostPartRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	userRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]{0,63}$`)
	sessionRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

type Paths struct {
	Dir, State, TunnelKey, TunnelPublicKey, HostKey, HostPublicKey           string
	AuthorizedKeys, KnownHosts, SSHDConfig, SSHDPid, HerdrWrapper, SSHBridge string
}

func StatePaths(override string) (Paths, error) {
	dir := override
	if dir == "" {
		base := os.Getenv("XDG_STATE_HOME")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return Paths{}, fmt.Errorf("find home directory: %w", err)
			}
			base = filepath.Join(home, ".local", "state")
		}
		dir = filepath.Join(base, "familiar-fleet")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Paths{}, err
	}
	if strings.ContainsRune(abs, ':') || strings.IndexFunc(abs, unicode.IsControl) >= 0 {
		return Paths{}, errors.New("state directory must not contain a colon or control character")
	}
	return Paths{
		Dir: abs, State: filepath.Join(abs, "state.json"),
		TunnelKey: filepath.Join(abs, "tunnel_ed25519"), TunnelPublicKey: filepath.Join(abs, "tunnel_ed25519.pub"),
		HostKey: filepath.Join(abs, "sshd_host_ed25519"), HostPublicKey: filepath.Join(abs, "sshd_host_ed25519.pub"),
		AuthorizedKeys: filepath.Join(abs, "authorized_keys"), KnownHosts: filepath.Join(abs, "known_hosts"),
		SSHDConfig: filepath.Join(abs, "sshd_config"), SSHDPid: filepath.Join(abs, "sshd.pid"),
		HerdrWrapper: filepath.Join(abs, "herdr"), SSHBridge: filepath.Join(abs, "ssh-bridge"),
	}, nil
}

type EnrollmentRequest struct {
	Host             string `json:"host"`
	TunnelPublicKey  string `json:"tunnel_public_key"`
	SSHHostPublicKey string `json:"ssh_host_public_key"`
	SSHUser          string `json:"ssh_user"`
}

type Enrollment struct {
	NodeID              string `json:"node_id"`
	Host                string `json:"host"`
	Port                int    `json:"port"`
	TunnelHost          string `json:"tunnel_host"`
	TunnelSSHPort       int    `json:"tunnel_ssh_port"`
	TunnelUser          string `json:"tunnel_user"`
	RemoteSession       string `json:"remote_session"`
	ControllerPublicKey string `json:"controller_public_key"`
	// Optional contract extension. If omitted, the same value must be supplied out of band.
	TunnelHostKey string `json:"tunnel_host_key,omitempty"`
}

type State struct {
	Version    int        `json:"version"`
	Endpoint   string     `json:"endpoint"`
	SSHUser    string     `json:"ssh_user"`
	Enrollment Enrollment `json:"enrollment"`
}

func EnsureStateDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("state path is not a real directory")
	}
	if info.Mode().Perm()&0077 != 0 {
		if err := os.Chmod(dir, 0700); err != nil {
			return fmt.Errorf("secure state directory: %w", err)
		}
	}
	return nil
}

func LoadState(path string) (*State, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("%s must be a regular file accessible only by its owner", path)
	}
	if info.Size() > 1<<20 {
		return nil, errors.New("state file exceeds 1 MiB")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s State
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("decode state: trailing JSON data")
	}
	if s.Version != stateVersion {
		return nil, fmt.Errorf("unsupported state version %d", s.Version)
	}
	if err := ValidateEndpoint(s.Endpoint); err != nil {
		return nil, fmt.Errorf("invalid saved endpoint: %w", err)
	}
	if !userRE.MatchString(s.SSHUser) {
		return nil, errors.New("invalid saved SSH user")
	}
	if err := ValidateEnrollment(s.Enrollment); err != nil {
		return nil, fmt.Errorf("invalid saved enrollment: %w", err)
	}
	return &s, nil
}

func SaveState(path string, s State) error {
	s.Version = stateVersion
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return atomicWrite(path, b, 0600)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

func ValidateEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("endpoint must be an absolute URL without credentials, query, or fragment")
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if u.Scheme == "http" && (host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()) {
		return nil
	}
	return errors.New("endpoint must use HTTPS (HTTP is allowed only for loopback testing)")
}

func ValidateEnrollmentRequest(r EnrollmentRequest) error {
	if !labelRE.MatchString(r.Host) {
		return errors.New("invalid requested host label (use --name with letters, digits, dot, underscore, or hyphen)")
	}
	if !userRE.MatchString(r.SSHUser) {
		return errors.New("invalid local SSH user")
	}
	if err := validatePublicKey(r.TunnelPublicKey); err != nil {
		return fmt.Errorf("tunnel public key: %w", err)
	}
	if err := validatePublicKey(r.SSHHostPublicKey); err != nil {
		return fmt.Errorf("SSH host public key: %w", err)
	}
	return nil
}

func ValidateEnrollment(e Enrollment) error {
	if !nodeIDRE.MatchString(e.NodeID) {
		return errors.New("invalid node_id")
	}
	if !labelRE.MatchString(e.Host) {
		return errors.New("invalid host label")
	}
	if e.Port < 1 || e.Port > 65535 {
		return errors.New("invalid reverse port")
	}
	if !validHost(e.TunnelHost) {
		return errors.New("invalid tunnel host")
	}
	if e.TunnelSSHPort < 1 || e.TunnelSSHPort > 65535 {
		return errors.New("invalid tunnel SSH port")
	}
	if !userRE.MatchString(e.TunnelUser) {
		return errors.New("invalid tunnel user")
	}
	if !sessionRE.MatchString(e.RemoteSession) || e.RemoteSession != "familiar-fleet" {
		return errors.New("remote session must be familiar-fleet")
	}
	if err := validatePublicKey(e.ControllerPublicKey); err != nil {
		return fmt.Errorf("controller public key: %w", err)
	}
	if e.TunnelHostKey != "" {
		if err := validatePublicKey(e.TunnelHostKey); err != nil {
			return fmt.Errorf("tunnel host key: %w", err)
		}
	}
	return nil
}

func validHost(s string) bool {
	if net.ParseIP(s) != nil {
		return true
	}
	if len(s) > 253 || strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if !hostPartRE.MatchString(part) {
			return false
		}
	}
	return true
}

func validatePublicKey(s string) error {
	fields := strings.Fields(s)
	if len(fields) < 2 || len(fields) > 3 || fields[0] != "ssh-ed25519" {
		return errors.New("expected one ssh-ed25519 OpenSSH public key")
	}
	if strings.ContainsAny(s, "\r\n") {
		return errors.New("key contains a newline")
	}
	decoded, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil || len(decoded) != 4+11+4+32 {
		return errors.New("invalid Ed25519 key encoding")
	}
	nameLen := int(binary.BigEndian.Uint32(decoded[:4]))
	if nameLen != 11 || string(decoded[4:15]) != "ssh-ed25519" || binary.BigEndian.Uint32(decoded[15:19]) != 32 {
		return errors.New("public key blob does not match ssh-ed25519")
	}
	return nil
}

func NormalizePublicKey(s string) (string, error) {
	s = strings.TrimSpace(s)
	if err := validatePublicKey(s); err != nil {
		return "", err
	}
	f := strings.Fields(s)
	return f[0] + " " + f[1], nil
}
