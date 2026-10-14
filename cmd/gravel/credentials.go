package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// The credential on this machine (ADR-0031 §4): one per hub, in ~/.config/gravel/credentials,
// mode 0600. A session holds the hub's session cookie; an app holds its client id and the path of
// the file its secret is in, never the secret. The browser login (and `gravel mcp`, #163) will
// share the store.

const (
	credentialSession = "session"
	credentialApp     = "app"
)

// credential is how this machine calls one hub.
type credential struct {
	HubURL string `json:"hub_url"`
	Kind   string `json:"kind"`
	// Cookie is the session cookie as the Cookie header carries it: name=value.
	Cookie     string `json:"cookie,omitempty"`
	ClientID   string `json:"client_id,omitempty"`
	SecretFile string `json:"secret_file,omitempty"`
}

// credentials is the file: every hub this machine logged in to, and the one a command uses when
// neither --hub nor GRAVEL_HUB names one (the last logged in to).
type credentials struct {
	Current     string       `json:"current,omitempty"`
	Credentials []credential `json:"credentials"`
}

// credentialsPath is the store; GRAVEL_CREDENTIALS replaces it (tests, a second identity).
func credentialsPath() (string, error) {
	if p := os.Getenv("GRAVEL_CREDENTIALS"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("credentials: %w", err)
	}
	return filepath.Join(dir, "gravel", "credentials"), nil
}

// loadCredentials reads the store; a missing file is an empty one. A file others can read is
// refused: a session cookie in it is as good as the password.
func loadCredentials(path string) (credentials, error) {
	var c credentials
	fi, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("credentials: %w", err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return c, fmt.Errorf("credentials file %s is readable by others (mode %04o); chmod 600 it, and log in again if others could have read it", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("credentials: %w", err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("credentials file %s: %w", path, err)
	}
	return c, nil
}

// save writes the store with mode 0600 through a temporary file, so a crash never leaves half a
// file or a moment where it is readable.
func (c credentials) save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("credentials: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("credentials: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("credentials: %w", err)
	}
	return nil
}

// find returns the credential for a hub.
func (c credentials) find(hub string) (credential, bool) {
	for _, cr := range c.Credentials {
		if cr.HubURL == hub {
			return cr, true
		}
	}
	return credential{}, false
}

// put replaces the hub's credential, or adds it, and makes it the current hub.
func (c *credentials) put(cr credential) {
	c.remove(cr.HubURL)
	c.Credentials = append(c.Credentials, cr)
	c.Current = cr.HubURL
}

// remove forgets a hub's credential; the current hub moves to the newest one left.
func (c *credentials) remove(hub string) {
	kept := c.Credentials[:0]
	for _, cr := range c.Credentials {
		if cr.HubURL != hub {
			kept = append(kept, cr)
		}
	}
	c.Credentials = kept
	if c.Current == hub {
		c.Current = ""
		if n := len(kept); n > 0 {
			c.Current = kept[n-1].HubURL
		}
	}
}

// normalizeHub turns what an operator typed into the hub's origin: https://hub.example.org. A
// bare host is https.
func normalizeHub(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("no hub: pass --hub https://… or set GRAVEL_HUB")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("hub %q is not an http(s) address", s)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", fmt.Errorf("hub %q: give the hub's origin only, like https://hub.example.org", s)
	}
	return u.Scheme + "://" + strings.ToLower(u.Host), nil
}
