package hubvm

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/cloud/fake"
)

// recorder is the fake cloud, keeping the spec of every instance it was asked to create (the
// fake does not expose an instance's user data).
type recorder struct {
	*fake.Provider
	mu    sync.Mutex
	specs []cloud.InstanceSpec
}

func newRecorder() *recorder { return &recorder{Provider: fake.New()} }

func (r *recorder) CreateInstance(ctx context.Context, spec cloud.InstanceSpec) (cloud.Instance, cloud.Operation, error) {
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	r.mu.Unlock()
	return r.Provider.CreateInstance(ctx, spec)
}

func (r *recorder) userData() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.specs))
	for i, s := range r.specs {
		out[i] = s.UserData
	}
	return out
}

// readOnly are the provider methods that change nothing.
var readOnly = []string{"Regions", "Sizes", "Instances", "Instance", "Volumes", "Addresses", "Firewalls", "Wait"}

func mutating(calls []string) []string {
	var out []string
	for _, c := range calls {
		if !slices.Contains(readOnly, c) {
			out = append(out, c)
		}
	}
	return out
}

func testSpec() Spec {
	const name = "hub-1"
	return Spec{
		Name: name, Region: "fake-1", Size: "small", Image: "fedora-44-x64", VolumeGiB: 10,
		SSHKeyName: "gravel-" + name, SSHPublicKey: testKey,
		SSHFrom:  []string{"203.0.113.9", "198.51.100.0/24"},
		Document: Params{Profile: ProfileHost, SSHKeys: []string{testKey}},
	}
}

