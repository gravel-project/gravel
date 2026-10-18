package servers

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/gravel-project/gravel/drivers"
	"github.com/gravel-project/gravel/drivers/cloudvm"
)

// downDriver is a listDriver whose host is not running.
type downDriver struct {
	*listDriver
	phase string
}

func (d *downDriver) Build(context.Context) (string, error) {
	return "", &drivers.HostStateError{Phase: d.phase}
}
func (d *downDriver) Status(context.Context) (drivers.Status, error) {
	return drivers.Status{}, &drivers.HostStateError{Phase: d.phase}
}

func TestMonitorParkedHost(t *testing.T) {
	r := newListRig(t)
	ctx := context.Background()
	w := r.mon.workers["wd-1"]
	up := w.drv
	for _, c := range []struct{ phase, state string }{{drivers.HostParked, StateParked}, {drivers.HostProvisioning, StateProvisioning}} {
		w.drv = &downDriver{listDriver: r.drv, phase: c.phase}
		if err := r.mon.poll(ctx, "wd-1"); err != nil {
			t.Fatalf("%s: a host that is not running is not a failed poll: %v", c.phase, err)
		}
		o, _ := r.mon.Observation("wd-1")
		if o.State != c.state || o.Reachable || o.Players != nil {
			t.Errorf("%s: observation %+v", c.phase, o)
		}
		if v := testutil.ToFloat64(r.mon.reachable.WithLabelValues("wd-1", "wardogs")); v != 0 {
			t.Errorf("reachable %v", v)
		}
		if err := r.mon.watchBuild(ctx, "wd-1"); err != nil {
			t.Errorf("the build watcher failed on a parked host: %v", err)
		}
	}
	w.drv = up
	if err := r.mon.poll(ctx, "wd-1"); err != nil {
		t.Fatal(err)
	}
	if o, _ := r.mon.Observation("wd-1"); o.State != StateOK || !o.Reachable {
		t.Errorf("back up: %+v", o)
	}
}

// notLoadedDriver is a listDriver whose cloud-VM manager has not read its hosts yet.
type notLoadedDriver struct{ *listDriver }

func (d *notLoadedDriver) Build(context.Context) (string, error) { return "", cloudvm.ErrNotLoaded }
func (d *notLoadedDriver) Status(context.Context) (drivers.Status, error) {
	return drivers.Status{}, cloudvm.ErrNotLoaded
}

// A poll before the cloud-VM manager has loaded its hosts is not a failure and records nothing:
// the observation stays as it was and the next tick asks again (#229).
func TestMonitorHostNotLoadedYet(t *testing.T) {
	r := newListRig(t)
	ctx := context.Background()
	w := r.mon.workers["wd-1"]
	up := w.drv
	before, _ := r.mon.Observation("wd-1")
	w.drv = &notLoadedDriver{listDriver: r.drv}
	if err := r.mon.poll(ctx, "wd-1"); err != nil {
		t.Fatalf("a host not loaded yet is not a failed poll: %v", err)
	}
	o, _ := r.mon.Observation("wd-1")
	if o.State != before.State || o.Reachable != before.Reachable || o.Misses != 0 || o.LastError != "" || !o.FailingSince.IsZero() {
		t.Errorf("the observation changed: %+v (was %+v)", o, before)
	}
	if v := testutil.ToFloat64(r.mon.reachable.WithLabelValues("wd-1", "wardogs")); v != 1 {
		t.Errorf("reachable %v", v)
	}
	w.drv = up
	if err := r.mon.poll(ctx, "wd-1"); err != nil {
		t.Fatal(err)
	}
	if o, _ := r.mon.Observation("wd-1"); o.State != StateOK || !o.Reachable {
		t.Errorf("after the load: %+v", o)
	}
}
