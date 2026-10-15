package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/hubvm"
)

// The documents `gravel hub create|status|destroy|allow-ssh --output json` print: one JSON object
// on stdout, written whether the command succeeded or not once its flags parsed, so a program
// driving the CLI never parses text (gravel#211). Field names are snake_case and are a public
// surface (docs/releasing.md, the command lines); docs/cli.md "Machine-readable output" lists
// them. A program in Go imports the hubvm package instead.

const (
	outputText = "text"
	outputJSON = "json"
)

// addOutputFlag is --output, checked by outputMode after parsing.
func addOutputFlag(fs *flag.FlagSet) *string {
	return fs.String("output", outputText, "text, or json: one JSON object on stdout (docs/cli.md)")
}

// outputMode reports whether --output asked for JSON, and false with a message for a value it
// does not know.
func outputMode(name, value string, stderr io.Writer) (json, ok bool) {
	switch value {
	case outputText:
		return false, true
	case outputJSON:
		return true, true
	}
	fmt.Fprintf(stderr, "gravel %s: --output %q: want text or json\n", name, value)
	return false, false
}

type stepJSON struct {
	Kind   hubvm.Kind   `json:"kind"`
	Name   string       `json:"name"`
	Action hubvm.Action `json:"action"`
	// ID is the provider's id; empty for something a dry run would make.
	ID string `json:"id"`
	// Detail is for a person: never parse it.
	Detail string `json:"detail"`
}

// stepsJSON is what every command that changes something prints. In a dry run the steps say
// what would happen and changed says whether anything would.
type stepsJSON struct {
	Hub     string     `json:"hub"`
	DryRun  bool       `json:"dry_run"`
	Changed bool       `json:"changed"`
	Steps   []stepJSON `json:"steps"`
	Error   string     `json:"error,omitempty"`
}

func newStepsJSON(hub string, dryRun bool, steps []hubvm.Step) stepsJSON {
	out := stepsJSON{Hub: hub, DryRun: dryRun, Steps: []stepJSON{}}
	for _, s := range steps {
		out.Steps = append(out.Steps, stepJSON{Kind: s.Kind, Name: s.Name, Action: s.Action, ID: s.Resource, Detail: s.Detail})
		if s.Action != hubvm.Unchanged {
			out.Changed = true
		}
	}
	return out
}

type firewallJSON struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	SSHFrom []string `json:"ssh_from"`
}

type volumeJSON struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Region     string   `json:"region"`
	SizeGiB    int      `json:"size_gib"`
	AttachedTo []string `json:"attached_to"`
}

type instanceJSON struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Status      cloud.Status `json:"status"`
	Region      string       `json:"region"`
	Size        string       `json:"size"`
	Image       string       `json:"image"`
	PublicIPv4  string       `json:"public_ipv4"`
	PrivateIPv4 string       `json:"private_ipv4"`
	Tags        []string     `json:"tags"`
}

// resourcesJSON is the hub's three resources; null is one that does not exist (or, in a dry
// run, one that would be made has no id yet).
type resourcesJSON struct {
	Firewall *firewallJSON `json:"firewall"`
	Volume   *volumeJSON   `json:"volume"`
	Instance *instanceJSON `json:"instance"`
}

func newResourcesJSON(r hubvm.Result) resourcesJSON {
	var out resourcesJSON
	if fw := r.Firewall; fw != nil {
		out.Firewall = &firewallJSON{ID: fw.ID, Name: fw.Name, SSHFrom: orEmpty(hubvm.SSHSources(*fw))}
	}
	if v := r.Volume; v != nil {
		out.Volume = &volumeJSON{ID: v.ID, Name: v.Name, Region: v.Region, SizeGiB: v.SizeGiB, AttachedTo: orEmpty(v.InstanceIDs)}
	}
	if in := r.Instance; in != nil {
		out.Instance = &instanceJSON{ID: in.ID, Name: in.Name, Status: in.Status, Region: in.Region, Size: in.Size,
			Image: in.Image, PublicIPv4: in.PublicIPv4, PrivateIPv4: in.PrivateIPv4, Tags: orEmpty(in.Tags)}
	}
	return out
}

// createJSON is `gravel hub create --output json`.
type createJSON struct {
	stepsJSON
	resourcesJSON
	// Recreated is a new instance on a volume that existed: the hub came back with its data.
	Recreated bool `json:"recreated"`
	// Ready is SSH answering and cloud-init finished; false with --no-wait or a dry run.
	Ready bool `json:"ready"`
	// TunnelInstalled is the stock profile's tunnel token installed and gravel-cloudflared started.
	TunnelInstalled bool `json:"tunnel_installed"`
}

// statusJSON is `gravel hub status --output json`.
type statusJSON struct {
	Hub string `json:"hub"`
	resourcesJSON
	// Claim is an unowned stock hub's owner-claim token, read from its log over SSH; null when
	// the hub is owned, the token was not read (--no-ssh, no running instance) or reading failed.
	Claim    *claimJSON `json:"claim"`
	Warnings []string   `json:"warnings"`
	Error    string     `json:"error,omitempty"`
}

type claimJSON struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

// destroyJSON is `gravel hub destroy --output json`.
type destroyJSON struct {
	stepsJSON
	// VolumeKept is the data volume left for a later create (no --delete-volume).
	VolumeKept bool `json:"volume_kept"`
}

func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// writeJSON prints v as one indented object and a newline.
func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}
