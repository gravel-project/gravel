package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestProgram(t *testing.T) {
	p := program()
	if p.Name != "gravel-bot" || p.DefaultConfig != "/etc/gravel/bot.yaml" || p.ConfigEnv != "GRAVEL_BOT_CONFIG" || p.Modules != nil {
		t.Errorf("program: %+v", p)
	}
	var out, errOut bytes.Buffer
	if code := p.Run(context.Background(), []string{"version"}, &out, &errOut); code != 0 || !strings.HasPrefix(out.String(), "gravel-bot ") {
		t.Errorf("version: %d %q", code, out.String())
	}
}
