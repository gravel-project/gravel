package main

import (
	"context"
	"strings"
	"testing"
)

// migrate --check says what an upgrade would apply and what each step costs (gravel#214), and
// exits 3 while anything is pending, so a rollout can gate on it.
func TestMigrateCheck(t *testing.T) {
	st, cfgPath, _, run := freshHub(t)
	if code, out, errOut := run("migrate", "--check", "--config", cfgPath); code != 0 || !strings.HasSuffix(out, ", pending 0\n") {
		t.Fatalf("migrated: exit %d, %q %q", code, out, errOut)
	}
	// The database of a hub one release behind: two migrations to go.
	if err := st.MigrateDownTo(context.Background(), 33); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--check", "--status"} {
		code, out, errOut := run("migrate", flag, "--config", cfgPath)
		want := "schema version 33, latest 35, pending 2\n  00034  deferrable_foreign_keys  instant\n  00035  email_verifications      instant\n"
		if code != 3 || out != want {
			t.Errorf("%s two behind: exit %d, %q %q; want 3 and:\n%s", flag, code, out, errOut, want)
		}
	}
	if code, _, errOut := run("migrate", "--config", cfgPath); code != 0 {
		t.Fatalf("migrate: %d %s", code, errOut)
	}
	if code, out, _ := run("migrate", "--check", "--config", cfgPath); code != 0 || !strings.HasSuffix(out, ", pending 0\n") {
		t.Errorf("after migrate: exit %d, %q", code, out)
	}
}
