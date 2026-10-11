package cloudvm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// ParseSSHKey reads the hub's private key (OpenSSH format). It must be ed25519: the public line
// the hosts authorize is then one short, stable line.
func ParseSSHKey(pem []byte) (ssh.Signer, error) {
	s, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		return nil, fmt.Errorf("cloudvm: ssh key: %w", err)
	}
	if t := s.PublicKey().Type(); t != ssh.KeyAlgoED25519 {
		return nil, fmt.Errorf("cloudvm: ssh key: %s, want ed25519 (ssh-keygen -t ed25519)", t)
	}
	return s, nil
}

// SSH is the Remote the hub uses: golang.org/x/crypto/ssh, with the host's key trusted on first
// use and pinned after (the hub keeps it in the host's row).
type SSH struct {
	// DialTimeout bounds connecting; 10 s by default.
	DialTimeout time.Duration
}

// Run implements Remote.
func (r SSH) Run(ctx context.Context, t RemoteTarget, stdin io.Reader, command string) ([]byte, string, error) {
	if t.Signer == nil {
		return nil, "", errors.New("cloudvm: no ssh key")
	}
	dialTimeout := r.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = 10 * time.Second
	}
	var seen string
	var pinned ssh.PublicKey
	if t.HostKey != "" {
		k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(t.HostKey))
		if err != nil {
			return nil, "", fmt.Errorf("cloudvm: the pinned host key: %w", err)
		}
		pinned = k
	}
	cfg := &ssh.ClientConfig{
		User:              t.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(t.Signer)},
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		Timeout:           dialTimeout,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			seen = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
			if pinned != nil && !bytes.Equal(pinned.Marshal(), key.Marshal()) {
				return ErrHostKeyChanged
			}
			return nil
		},
	}
	port := "22"
	if t.Port > 0 {
		port = strconv.Itoa(t.Port)
	}
	addr := net.JoinHostPort(t.Address, port)
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, "", err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		if errors.Is(err, ErrHostKeyChanged) {
			return nil, seen, ErrHostKeyChanged
		}
		return nil, seen, err
	}
	client := ssh.NewClient(c, chans, reqs)
	defer func() { _ = client.Close() }()
	session, err := client.NewSession()
	if err != nil {
		return nil, seen, err
	}
	defer func() { _ = session.Close() }()
	session.Stdin = stdin
	out, err := session.CombinedOutput(command)
	return out, seen, err
}
