package cloudvm_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/drivers/cloudvm"
)

// #166: a new server's volume made from a snapshot of an install, and a parked host's volume
// copied into one.

// installSnapshot is a snapshot in the rig's region, as one taken of another server's volume.
func installSnapshot(t *testing.T, r *rig) cloud.Snapshot {
	t.Helper()
	ctx := context.Background()
	src, err := r.p.CreateVolume(ctx, cloud.VolumeSpec{Name: "other-data", Region: "fake-1", SizeGiB: 100})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := r.p.CreateVolumeSnapshot(ctx, src.ID, "cs2-install", nil)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// seeded is a rig whose server's volume is made from ref; up says whether to run it.
func seeded(t *testing.T, ref cloud.SnapshotRef) *rig {
	t.Helper()
	r := newRig(t)
	r.src.edit(func(s *cloudvm.ServerSpec) { s.SeedSnapshot = &ref })
	if err := r.m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestASeedSnapshotMakesTheNewVolume(t *testing.T) {
	r := newRig(t)
	snap := installSnapshot(t, r)
	r.src.edit(func(s *cloudvm.ServerSpec) { s.SeedSnapshot = &cloud.SnapshotRef{Name: "cs2-install"} })
	if err := r.m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := r.up()
	if got := r.p.SourceSnapshot(h.VolumeID); got != snap.ID {
		t.Errorf("the volume was made from %q, want the seed %s", got, snap.ID)
	}
	if !slices.ContainsFunc(r.st.Events(), func(e cloudvm.Event) bool { return e.Detail == "from snapshot cs2-install" }) {
		t.Error("the seed is not in the host's events")
	}
	// The seed's environment file was another server's: the start pushed this one's (#169).
	if !strings.Contains(r.env(h.InstanceAddress), "CS2_RCONPW=rcon-secret\n") {
		t.Errorf("env:\n%s", r.env(h.InstanceAddress))
	}
}

func TestASeedSnapshotNeverReplacesTheServersVolume(t *testing.T) {
	r := newRig(t)
	installSnapshot(t, r)
	r.src.edit(func(s *cloudvm.ServerSpec) { s.SeedSnapshot = &cloud.SnapshotRef{Name: "cs2-install"} })
	if err := r.m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := r.up()
	ctx := context.Background()
	if _, err := r.m.SetDesired(ctx, serverID, cloudvm.DesiredParked, 0); err != nil {
		t.Fatal(err)
	}
	r.step(time.Second)
	if _, err := r.m.SetDesired(ctx, serverID, cloudvm.DesiredRunning, 0); err != nil {
		t.Fatal(err)
	}
	r.advance(time.Minute)
	again := r.up()
	if again.VolumeID != first.VolumeID || r.p.CallCount("CreateVolume") != 2 { // the source volume above, and the server's once
		t.Errorf("unparked on volume %s (first %s), %d volumes made: the server's own volume, never the seed again", again.VolumeID, first.VolumeID, r.p.CallCount("CreateVolume"))
	}
}

func TestAMissingSeedSnapshotFailsTheVolumeStep(t *testing.T) {
	r := seeded(t, cloud.SnapshotRef{Name: "nowhere"})
	r.remote.BootAll()
	if err := r.tick(); err == nil {
		t.Fatal("no error for a seed that does not exist")
	}
	if h := r.host(); h.State != cloudvm.StateCreating || h.VolumeID != "" || h.ErrorCode != cloudvm.CodeCloudFailed {
		t.Errorf("host %s/%s volume %q code %q: the step fails and is retried; no empty volume instead", h.State, h.Step, h.VolumeID, h.ErrorCode)
	}
}

func TestATooSmallVolumeForTheSeedIsRefused(t *testing.T) {
	r := newRig(t)
	installSnapshot(t, r)
	r.src.edit(func(s *cloudvm.ServerSpec) {
		s.SeedSnapshot = &cloud.SnapshotRef{Name: "cs2-install"}
		s.VolumeGiB = 50
	})
	if err := r.m.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.remote.BootAll()
	_ = r.tick()
	if h := r.host(); h.VolumeID != "" || h.ErrorCode != cloudvm.CodeCloudInvalid {
		t.Errorf("host volume %q code %q", h.VolumeID, h.ErrorCode)
	}
}

func TestSnapshotAParkedHostsVolume(t *testing.T) {
	r := newRig(t)
	h := r.up()
	ctx := context.Background()
	if _, err := r.m.SnapshotVolume(ctx, serverID, "", 3); !errors.Is(err, cloudvm.ErrNotParked) {
		t.Errorf("while running: %v; the volume is attached, so the copy would not be consistent", err)
	}
	if _, err := r.m.SetDesired(ctx, serverID, cloudvm.DesiredParked, 0); err != nil {
		t.Fatal(err)
	}
	r.step(time.Second)
	if _, err := r.m.SnapshotVolume(ctx, serverID, "bad name!", 3); !errors.Is(err, cloudvm.ErrInvalid) {
		t.Errorf("a bad name: %v", err)
	}
	snap, err := r.m.SnapshotVolume(ctx, serverID, "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if want := "cs2-1-data-cs2-1.0-2026-10-12"; snap.Name != want {
		t.Errorf("default name %q, want %q (the server's volume, the game, the build the last status read, the day)", snap.Name, want)
	}
	if !slices.Contains(snap.Tags, cloudvm.Tag(serverID)) || !slices.Contains(snap.Tags, cloudvm.BaseTag) {
		t.Errorf("tags %v", snap.Tags)
	}
	ev := r.eventsWith(cloudvm.DetailSnapshot)
	if len(ev) != 1 || ev[0].Detail != snap.Name || ev[0].AuditID != 3 || ev[0].From != cloudvm.StateParked {
		t.Errorf("events %+v", ev)
	}
	if r.counter("gravel_cloudvm_volume_snapshots_total", map[string]string{"result": "ok"}) != 1 {
		t.Error("the snapshot was not counted")
	}
	if got, err := r.p.GetVolumeSnapshot(ctx, snap.ID); err != nil || got.Region != "fake-1" {
		t.Errorf("in the cloud: %+v %v", got, err)
	}
	r.p.FailNext("CreateVolumeSnapshot", cloud.ErrRateLimited)
	if _, err := r.m.SnapshotVolume(ctx, serverID, "second", 4); err == nil || r.counter("gravel_cloudvm_volume_snapshots_total", map[string]string{"result": "error"}) != 1 {
		t.Errorf("a failing cloud: %v", err)
	}
	if h2 := r.host(); h2.State != cloudvm.StateParked || h2.VolumeID != h.VolumeID {
		t.Errorf("after the snapshots: %+v", h2)
	}
}

func TestSnapshotName(t *testing.T) {
	at := time.Date(2026, 10, 12, 23, 59, 0, 0, time.FixedZone("CDT", -5*3600))
	for _, c := range []struct{ server, game, build, want string }{
		{"htg-cs2-1", "cs2", "1.41.9.0", "htg-cs2-1-data-cs2-1.41.9.0-2026-10-13"}, // UTC day
		{"htg-cs2-1", "cs2", "", "htg-cs2-1-data-cs2-2026-10-13"},
		{"s", "cs2", "v 2/beta", "s-data-cs2-v-2-beta-2026-10-13"},
	} {
		if got := cloudvm.SnapshotName(c.server, c.game, c.build, at); got != c.want || !cloudvm.ValidSnapshotName(got) {
			t.Errorf("%q %q %q: %q, want %q", c.server, c.game, c.build, got, c.want)
		}
	}
	long := cloudvm.SnapshotName(strings.Repeat("a", 40), "cs2", strings.Repeat("9", 30), at)
	if len(long) > 64 || !cloudvm.ValidSnapshotName(long) {
		t.Errorf("long: %q (%d)", long, len(long))
	}
}
