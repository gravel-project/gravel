package config

import (
	"errors"
	"strings"
	"testing"
)

func TestNotify(t *testing.T) {
	files := map[string]string{"/run/secrets/hook": "signing-key\n", "/run/secrets/discord": "https://discord.com/api/webhooks/1/tok\n", "/run/secrets/smtp": "pw\n"}
	env := Env{
		LookupEnv: func(string) (string, bool) { return "", false },
		ReadFile: func(p string) ([]byte, error) {
			if v, ok := files[p]; ok {
				return []byte(v), nil
			}
			return nil, errors.New("no such file")
		},
	}
	doc := minimal + `notify:
  secrets:
    ops-hook: {file: /run/secrets/hook}
    discord: {file: /run/secrets/discord}
  smtp:
    host: smtp.example.com
    username: hub
    password_file: /run/secrets/smtp
    from: hub@example.com
`
	cfg, err := Parse(strings.NewReader(doc), env)
	if err != nil {
		t.Fatal(err)
	}
	n := cfg.Notify
	if n.Secret("ops-hook") != "signing-key" || n.Secret("discord") != "https://discord.com/api/webhooks/1/tok" || n.Secret("nobody") != "" {
		t.Errorf("secrets: %q %q", n.Secret("ops-hook"), n.Secret("discord"))
	}
	if !n.SMTP.Enabled() || n.SMTP.Password != "pw" || n.SMTP.PasswordSource() != "file /run/secrets/smtp" || n.SMTP.TLSMode() != SMTPStartTLS || n.SMTP.PortOrDefault() != 587 {
		t.Errorf("smtp: %+v", n.SMTP)
	}
	if (SMTP{TLS: SMTPImplicitTLS}).PortOrDefault() != 465 || (SMTP{Port: 2525}).PortOrDefault() != 2525 {
		t.Error("ports")
	}
	env.LookupEnv = func(k string) (string, bool) { return "env-pw", k == EnvSMTPPassword }
	if cfg, err := Parse(strings.NewReader(doc), env); err != nil || cfg.Notify.SMTP.Password != "env-pw" {
		t.Errorf("the environment wins: %v", err)
	}
	if cfg, err := Parse(strings.NewReader(minimal), noEnv()); err != nil || cfg.Notify.SMTP.Enabled() || len(cfg.Notify.Secrets) != 0 {
		t.Errorf("none: %v", err)
	}
	if _, err := Parse(strings.NewReader(minimal+"notify:\n  secrets:\n    x: {file: /nope}\n"), noEnv()); err == nil || !strings.Contains(err.Error(), "notify.secrets.x.file") {
		t.Errorf("an unreadable secret: %v", err)
	}
	env.LookupEnv = func(string) (string, bool) { return "", false }
	for name, c := range map[string]struct{ doc, want string }{
		"name":                  {"notify:\n  secrets:\n    Ops Hook: {file: /run/secrets/hook}\n", `notify.secrets: "Ops Hook" is not a notifier name`},
		"no file":               {"notify:\n  secrets:\n    hook: {}\n", "notify.secrets.hook.file: required"},
		"no host":               {"notify:\n  smtp:\n    from: a@b.c\n", "notify.smtp.host: required"},
		"from":                  {"notify:\n  smtp:\n    host: h\n    from: Hub <a@b.c>\n", `notify.smtp.from: "Hub <a@b.c>" is not a plain email address`},
		"clear text":            {"notify:\n  smtp:\n    host: smtp.example.com\n    from: a@b.c\n    tls: none\n", "notify.smtp.tls: none sends the mail in the clear"},
		"password in the clear": {"notify:\n  smtp:\n    host: 127.0.0.1\n    from: a@b.c\n    tls: none\n    username: u\n    password: p\n", "notify.smtp.username: tls: none sends no password"},
		"tls":                   {"notify:\n  smtp:\n    host: h\n    from: a@b.c\n    tls: maybe\n", `notify.smtp.tls: "maybe" is not`},
		"no password":           {"notify:\n  smtp:\n    host: h\n    from: a@b.c\n    username: u\n", "notify.smtp: a username needs a password"},
		"port":                  {"notify:\n  smtp:\n    host: h\n    from: a@b.c\n    port: 70000\n", "notify.smtp.port: 70000 is not a port"},
	} {
		if _, err := Parse(strings.NewReader(minimal+c.doc), env); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", name, c.want, err)
		}
	}
	if _, err := Parse(strings.NewReader(minimal+"notify:\n  smtp:\n    host: 127.0.0.1\n    from: a@b.c\n    tls: none\n"), noEnv()); err != nil {
		t.Errorf("a relay on the host in the clear: %v", err)
	}
}
