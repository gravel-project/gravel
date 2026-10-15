package main

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/cloud/fake"
)

// decodeJSON reads stdout as exactly one JSON object into v and returns its top-level keys,
// sorted: the shape docs/cli.md promises.
func decodeJSON(t *testing.T, stdout string, v any) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	var raw map[string]json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\n%s", err, stdout)
	}
	if dec.More() {
		t.Fatalf("stdout holds more than one JSON value:\n%s", stdout)
	}
	if err := json.Unmarshal([]byte(stdout), v); err != nil {
		t.Fatal(err)
	}
	return slices.Sorted(maps.Keys(raw))
}

func wantKeys(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s keys %v, want %v (a public surface: docs/cli.md \"Machine-readable output\")", what, got, want)
	}
}

func stepActions(steps []stepJSON) string {
	var parts []string
	for _, s := range steps {
		parts = append(parts, string(s.Kind)+":"+string(s.Action))
	}
	return strings.Join(parts, " ")
}

func TestHubCreateJSON(t *testing.T) {
	e := newEnv(t)
	if code, _, errOut := e.run(e.createArgs("--ssh-from-here", "--output", "yaml")...); code != 2 || !strings.Contains(errOut, "want text or json") {
		t.Errorf("--output yaml: %d %q", code, errOut)
	}

	// A dry run from nothing: exit 3, nothing made, the steps say what would be.
	code, out, errOut := e.run(e.createArgs("--ssh-from-here", "--dry-run", "--output", "json")...)
	var dry createJSON
	keys := decodeJSON(t, out, &dry)
	if code != 3 || !dry.DryRun || !dry.Changed || stepActions(dry.Steps) != "firewall:created volume:created instance:created" || dry.Ready {
		t.Errorf("dry run: %d %+v\n%s", code, dry, errOut)
	}
	for _, s := range dry.Steps {
		if s.ID != "" {
			t.Errorf("a dry run step has an id: %+v", s)
		}
	}
	if s := e.cloud.Counts(); s != (fake.Counts{}) {
		t.Errorf("a dry run made %+v", s)
	}
	wantKeys(t, "create", keys, "hub", "dry_run", "changed", "steps", "firewall", "volume", "instance", "recreated", "ready", "tunnel_installed")

	// The real thing, with a caller's tags: one object on stdout, the progress on stderr.
	code, out, errOut = e.run(e.createArgs("--ssh-from-here", "--tag", "grvl", "--tag", "grvl:org:7", "--output", "json")...)
	var made createJSON
	decodeJSON(t, out, &made)
	if code != 0 || made.Error != "" || !made.Ready || made.Recreated || made.DryRun {
		t.Fatalf("create: %d %+v\n%s", code, made, errOut)
	}
	if got := stepActions(made.Steps); got != "firewall:created volume:created instance:created" {
		t.Errorf("steps %s", got)
	}
	if in := made.Instance; in == nil || in.PublicIPv4 != e.instanceIP() || in.Status != "running" || !slices.Contains(in.Tags, "grvl:org:7") {
		t.Errorf("instance %+v", made.Instance)
	}
	if v := made.Volume; v == nil || v.Name != "hub-1-data" || v.SizeGiB != 20 || v.ID == "" {
		t.Errorf("volume %+v", made.Volume)
	}
	if fw := made.Firewall; fw == nil || !slices.Equal(fw.SSHFrom, []string{"203.0.113.9/32"}) {
		t.Errorf("firewall %+v", made.Firewall)
	}
	if !strings.Contains(errOut, "waiting for") || strings.Contains(out, "waiting for") {
		t.Errorf("progress belongs on stderr:\nstdout %s\nstderr %s", out, errOut)
	}

	// A failure still prints the object, with the error and the steps that ran.
	if _, err := e.cloud.CreateVolume(t.Context(), cloud.VolumeSpec{Name: "hub-2-data", Region: "fake-1", SizeGiB: 1}); err != nil {
		t.Fatal(err)
	}
	args := e.createArgs("--ssh-from-here", "--output", "json")
	args[slices.Index(args, "hub-1")] = "hub-2"
	code, out, errOut = e.run(args...)
	var failed createJSON
	decodeJSON(t, out, &failed)
	if code != 1 || !strings.Contains(failed.Error, "not this hub's") || stepActions(failed.Steps) != "firewall:created" {
		t.Errorf("failure: %d %+v\n%s", code, failed, errOut)
	}
	if !strings.Contains(errOut, "not this hub's") {
		t.Errorf("the error is not on stderr too: %q", errOut)
	}
}