func mustCreate(t *testing.T, p cloud.Provider, s Spec) Result {
	t.Helper()
	r, err := Create(t.Context(), p, s, false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return r
}

func actions(steps []Step) string {
	var parts []string
	for _, s := range steps {
		parts = append(parts, string(s.Kind)+":"+string(s.Action))
	}
	return strings.Join(parts, " ")
}

func TestCreateFromNothing(t *testing.T) {
	p := newRecorder()
	r := mustCreate(t, p, testSpec())

	if got, want := actions(r.Steps), "firewall:created volume:created instance:created"; got != want {
		t.Errorf("steps %q, want %q", got, want)
	}
	if !r.Changed() || r.Recreated {
		t.Errorf("Changed %v Recreated %v", r.Changed(), r.Recreated)
	}
	if r.Instance == nil || r.Volume == nil || r.Firewall == nil {
		t.Fatalf("result: %+v", r)
	}
	inst := r.Instance
	if !slices.Contains(inst.Tags, BaseTag) || !slices.Contains(inst.Tags, Tag("hub-1")) {
		t.Errorf("instance tags %v", inst.Tags)
	}
	if !slices.Equal(inst.VolumeIDs, []string{r.Volume.ID}) {
		t.Errorf("instance volumes %v, want %s", inst.VolumeIDs, r.Volume.ID)
	}
	if inst.Status != cloud.StatusRunning || inst.PublicIPv4 == "" {
		t.Errorf("instance %s, %q", inst.Status, inst.PublicIPv4)
	}
	if r.Volume.Name != "hub-1-data" || r.Volume.SizeGiB != 10 || !slices.Contains(r.Volume.Tags, Tag("hub-1")) {
		t.Errorf("volume %+v", r.Volume)
	}
	for i, s := range r.Steps {
		if s.Resource == "" {
			t.Errorf("step %d has no resource id", i)
		}
	}

	fws, err := p.Firewalls(t.Context(), cloud.Filter{Name: FirewallName("hub-1")})
	if err != nil || len(fws) != 1 {
		t.Fatalf("firewalls %v %v", fws, err)
	}
	fw := fws[0]
	wantIn := []cloud.Rule{{Protocol: "tcp", Ports: "22", Addresses: []string{"198.51.100.0/24", "203.0.113.9/32"}}}
	if !rulesEqual(fw.Inbound, wantIn) {
		t.Errorf("inbound %+v, want %+v", fw.Inbound, wantIn)
	}
	if !rulesEqual(fw.Outbound, cloud.AllowAllOutbound()) {
		t.Errorf("outbound %+v", fw.Outbound)
	}
	if !slices.Equal(fw.Tags, []string{Tag("hub-1")}) {
		t.Errorf("firewall targets %v", fw.Tags)
	}

	ud := p.userData()
	if len(ud) != 1 {
		t.Fatalf("%d CreateInstance calls", len(ud))
	}
	c, _ := parseDoc(t, ud[0])
	if dev := c.FSSetup[0]["device"]; dev != p.VolumeDevice(*r.Volume) {
		t.Errorf("user data device %v, want %s", dev, p.VolumeDevice(*r.Volume))
	}
	if !slices.ContainsFunc(c.RunCmd, func(cmd []string) bool {
		return slices.Equal(cmd, []string{"hostnamectl", "set-hostname", "hub-1"})
	}) {
		t.Errorf("user data runcmd %v does not set the hostname", c.RunCmd)
	}
	spec := p.specs[0]
	if spec.Name != "hub-1" || spec.Region != "fake-1" || spec.Size != "small" || spec.Image != "fedora-44-x64" || len(spec.SSHKeyIDs) != 1 {
		t.Errorf("instance spec %+v", spec)
	}
}

func TestCreateTags(t *testing.T) {
	p := newRecorder()
	s := testSpec()
	s.Tags = []string{"grvl", "grvl:org:42", "grvl"}
	r := mustCreate(t, p, s)
	want := []string{BaseTag, Tag("hub-1"), "grvl", "grvl:org:42"}
	if !slices.Equal(p.specs[0].Tags, want) {
		t.Errorf("instance tags %v, want %v", p.specs[0].Tags, want)
	}
	if !slices.Equal(r.Volume.Tags, want) {
		t.Errorf("volume tags %v, want %v", r.Volume.Tags, want)
	}
	if !slices.Equal(r.Firewall.Tags, []string{Tag("hub-1")}) {
		t.Errorf("firewall targets %v: a caller's tag would widen what the firewall guards", r.Firewall.Tags)
	}
	// Found again without them: the tags are the caller's, gravel finds the hub by its own.
	again := mustCreate(t, p, testSpec())
	if again.Changed() {
		t.Errorf("a second create without the tags changed %s", actions(again.Steps))
	}

	for _, bad := range []string{BaseTag, Tag("other"), "", "two words"} {
		s := testSpec()
		s.Tags = []string{bad}
		if _, err := Create(t.Context(), fake.New(), s, true); err == nil {
			t.Errorf("tag %q: no error", bad)
		}
	}
}

func rulesEqual(a, b []cloud.Rule) bool {
	return slices.EqualFunc(a, b, func(x, y cloud.Rule) bool {
		return x.Protocol == y.Protocol && x.Ports == y.Ports && slices.Equal(x.Addresses, y.Addresses)
	})
}

func TestCreateTwiceChangesNothing(t *testing.T) {
	p := fake.New()
	first := mustCreate(t, p, testSpec())
	counts, n := p.Counts(), len(p.Calls())

	r := mustCreate(t, p, testSpec())
	if got, want := actions(r.Steps), "firewall:unchanged volume:unchanged instance:unchanged"; got != want {
		t.Errorf("steps %q, want %q", got, want)
	}
	if r.Changed() || r.Recreated {
		t.Errorf("Changed %v Recreated %v", r.Changed(), r.Recreated)
	}
	if p.Counts() != counts {
		t.Errorf("counts %+v, want %+v", p.Counts(), counts)
	}
	if m := mutating(p.Calls()[n:]); len(m) != 0 {
		t.Errorf("the second run called %v", m)
	}
	if r.Instance.ID != first.Instance.ID || r.Volume.ID != first.Volume.ID || r.Firewall.ID != first.Firewall.ID {
		t.Error("the second run found other resources")
	}
	if !strings.Contains(r.Steps[2].Detail, r.Instance.PublicIPv4) {
		t.Errorf("instance detail %q lacks the address", r.Steps[2].Detail)
	}
}

func TestCreateAfterDestroyRecreatesOnTheVolume(t *testing.T) {
	p := fake.New()
	first := mustCreate(t, p, testSpec())
	if _, err := Destroy(t.Context(), p, "hub-1", DestroyOptions{}); err != nil {
		t.Fatal(err)
	}
	r := mustCreate(t, p, testSpec())
	if got, want := actions(r.Steps), "firewall:unchanged volume:unchanged instance:created"; got != want {
		t.Errorf("steps %q, want %q", got, want)
	}
	if !r.Recreated {
		t.Error("Recreated is false")
	}
	if r.Instance.ID == first.Instance.ID {
		t.Error("the instance was not replaced")
	}
	if !slices.Equal(r.Instance.VolumeIDs, []string{first.Volume.ID}) {
		t.Errorf("new instance volumes %v, want %s", r.Instance.VolumeIDs, first.Volume.ID)
	}
	if !strings.Contains(r.Steps[2].Detail, "existing data volume") {
		t.Errorf("detail %q", r.Steps[2].Detail)
	}
}

func TestCreateSSHFrom(t *testing.T) {
	p := fake.New()
	first := mustCreate(t, p, testSpec())

	s := testSpec()
	s.SSHFrom = []string{"192.0.2.1"}
	r := mustCreate(t, p, s)
	fw := r.Steps[0]
	if fw.Action != Updated || fw.Resource != first.Firewall.ID {
		t.Errorf("firewall step %+v", fw)
	}
	if want := "198.51.100.0/24, 203.0.113.9/32 → 192.0.2.1/32"; !strings.Contains(fw.Detail, want) {
		t.Errorf("detail %q, want %q", fw.Detail, want)
	}
	if r.Steps[1].Action != Unchanged || r.Steps[2].Action != Unchanged {
		t.Errorf("steps %s", actions(r.Steps))
	}

	s.SSHFrom = nil
	r = mustCreate(t, p, s)
	if r.Steps[0].Action != Unchanged || !slices.Equal(SSHSources(*r.Firewall), []string{"192.0.2.1/32"}) {
		t.Errorf("empty SSHFrom: %+v, sources %v", r.Steps[0], SSHSources(*r.Firewall))
	}

	_, err := Create(t.Context(), fake.New(), s, false)
	if err == nil || !strings.Contains(err.Error(), "--ssh-from") {
		t.Errorf("a new hub without SSHFrom: %v", err)
	}
}

func TestCreateDryRun(t *testing.T) {
	p := fake.New()
	r, err := Create(t.Context(), p, testSpec(), true)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := actions(r.Steps), "firewall:created volume:created instance:created"; got != want {
		t.Errorf("steps %q, want %q", got, want)
	}
	if m := mutating(p.Calls()); len(m) != 0 {
		t.Errorf("a dry run called %v", m)
	}
	if s := p.Counts(); s != (fake.Counts{}) {
		t.Errorf("a dry run made %+v", s)
	}

	mustCreate(t, p, testSpec())
	n := len(p.Calls())
	r, err = Create(t.Context(), p, testSpec(), true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Changed() {
		t.Errorf("dry run after create: %s", actions(r.Steps))
	}
	if m := mutating(p.Calls()[n:]); len(m) != 0 {
		t.Errorf("a dry run called %v", m)
	}

	// A dry run that would change the firewall says so and leaves it.
	s := testSpec()
	s.SSHFrom = []string{"192.0.2.1"}
	r, err = Create(t.Context(), p, s, true)
	if err != nil || r.Steps[0].Action != Updated {
		t.Fatalf("%v %+v", err, r.Steps)
	}
	fws, _ := p.Firewalls(t.Context(), cloud.Filter{Name: FirewallName("hub-1")})
	if got := SSHSources(fws[0]); slices.Contains(got, "192.0.2.1/32") {
		t.Errorf("a dry run changed the firewall: %v", got)
	}
}

func TestCreateRefuses(t *testing.T) {
	ctx := t.Context()

	t.Run("two instances", func(t *testing.T) {
		p := fake.New()
		r := mustCreate(t, p, testSpec())
		other, op, err := p.CreateInstance(ctx, cloud.InstanceSpec{Name: "stray", Region: "fake-1", Size: "small", Image: "x", Tags: []string{Tag("hub-1")}})
		if err != nil || p.Wait(ctx, op) != nil {
			t.Fatal(err)
		}
		_, err = Create(ctx, p, testSpec(), false)
		if err == nil || !strings.Contains(err.Error(), "hub-1 ("+r.Instance.ID+")") || !strings.Contains(err.Error(), "stray ("+other.ID+")") {
			t.Errorf("err %v", err)
		}
		if _, err := Status(ctx, p, "hub-1"); err == nil {
			t.Error("Status with two instances: no error")
		}
	})

	t.Run("instance without the volume", func(t *testing.T) {
		p := fake.New()
		_, op, err := p.CreateInstance(ctx, cloud.InstanceSpec{Name: "hub-1", Region: "fake-1", Size: "small", Image: "x", Tags: []string{Tag("hub-1")}})
		if err != nil || p.Wait(ctx, op) != nil {
			t.Fatal(err)
		}
		_, err = Create(ctx, p, testSpec(), false)
		if err == nil || !strings.Contains(err.Error(), "destroy") {
			t.Errorf("err %v", err)
		}
		if n := p.CallCount("CreateInstance"); n != 1 {
			t.Errorf("CreateInstance called %d times", n)
		}
	})

	t.Run("someone else's volume", func(t *testing.T) {
		p := fake.New()
		if _, err := p.CreateVolume(ctx, cloud.VolumeSpec{Name: "hub-1-data", Region: "fake-1", SizeGiB: 1}); err != nil {
			t.Fatal(err)
		}
		_, err := Create(ctx, p, testSpec(), false)
		if err == nil || !strings.Contains(err.Error(), "not this hub's") {
			t.Errorf("err %v", err)
		}
		// The same name in another region is not this hub's volume either way.
		q := fake.New(fake.WithRegions(cloud.Region{Slug: "fake-1", Available: true}, cloud.Region{Slug: "fake-3", Available: true}))
		if _, err := q.CreateVolume(ctx, cloud.VolumeSpec{Name: "hub-1-data", Region: "fake-3", SizeGiB: 1}); err != nil {
			t.Fatal(err)
		}
		if r, err := Create(ctx, q, testSpec(), false); err != nil || r.Steps[1].Action != Created {
			t.Errorf("a volume in another region: %v %+v", err, r.Steps)
		}
	})

	t.Run("create volume fails", func(t *testing.T) {
		p := fake.New()
		p.FailNext("CreateVolume", errors.New("boom"))
		_, err := Create(ctx, p, testSpec(), false)
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("err %v", err)
		}
		if s := p.Counts(); s.Instances != 0 || s.Volumes != 0 || p.CallCount("CreateInstance") != 0 {
			t.Errorf("after a failed volume: %+v", s)
		}
		// The next run finishes the job.
		if r := mustCreate(t, p, testSpec()); actions(r.Steps) != "firewall:unchanged volume:created instance:created" {
			t.Errorf("retry: %s", actions(r.Steps))
		}
	})

	t.Run("instance never comes up", func(t *testing.T) {
		p := fake.New()
		p.HangNext("CreateInstance")
		short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		_, err := Create(short, p, testSpec(), false)
		if err == nil || !strings.Contains(err.Error(), "run create again") {
			t.Errorf("err %v", err)
		}
		r := mustCreate(t, p, testSpec())
		if r.Steps[2].Action != Unchanged || r.Instance.Status != cloud.StatusPending {
			t.Errorf("rerun: %+v", r.Steps[2])
		}
		if n := p.CallCount("CreateInstance"); n != 1 {
			t.Errorf("CreateInstance called %d times", n)
		}
		if s := p.Counts(); s.Instances != 1 {
			t.Errorf("%d instances", s.Instances)
		}
	})

	t.Run("rate limited", func(t *testing.T) {
		p := fake.New()
		p.RateLimitNext("Instances")
		_, err := Create(ctx, p, testSpec(), false)
		if !errors.Is(err, cloud.ErrRateLimited) {
			t.Errorf("err %v does not wrap ErrRateLimited", err)
		}
	})

	t.Run("bad spec", func(t *testing.T) {
		for _, mod := range []func(*Spec){
			func(s *Spec) { s.Name = "Hub" },
			func(s *Spec) { s.Region = "" },
			func(s *Spec) { s.Image = "" },
			func(s *Spec) { s.VolumeGiB = 0 },
			func(s *Spec) { s.SSHFrom = []string{"0.0.0.0/0"} },
			func(s *Spec) { s.Document.Profile = "nope" },
		} {
			p := fake.New()
			s := testSpec()
			mod(&s)
			if _, err := Create(ctx, p, s, false); err == nil {
				t.Errorf("%+v: no error", s)
			}
			if p.CallCount("CreateInstance") != 0 {
				t.Errorf("%+v: an instance was created", s)
			}
		}
	})
}

func TestCreateVolumeSizeMismatch(t *testing.T) {
	p := fake.New()
	first := mustCreate(t, p, testSpec())
	n := len(p.Calls())
	s := testSpec()
	s.VolumeGiB = 20
	r := mustCreate(t, p, s)
	if r.Steps[1].Action != Unchanged || !strings.Contains(r.Steps[1].Detail, "asked for 20 GiB") {
		t.Errorf("volume step %+v", r.Steps[1])
	}
	if !strings.Contains(r.Steps[1].Detail, "attached to "+first.Instance.ID) {
		t.Errorf("volume detail %q", r.Steps[1].Detail)
	}
	if r.Volume.SizeGiB != 10 {
		t.Errorf("volume is %d GiB", r.Volume.SizeGiB)
	}
	if m := mutating(p.Calls()[n:]); len(m) != 0 {
		t.Errorf("called %v", m)
	}
}

func TestValidName(t *testing.T) {
	for _, name := range []string{"hub-1", "ab", "a" + strings.Repeat("b", 40)} {
		if err := ValidName(name); err != nil {
			t.Errorf("%q: %v", name, err)
		}
	}
	for _, name := range []string{"1hub", "Hub", "hub-", "a", strings.Repeat("a", 42), "hub_1", "", "-hub"} {
		if err := ValidName(name); err == nil {
			t.Errorf("%q: no error", name)
		}
	}
}

func TestDestroy(t *testing.T) {
	ctx := t.Context()

	p := fake.New()
	mustCreate(t, p, testSpec())
	steps, err := Destroy(ctx, p, "hub-1", DestroyOptions{})
	if err != nil || len(steps) != 1 || steps[0].Kind != "instance" {
		t.Fatalf("%v %+v", err, steps)
	}
	if s := p.Counts(); s.Instances != 0 || s.Volumes != 1 || s.Firewalls != 1 {
		t.Errorf("default destroy left %+v", s)
	}

	p = fake.New()
	mustCreate(t, p, testSpec())
	n := len(p.Calls())
	steps, err = Destroy(ctx, p, "hub-1", DestroyOptions{DeleteVolume: true, DeleteFirewall: true, DryRun: true})
	if err != nil || len(steps) != 3 {
		t.Fatalf("dry run: %v %+v", err, steps)
	}
	if m := mutating(p.Calls()[n:]); len(m) != 0 {
		t.Errorf("a dry run called %v", m)
	}
	if s := p.Counts(); s.Instances != 1 || s.Volumes != 1 || s.Firewalls != 1 {
		t.Errorf("a dry run left %+v", s)
	}

	steps, err = Destroy(ctx, p, "hub-1", DestroyOptions{DeleteVolume: true, DeleteFirewall: true})
	if err != nil || len(steps) != 3 {
		t.Fatalf("%v %+v", err, steps)
	}
	if s := p.Counts(); s.Instances != 0 || s.Volumes != 0 || s.Firewalls != 0 {
		t.Errorf("full destroy left %+v", s)
	}

	steps, err = Destroy(ctx, fake.New(), "hub-1", DestroyOptions{DeleteVolume: true, DeleteFirewall: true})
	if err != nil || len(steps) != 0 {
		t.Errorf("nothing to destroy: %v %+v", err, steps)
	}
	if _, err := Destroy(ctx, p, "Bad", DestroyOptions{}); err == nil {
		t.Error("a bad name: no error")
	}
}

func TestDestroyWaitsForTheInstance(t *testing.T) {
	ctx := t.Context()
	p := fake.New()
	mustCreate(t, p, testSpec())
	p.FailOperationNext("DeleteInstance")
	if _, err := Destroy(ctx, p, "hub-1", DestroyOptions{DeleteVolume: true}); !errors.Is(err, cloud.ErrOperationFailed) {
		t.Errorf("err %v", err)
	}
	if s := p.Counts(); s.Volumes != 1 {
		t.Errorf("the volume went though the instance did not: %+v", s)
	}
}

func TestAllowSSH(t *testing.T) {
	ctx := t.Context()
	p := fake.New()
	first := mustCreate(t, p, testSpec())

	step, err := AllowSSH(ctx, p, "hub-1", []string{"192.0.2.0/24"}, true)
	if err != nil || step.Action != Updated {
		t.Fatalf("dry run: %v %+v", err, step)
	}
	if st, _ := Status(ctx, p, "hub-1"); slices.Contains(SSHSources(*st.Firewall), "192.0.2.0/24") {
		t.Error("a dry run changed the firewall")
	}

	step, err = AllowSSH(ctx, p, "hub-1", []string{"192.0.2.0/24"}, false)
	if err != nil || step.Action != Updated || step.Resource != first.Firewall.ID {
		t.Fatalf("%v %+v", err, step)
	}
	st, err := Status(ctx, p, "hub-1")
	if err != nil || !slices.Equal(SSHSources(*st.Firewall), []string{"192.0.2.0/24"}) {
		t.Errorf("sources %v (%v)", SSHSources(*st.Firewall), err)
	}
	if step, err := AllowSSH(ctx, p, "hub-1", []string{"192.0.2.7/24"}, false); err != nil || step.Action != Unchanged {
		t.Errorf("same sources: %v %+v", err, step)
	}

	if _, err := AllowSSH(ctx, fake.New(), "hub-1", []string{"192.0.2.1"}, false); err == nil || !strings.Contains(err.Error(), "run create first") {
		t.Errorf("no firewall: %v", err)
	}
	if _, err := AllowSSH(ctx, p, "hub-1", nil, false); err == nil {
		t.Error("no addresses: no error")
	}
	if _, err := AllowSSH(ctx, p, "hub-1", []string{"0.0.0.0/0"}, false); err == nil {
		t.Error("everywhere: no error")
	}
}

func TestStatus(t *testing.T) {
	ctx := t.Context()
	p := fake.New()
	st, err := Status(ctx, p, "hub-1")
	if err != nil || st.Instance != nil || st.Volume != nil || st.Firewall != nil {
		t.Errorf("nothing: %v %+v", err, st)
	}
	first := mustCreate(t, p, testSpec())
	n := len(p.Calls())
	st, err = Status(ctx, p, "hub-1")
	if err != nil || st.Instance == nil || st.Volume == nil || st.Firewall == nil {
		t.Fatalf("%v %+v", err, st)
	}
	if st.Instance.ID != first.Instance.ID || st.Volume.ID != first.Volume.ID || st.Firewall.ID != first.Firewall.ID {
		t.Errorf("status found other resources: %+v", st)
	}
	if m := mutating(p.Calls()[n:]); len(m) != 0 {
		t.Errorf("Status called %v", m)
	}
	if _, err := Status(ctx, p, "Bad"); err == nil {
		t.Error("a bad name: no error")
	}
	// Another hub's resources are not this one's.
	if st, _ := Status(ctx, p, "hub-2"); st.Instance != nil || st.Volume != nil || st.Firewall != nil {
		t.Errorf("hub-2: %+v", st)
	}
}