func TestHubStatusJSON(t *testing.T) {
	e := newEnv(t)
	status := func(extra ...string) (int, statusJSON, []string) {
		t.Helper()
		code, out, errOut := e.run(append([]string{"hub", "status", "--provider", "fake", "--name", "hub-1", "--output", "json"}, extra...)...)
		var s statusJSON
		keys := decodeJSON(t, out, &s)
		if errOut != "" {
			t.Errorf("stderr %q", errOut)
		}
		return code, s, keys
	}

	code, none, keys := status()
	if code != 0 || none.Firewall != nil || none.Volume != nil || none.Instance != nil || none.Claim != nil || none.Warnings == nil {
		t.Errorf("nothing: %d %+v", code, none)
	}
	wantKeys(t, "status", keys, "hub", "firewall", "volume", "instance", "claim", "warnings")

	e.mustCreate("--ssh-from", "192.0.2.1", "--no-wait")
	e.remote.journal = `{"msg":"the hub is unowned: claim it once with this token","token":"abc","expires_at":"2026-10-12T00:00:00Z"}` + "\n"
	code, s, _ := status()
	if code != 0 || s.Instance == nil || s.Instance.PublicIPv4 != e.instanceIP() || s.Volume == nil || !slices.Equal(s.Volume.AttachedTo, []string{s.Instance.ID}) {
		t.Fatalf("status: %d %+v", code, s)
	}
	if s.Claim == nil || s.Claim.Token != "abc" || s.Claim.ExpiresAt != "2026-10-12T00:00:00Z" {
		t.Errorf("claim %+v", s.Claim)
	}
	if _, s, _ := status("--no-ssh"); s.Claim != nil {
		t.Errorf("--no-ssh read the claim: %+v", s.Claim)
	}
}

func TestHubDestroyAndAllowSSHJSON(t *testing.T) {
	e := newEnv(t)
	e.mustCreate("--ssh-from", "192.0.2.1", "--no-wait")

	code, out, _ := e.run("hub", "allow-ssh", "--provider", "fake", "--name", "hub-1", "--ssh-from", "198.51.100.7", "--output", "json")
	var allowed stepsJSON
	keys := decodeJSON(t, out, &allowed)
	if code != 0 || !allowed.Changed || stepActions(allowed.Steps) != "firewall:updated" || allowed.Steps[0].ID == "" {
		t.Errorf("allow-ssh: %d %+v", code, allowed)
	}
	wantKeys(t, "allow-ssh", keys, "hub", "dry_run", "changed", "steps")

	code, out, _ = e.run("hub", "destroy", "--provider", "fake", "--name", "hub-1", "--dry-run", "--delete-volume", "--output", "json")
	var plan destroyJSON
	keys = decodeJSON(t, out, &plan)
	if code != 3 || !plan.DryRun || plan.VolumeKept || stepActions(plan.Steps) != "instance:deleted volume:deleted" {
		t.Errorf("destroy dry run: %d %+v", code, plan)
	}
	wantKeys(t, "destroy", keys, "hub", "dry_run", "changed", "steps", "volume_kept")

	code, out, _ = e.run("hub", "destroy", "--provider", "fake", "--name", "hub-1", "--yes", "--output", "json")
	var gone destroyJSON
	decodeJSON(t, out, &gone)
	if code != 0 || !gone.VolumeKept || stepActions(gone.Steps) != "instance:deleted" {
		t.Errorf("destroy: %d %+v", code, gone)
	}
	if s := e.cloud.Counts(); s.Instances != 0 || s.Volumes != 1 {
		t.Errorf("destroy left %+v", s)
	}
}
