package cloudvm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/gravel-project/gravel/cloud"
	"github.com/gravel-project/gravel/internal/jobs"
	"github.com/gravel-project/gravel/internal/vmdoc"
)

// Steps within a transition.
const (
	stepFirewall   = "firewall"
	stepVolume     = "volume"
	stepAddress    = "address"
	stepInstance   = "instance"
	stepWait       = "wait"
	stepAssign     = "assign"
	stepEnv        = "env"
	stepGame       = "game"
	stepEnvPending = "env_pending"
	stepQuit       = "quit"
	stepStop       = "stop"
	stepDelete     = "delete"
	stepDeleted    = "deleted"
	stepResize     = "resize"
	stepResized    = "resized"
	stepStart      = "start"
	stepStarted    = "started"
)

// Error codes: the words the API and the failing gauge show while a step keeps failing. The cause
// (which may name an address) goes to the log only.
const (
	CodeCloudFailed        = "cloud_failed"
	CodeCloudRateLimited   = "cloud_rate_limited"
	CodeCloudInvalid       = "cloud_invalid"
	CodeSSHFailed          = "ssh_failed"
	CodeHostKeyChanged     = "ssh_host_key_changed"
	CodeBootTimeout        = "boot_timeout"
	CodeInstallTimeout     = "install_timeout"
	CodeResizeFailed       = "resize_failed"
	CodeDuplicateInstances = "duplicate_instances"
	CodeVolumeNotOurs      = "volume_not_ours"
	CodeGameFailed         = "game_failed"
)

// ErrHostKeyChanged is a host presenting another SSH host key than the one the hub trusted: the
// hub stops until a human looks (it never forgets a key by itself).
var ErrHostKeyChanged = errors.New("cloudvm: the host's SSH key changed")

// stepError is a failure with its word.
type stepError struct {
	code string
	err  error
}

func (e *stepError) Error() string { return e.code + ": " + e.err.Error() }
func (e *stepError) Unwrap() error { return e.err }

func fail(code string, err error) error { return &stepError{code: code, err: err} }

// cloudErr gives a cloud error its word.
func cloudErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, cloud.ErrRateLimited):
		return fail(CodeCloudRateLimited, err)
	case errors.Is(err, cloud.ErrInvalid), errors.Is(err, cloud.ErrResizeNotAllowed):
		return fail(CodeCloudInvalid, err)
	}
	return fail(CodeCloudFailed, err)
}

// tick is one pass of one host's state machine.
type tick struct {
	m      *Manager
	rt     *hostRT
	s      ServerSpec
	p      cloud.Provider
	now    time.Time
	events []Event
}

// Tick runs a host's state machine until it has to wait, saving after every step, so a hub that
// stops halfway resumes where it was: everything it made is found again by tag and name. A
// failing step is retried with a growing backoff, and its word is on the host.
func (m *Manager) Tick(ctx context.Context, id string) error {
	rt, s, ok := m.host(id)
	if !ok {
		return nil
	}
	rt.tickMu.Lock()
	defer rt.tickMu.Unlock()
	before := rt.snapshot()
	now := m.opt.Now()
	if now.Before(before.NextAttemptAt) {
		return nil
	}
	m.account(rt, before, now)
	p, err := m.provider(ctx, s.Location)
	if err != nil {
		return m.failed(ctx, rt, before, fail(CodeCloudFailed, err))
	}
	for i := 0; i < maxStepsPerTick; i++ {
		t := &tick{m: m, rt: rt, s: s, p: p, now: m.opt.Now()}
		w := cloneHost(before)
		progressed, err := t.advance(ctx, &w)
		if err != nil {
			if cerr := m.commit(ctx, rt, before, w, t.events); cerr != nil {
				return cerr
			}
			return m.failed(ctx, rt, rt.snapshot(), err)
		}
		if progressed && (w.Attempts != 0 || w.ErrorCode != "") {
			w.Attempts, w.NextAttemptAt, w.ErrorCode, w.LastError = 0, time.Time{}, "", ""
			m.met.clearFailing(rt, s.ID)
		}
		if err := m.commit(ctx, rt, before, w, t.events); err != nil {
			return err
		}
		if !progressed {
			return nil
		}
		before = rt.snapshot()
	}
	return nil
}

// failed records a failing step: its word, the attempt and when to try again.
func (m *Manager) failed(ctx context.Context, rt *hostRT, h Host, err error) error {
	code := CodeCloudFailed
	var se *stepError
	if errors.As(err, &se) {
		code = se.code
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	after := cloneHost(h)
	after.Attempts++
	after.NextAttemptAt = m.opt.Now().Add(jobs.Backoff(backoffBase, backoffMax, after.Attempts))
	first := after.ErrorCode != code
	after.ErrorCode, after.LastError = code, err.Error()
	if cerr := m.commit(ctx, rt, h, after, nil); cerr != nil {
		return cerr
	}
	m.met.setFailing(rt, h.ServerID, code)
	m.met.ops.WithLabelValues(h.ServerID, opOf(h.State), "error").Inc()
	if first {
		m.log.Warn("a host step failed; retrying with backoff", "server", h.ServerID, "state", h.State, "step", h.Step, "code", code, "error", err.Error())
	}
	return fmt.Errorf("server %s: %s", h.ServerID, code)
}

// account adds the time a host ran since the last tick to its running counter.
func (m *Manager) account(rt *hostRT, h Host, now time.Time) {
	rt.mu.Lock()
	last := rt.lastAccount
	rt.lastAccount = now
	rt.mu.Unlock()
	if h.State == StateRunning && !last.IsZero() && now.After(last) {
		m.met.running.WithLabelValues(h.ServerID).Add(now.Sub(last).Seconds())
	}
}

// to moves the host to a state (and step), recording the transition with the request that caused
// it, if any.
func (t *tick) to(h *Host, state, step, detail string) {
	from := h.State
	h.Step = step
	if state == from {
		return
	}
	h.State, h.StateSince = state, t.now
	e := Event{ServerID: h.ServerID, At: t.now, From: from, To: state, Step: step, Detail: detail, Size: h.Size, AuditID: h.RequestAuditID}
	if state == StateRunning {
		e.PriceHourly = t.m.Price(context.Background(), h.ServerID, h.Size)
	}
	h.RequestAuditID = 0
	t.events = append(t.events, e)
	t.m.log.Info("host", "server", h.ServerID, "from", from, "to", state, "detail", detail)
	t.m.met.transition(t.rt, h.ServerID, from, state, t.now)
}

// desired is the state the host should rest in: a removed server's host is parked.
func (t *tick) desired(h *Host) string {
	if t.s.Removed && h.Desired == DesiredRunning {
		return DesiredParked
	}
	return h.Desired
}

func (t *tick) advance(ctx context.Context, h *Host) (bool, error) {
	if h.Operation != nil {
		return t.waitOperation(ctx, h)
	}
	want := t.desired(h)
	if want != DesiredRunning && (h.State == StateCreating || h.State == StateBooting || h.State == StateStarting) {
		if h.InstanceID != "" {
			t.to(h, StateParking, stepStop, "parked while starting")
		} else {
			t.to(h, StateParked, "", "parked while starting")
		}
		return true, nil
	}
	switch h.State {
	case StateAbsent, StateParked:
		switch {
		case want == DesiredRunning:
			detail := "create"
			if h.State == StateParked {
				detail = "unpark"
			}
			t.to(h, StateCreating, stepFirewall, detail)
			return true, nil
		case want == DesiredAbsent && h.State == StateParked:
			t.to(h, StateDestroying, stepInstance, "destroy")
			return true, nil
		}
		return false, nil
	case StateCreating:
		return t.create(ctx, h)
	case StateBooting:
		return t.boot(ctx, h)
	case StateStarting:
		return t.start(ctx, h)
	case StateRunning:
		return t.running(ctx, h, want)
	case StateParking:
		return t.park(ctx, h)
	case StateResizing:
		return t.resize(ctx, h)
	case StateDestroying:
		return t.destroy(ctx, h)
	}
	return false, fmt.Errorf("cloudvm: host %s is in an unknown state %q", h.ServerID, h.State)
}

// waitOperation waits a while for the cloud operation in flight. A failed resize restarts the
// host at its old size; any other failure is retried by its step.
func (t *tick) waitOperation(ctx context.Context, h *Host) (bool, error) {
	wctx, cancel := context.WithTimeout(ctx, OperationWait)
	defer cancel()
	err := t.p.Wait(wctx, *h.Operation)
	switch {
	case err == nil:
		h.Operation = nil
		return true, nil
	case errors.Is(err, cloud.ErrOperationFailed):
		h.Operation = nil
		if h.State == StateResizing && h.Step == stepResized {
			h.TargetSize, h.Step = "", stepStart
			t.m.log.Warn("the resize failed; starting the host at its old size", "server", h.ServerID, "error", err.Error())
			t.events = append(t.events, Event{ServerID: h.ServerID, At: t.now, From: h.State, To: h.State, Step: stepStart, Detail: CodeResizeFailed, Size: h.Size})
			t.m.met.ops.WithLabelValues(h.ServerID, "resize", "error").Inc()
			return true, nil
		}
		return false, fail(CodeCloudFailed, err)
	case ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded):
		return false, nil // still running at the cloud; the next tick waits again
	}
	return false, cloudErr(err)
}

func (t *tick) create(ctx context.Context, h *Host) (bool, error) {
	s, p := t.s, t.p
	switch h.Step {
	case stepFirewall, "":
		egress := t.m.currentEgress()
		fw, err := p.EnsureFirewall(ctx, firewallSpec(s, egress))
		if err != nil {
			return false, cloudErr(err)
		}
		h.FirewallID, h.Egress, h.Step = fw.ID, egress, stepVolume
		return true, nil
	case stepVolume:
		vols, err := p.Volumes(ctx, cloud.Filter{Name: VolumeName(s.ID)})
		if err != nil {
			return false, cloudErr(err)
		}
		vols = slices.DeleteFunc(vols, func(v cloud.Volume) bool { return v.Region != s.Location.Region })
		switch {
		case len(vols) > 0:
			v := vols[0]
			if !slices.Contains(v.Tags, Tag(s.ID)) {
				return false, fail(CodeVolumeNotOurs, fmt.Errorf("volume %s in %s exists without the tag %s; the hub will not use a volume it did not make", v.Name, v.Region, Tag(s.ID)))
			}
			if v.SizeGiB < s.VolumeGiB {
				t.m.log.Warn("the data volume is smaller than the manifest says; the hub never resizes a volume", "server", s.ID, "volume_gib", v.SizeGiB, "manifest_gib", s.VolumeGiB)
			}
			if h.VolumeID != v.ID {
				h.EnvSHA256 = nil // another volume than the one the env file was pushed to
			}
			h.VolumeID = v.ID
		default:
			v, err := p.CreateVolume(ctx, cloud.VolumeSpec{Name: VolumeName(s.ID), Region: s.Location.Region, SizeGiB: s.VolumeGiB, Tags: []string{BaseTag, Tag(s.ID)}})
			if err != nil {
				return false, cloudErr(err)
			}
			h.VolumeID, h.EnvSHA256 = v.ID, nil
		}
		h.Step = stepInstance
		if s.AddressReserved {
			h.Step = stepAddress
		}
		return true, nil
	case stepAddress:
		if h.AddressReserved && h.Address != "" {
			addrs, err := p.Addresses(ctx, cloud.Filter{Name: h.Address})
			if err != nil {
				return false, cloudErr(err)
			}
			if len(addrs) == 0 {
				t.m.log.Warn("the server's reserved address is gone; allocating another (the connect string changes)", "server", s.ID)
				h.Address, h.AddressReserved = "", false
			}
		}
		if h.Address == "" || !h.AddressReserved {
			a, err := p.AllocateAddress(ctx, s.Location.Region, []string{BaseTag, Tag(s.ID)})
			if err != nil {
				return false, cloudErr(err)
			}
			h.Address, h.AddressReserved = a.IP, true
		}
		h.Step = stepInstance
		return true, nil
	case stepInstance:
		insts, err := p.Instances(ctx, cloud.Filter{Tag: Tag(s.ID)})
		if err != nil {
			return false, cloudErr(err)
		}
		switch len(insts) {
		case 0:
		case 1:
			if h.InstanceID != insts[0].ID {
				h.HostKey = "" // an instance the hub made before it could record it
			}
			h.InstanceID, h.Step = insts[0].ID, stepWait
			if h.InstanceCreatedAt.IsZero() {
				h.InstanceCreatedAt = insts[0].Created
			}
			return true, nil
		default:
			return false, fail(CodeDuplicateInstances, fmt.Errorf("%d instances carry %s; delete all but one", len(insts), Tag(s.ID)))
		}
		pub := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.Location.SSHKey.PublicKey())))
		keyID, err := p.EnsureSSHKey(ctx, SSHKeyName(s.Location.ID), pub)
		if err != nil {
			return false, cloudErr(err)
		}
		doc, err := Document(s, s.GameName, p.VolumeDevice(cloud.Volume{ID: h.VolumeID, Name: VolumeName(s.ID), Region: s.Location.Region}), append([]string{pub + " gravel-hub"}, s.Location.ExtraSSHKeys...))
		if err != nil {
			return false, err
		}
		image := s.Location.Image
		if image == "" {
			image = t.m.opt.DefaultImages[s.Location.Provider]
		}
		inst, op, err := p.CreateInstance(ctx, cloud.InstanceSpec{
			Name: InstanceName(s.ID), Region: s.Location.Region, Size: s.Size, Image: image, UserData: doc,
			SSHKeyIDs: []string{keyID}, Tags: []string{BaseTag, Tag(s.ID)}, VolumeIDs: []string{h.VolumeID}, Monitoring: true,
		})
		if inst.ID != "" {
			h.InstanceID, h.InstanceCreatedAt, h.HostKey, h.Step = inst.ID, t.now, "", stepWait
			if !op.Done() {
				h.Operation = &op
			}
		}
		if err != nil {
			return inst.ID != "", cloudErr(err)
		}
		return true, nil
	case stepWait:
		inst, err := p.Instance(ctx, h.InstanceID)
		if errors.Is(err, cloud.ErrNotFound) {
			h.InstanceID, h.Step = "", stepInstance
			return true, nil
		}
		if err != nil {
			return false, cloudErr(err)
		}
		if inst.Status != cloud.StatusRunning || inst.PublicIPv4 == "" {
			return false, nil
		}
		h.InstanceAddress, h.DiskGiB, h.Size, h.Step = inst.PublicIPv4, inst.DiskGiB, inst.Size, stepAssign
		return true, nil
	case stepAssign:
		if !s.AddressReserved {
			h.Address, h.AddressReserved = h.InstanceAddress, false
			t.to(h, StateBooting, "", "")
			return true, nil
		}
		addrs, err := p.Addresses(ctx, cloud.Filter{Name: h.Address})
		if err != nil {
			return false, cloudErr(err)
		}
		if len(addrs) == 0 {
			h.Step = stepAddress
			return true, nil
		}
		if addrs[0].InstanceID != h.InstanceID {
			op, err := p.AssignAddress(ctx, h.Address, h.InstanceID)
			if err != nil {
				return false, cloudErr(err)
			}
			if !op.Done() {
				h.Operation = &op
			}
			return true, nil
		}
		t.to(h, StateBooting, "", "")
		return true, nil
	}
	return false, fmt.Errorf("cloudvm: unknown creating step %q", h.Step)
}

// remote runs a command on the host as the runtime user, trusting its key on first use.
func (t *tick) remote(ctx context.Context, h *Host, stdin []byte, command string) error {
	rctx, cancel := context.WithTimeout(ctx, remoteRunTimeout)
	defer cancel()
	var in io.Reader
	if stdin != nil {
		in = bytes.NewReader(stdin)
	}
	target := RemoteTarget{Address: h.InstanceAddress, User: vmdoc.DefaultUser, Signer: t.s.Location.SSHKey, HostKey: h.HostKey}
	_, key, err := t.m.opt.Remote.Run(rctx, target, in, command)
	if h.HostKey == "" && key != "" {
		h.HostKey = key
	}
	if errors.Is(err, ErrHostKeyChanged) {
		return fail(CodeHostKeyChanged, err)
	}
	return err
}

func (t *tick) boot(ctx context.Context, h *Host) (bool, error) {
	err := t.remote(ctx, h, nil, "test -e ~/"+BootMarker)
	if err != nil {
		var se *stepError
		if errors.As(err, &se) {
			return false, err
		}
		if t.now.Sub(h.StateSince) > BootTimeout {
			return false, fail(CodeBootTimeout, fmt.Errorf("the host has not finished its first boot in %s: %w", BootTimeout, err))
		}
		return false, nil // not up yet
	}
	if h.Step != stepResized && !h.InstanceCreatedAt.IsZero() {
		h.BootSeconds = int(t.now.Sub(h.InstanceCreatedAt).Seconds())
	}
	t.to(h, StateStarting, stepEnv, "")
	return true, nil
}

// pushEnv writes the game's environment file and restarts its unit (or, with restart false, only
// writes it).
func (t *tick) pushEnv(ctx context.Context, h *Host, env []byte, restart bool) error {
	g := t.s.Game
	script := "set -eu\numask 077\nmkdir -p ~/.config/" + g + "\ncat > ~/.config/" + g + "/env.tmp\nmv -f ~/.config/" + g + "/env.tmp ~/.config/" + g + "/env\n"
	if restart {
		script += "systemctl --user daemon-reload\nsystemctl --user restart " + g + ".service\n"
	}
	if err := t.remote(ctx, h, env, script); err != nil {
		var se *stepError
		if errors.As(err, &se) {
			return err
		}
		return fail(CodeSSHFailed, err)
	}
	return nil
}

func (t *tick) start(ctx context.Context, h *Host) (bool, error) {
	switch h.Step {
	case stepEnv, "":
		env, err := RenderEnv(t.s)
		if err != nil {
			return false, err
		}
		if sum := envHash(env); !bytes.Equal(sum, h.EnvSHA256) {
			if err := t.pushEnv(ctx, h, env, true); err != nil {
				return false, err
			}
			h.EnvSHA256 = sum
		}
		h.Step = stepGame
		return true, nil
	case stepGame:
		st, err := t.status(ctx, h)
		if err != nil {
			if t.now.Sub(h.StateSince) > time.Duration(t.s.Profile.ReadyTimeout) {
				return false, fail(CodeInstallTimeout, fmt.Errorf("the game has not answered in %s: %w", time.Duration(t.s.Profile.ReadyTimeout), err))
			}
			return false, nil // installing or starting
		}
		t.rt.mu.Lock()
		op := t.rt.op
		t.rt.mu.Unlock()
		if (op == "create" || op == "unpark" || op == "recreate") && !h.InstanceCreatedAt.IsZero() {
			h.ReadySeconds = int(t.now.Sub(h.InstanceCreatedAt).Seconds())
		}
		t.to(h, StateRunning, "", st.Build)
		return true, nil
	}
	return false, fmt.Errorf("cloudvm: unknown starting step %q", h.Step)
}

// status reads the game's status and records it.
func (t *tick) status(ctx context.Context, h *Host) (GameStatus, error) {
	t.rt.mu.Lock()
	g, err := t.m.gameForLocked(t.rt, t.s, h.InstanceAddress)
	t.rt.mu.Unlock()
	if err != nil {
		return GameStatus{}, err
	}
	sctx, cancel := context.WithTimeout(ctx, gameCallTimeout)
	defer cancel()
	st, err := g.Status(sctx)
	if err != nil {
		return GameStatus{}, err
	}
	t.m.observe(t.rt, st, t.now)
	return st, nil
}

func (t *tick) running(ctx context.Context, h *Host, want string) (bool, error) {
	if want != DesiredRunning {
		t.to(h, StateParking, stepQuit, map[string]string{DesiredParked: "park", DesiredAbsent: "destroy"}[want])
		return true, nil
	}
	if h.TargetSize != "" {
		if h.TargetSize == h.Size {
			h.TargetSize = ""
			return true, nil
		}
		t.to(h, StateResizing, stepQuit, h.Size+" to "+h.TargetSize)
		return true, nil
	}
	t.rt.mu.Lock()
	drift := t.now.Sub(t.rt.lastDrift) >= DriftEvery
	if drift {
		t.rt.lastDrift = t.now
	}
	players, known, emptySince := t.rt.players, t.rt.known, t.rt.h.EmptySince
	t.rt.mu.Unlock()
	if drift || !slices.Equal(t.m.currentEgress(), h.Egress) {
		if moved, err := t.checkDrift(ctx, h); moved || err != nil {
			return moved, err
		}
	}
	env, err := RenderEnv(t.s)
	if err != nil {
		return false, err
	}
	if sum := envHash(env); !bytes.Equal(sum, h.EnvSHA256) {
		if !known || players > 0 {
			if h.Step != stepEnvPending {
				h.Step = stepEnvPending
				t.m.log.Info("the server's settings or secrets changed; the game restarts with them when it is empty", "server", h.ServerID)
				return true, nil
			}
			return false, nil
		}
		if err := t.pushEnv(ctx, h, env, true); err != nil {
			return false, err
		}
		h.EnvSHA256 = sum
		t.to(h, StateStarting, stepGame, "settings changed")
		return true, nil
	}
	if t.s.ParkAfter > 0 && known && players == 0 && !emptySince.IsZero() && t.now.Sub(emptySince) >= t.s.ParkAfter {
		h.Desired = DesiredParked
		t.m.log.Info("the server has been empty; parking its host", "server", h.ServerID, "empty_for", t.now.Sub(emptySince).Round(time.Second).String())
		t.to(h, StateParking, stepQuit, "idle")
		return true, nil
	}
	return false, nil
}

// checkDrift looks at the host as the cloud has it: an instance someone stopped is started, a lost
// one made again on the volume, and the firewall pinned to the hub's egress.
func (t *tick) checkDrift(ctx context.Context, h *Host) (bool, error) {
	insts, err := t.p.Instances(ctx, cloud.Filter{Tag: Tag(t.s.ID)})
	if err != nil {
		return false, cloudErr(err)
	}
	switch {
	case len(insts) == 0:
		t.m.log.Warn("the server's instance is gone; making it again on its volume", "server", h.ServerID)
		h.InstanceID, h.InstanceAddress, h.HostKey = "", "", ""
		t.forgetGame()
		t.to(h, StateCreating, stepInstance, "recreate")
		return true, nil
	case len(insts) > 1:
		return false, fail(CodeDuplicateInstances, fmt.Errorf("%d instances carry %s", len(insts), Tag(t.s.ID)))
	case insts[0].Status == cloud.StatusStopped:
		op, err := t.p.StartInstance(ctx, insts[0].ID)
		if err != nil {
			return false, cloudErr(err)
		}
		if !op.Done() {
			h.Operation = &op
		}
		t.to(h, StateBooting, stepResized, "started")
		return true, nil
	}
	egress := t.m.currentEgress()
	want := firewallSpec(t.s, egress)
	fws, err := t.p.Firewalls(ctx, cloud.Filter{Name: want.Name})
	if err != nil {
		return false, cloudErr(err)
	}
	if len(fws) == 0 || !cloud.SameFirewallSpec(fws[0].FirewallSpec, want) {
		fw, err := t.p.EnsureFirewall(ctx, want)
		if err != nil {
			return false, cloudErr(err)
		}
		h.FirewallID = fw.ID
		t.m.log.Info("the server's firewall was pinned again", "server", h.ServerID, "egress", egress)
	}
	h.Egress = egress
	return false, nil
}

func (t *tick) forgetGame() {
	t.rt.mu.Lock()
	if t.rt.game != nil {
		t.rt.game.Forget()
	}
	t.rt.game, t.rt.gameAddr, t.rt.known, t.rt.players = nil, "", false, 0
	t.rt.mu.Unlock()
}

// quit stops the game first, warning whoever is on.
func (t *tick) quit(ctx context.Context, h *Host, warning string) {
	t.rt.mu.Lock()
	g, players := t.rt.game, t.rt.players
	t.rt.mu.Unlock()
	if g == nil || h.InstanceAddress == "" {
		return
	}
	if players > 0 {
		sctx, cancel := context.WithTimeout(ctx, gameCallTimeout)
		if err := g.Say(sctx, warning); err == nil {
			t.m.opt.Sleep(ctx, ParkWarning)
		}
		cancel()
	}
	qctx, cancel := context.WithTimeout(ctx, gameCallTimeout)
	defer cancel()
	_ = g.Quit(qctx) // the instance stops either way
}

// stop stops the instance; true once it is stopped.
func (t *tick) stop(ctx context.Context, h *Host) (stopped bool, progressed bool, err error) {
	inst, err := t.p.Instance(ctx, h.InstanceID)
	if errors.Is(err, cloud.ErrNotFound) {
		h.InstanceID = ""
		return true, true, nil
	}
	if err != nil {
		return false, false, cloudErr(err)
	}
	if inst.Status == cloud.StatusStopped {
		return true, false, nil
	}
	op, err := t.p.StopInstance(ctx, h.InstanceID, true)
	if err != nil {
		return false, false, cloudErr(err)
	}
	if !op.Done() {
		h.Operation = &op
	}
	return false, true, nil
}

func (t *tick) billsWhenStopped(ctx context.Context, size string) bool {
	all, err := t.p.Sizes(ctx)
	if err != nil {
		return true // the safe assumption: delete it
	}
	z, ok := sizeBySlug(all, size)
	return !ok || z.BillsWhenStopped
}

func (t *tick) park(ctx context.Context, h *Host) (bool, error) {
	switch h.Step {
	case stepQuit, "":
		t.quit(ctx, h, "This server is parking in 10 seconds. It comes back when someone starts it again.")
		h.Step = stepStop
		return true, nil
	case stepStop:
		if h.InstanceID == "" {
			return t.parked(h), nil
		}
		stopped, progressed, err := t.stop(ctx, h)
		if err != nil || !stopped {
			return progressed, err
		}
		if h.InstanceID == "" {
			return t.parked(h), nil
		}
		if !t.billsWhenStopped(ctx, h.Size) {
			return t.parked(h), nil
		}
		h.Step = stepDelete
		return true, nil
	case stepDelete:
		op, err := t.p.DeleteInstance(ctx, h.InstanceID)
		if err != nil && !errors.Is(err, cloud.ErrNotFound) {
			return false, cloudErr(err)
		}
		if err == nil && !op.Done() {
			h.Operation = &op
		}
		h.Step = stepDeleted
		return true, nil
	case stepDeleted:
		h.InstanceID, h.InstanceAddress, h.HostKey = "", "", ""
		return t.parked(h), nil
	}
	return false, fmt.Errorf("cloudvm: unknown parking step %q", h.Step)
}

func (t *tick) parked(h *Host) bool {
	if !h.AddressReserved && h.InstanceID == "" {
		h.Address = ""
	}
	t.forgetGame()
	t.rt.mu.Lock()
	t.rt.h.EmptySince = time.Time{}
	t.rt.mu.Unlock()
	t.to(h, StateParked, "", "")
	return true
}

func (t *tick) resize(ctx context.Context, h *Host) (bool, error) {
	switch h.Step {
	case stepQuit, "":
		t.quit(ctx, h, "This server restarts in 10 seconds on a "+h.TargetSize+" machine. Rejoin in a few minutes.")
		h.Step = stepStop
		return true, nil
	case stepStop:
		stopped, progressed, err := t.stop(ctx, h)
		if err != nil || !stopped {
			return progressed, err
		}
		if h.InstanceID == "" {
			t.to(h, StateCreating, stepInstance, "recreate")
			return true, nil
		}
		h.Step = stepResize
		return true, nil
	case stepResize:
		all, err := t.p.Sizes(ctx)
		if err != nil {
			return false, cloudErr(err)
		}
		from, okFrom := sizeBySlug(all, h.Size)
		to, okTo := sizeBySlug(all, h.TargetSize)
		if okFrom && h.DiskGiB > 0 {
			from.DiskGiB = h.DiskGiB
		}
		if !okFrom || !okTo || t.p.CanResize(from, to) != nil {
			t.m.log.Warn("the resize is not allowed; starting the host at its size", "server", h.ServerID, "from", h.Size, "to", h.TargetSize)
			t.events = append(t.events, Event{ServerID: h.ServerID, At: t.now, From: h.State, To: h.State, Step: stepStart, Detail: CodeResizeFailed, Size: h.Size})
			h.TargetSize, h.Step = "", stepStart
			return true, nil
		}
		op, err := t.p.ResizeInstance(ctx, h.InstanceID, h.TargetSize)
		if err != nil {
			if errors.Is(err, cloud.ErrResizeNotAllowed) || errors.Is(err, cloud.ErrInvalid) {
				h.TargetSize, h.Step = "", stepStart
				return true, nil
			}
			return false, cloudErr(err)
		}
		if !op.Done() {
			h.Operation = &op
		}
		h.Step = stepResized
		return true, nil
	case stepResized:
		inst, err := t.p.Instance(ctx, h.InstanceID)
		if err != nil {
			return false, cloudErr(err)
		}
		h.Size, h.TargetSize, h.Step = inst.Size, "", stepStart
		return true, nil
	case stepStart:
		op, err := t.p.StartInstance(ctx, h.InstanceID)
		if err != nil {
			return false, cloudErr(err)
		}
		if !op.Done() {
			h.Operation = &op
		}
		h.Step = stepStarted
		return true, nil
	case stepStarted:
		t.forgetGame()
		t.to(h, StateBooting, stepResized, "")
		return true, nil
	}
	return false, fmt.Errorf("cloudvm: unknown resizing step %q", h.Step)
}

func (t *tick) destroy(ctx context.Context, h *Host) (bool, error) {
	switch h.Step {
	case stepInstance, "":
		insts, err := t.p.Instances(ctx, cloud.Filter{Tag: Tag(t.s.ID)})
		if err != nil {
			return false, cloudErr(err)
		}
		if len(insts) > 0 {
			op, err := t.p.DeleteInstance(ctx, insts[0].ID)
			if err != nil && !errors.Is(err, cloud.ErrNotFound) {
				return false, cloudErr(err)
			}
			if err == nil && !op.Done() {
				h.Operation = &op
			}
			return true, nil
		}
		h.InstanceID, h.InstanceAddress, h.HostKey, h.Step = "", "", "", stepVolume
		return true, nil
	case stepVolume:
		if h.DestroyVolume && h.VolumeID != "" {
			if err := t.p.DeleteVolume(ctx, h.VolumeID); err != nil && !errors.Is(err, cloud.ErrNotFound) {
				return false, cloudErr(err)
			}
			h.VolumeID, h.EnvSHA256 = "", nil
		}
		h.Step = stepAddress
		return true, nil
	case stepAddress:
		if h.ReleaseAddress && h.AddressReserved && h.Address != "" {
			rctx, cancel := context.WithTimeout(ctx, OperationWait)
			err := t.p.ReleaseAddress(rctx, h.Address)
			cancel()
			if err != nil && !errors.Is(err, cloud.ErrNotFound) {
				return false, cloudErr(err)
			}
			h.Address, h.AddressReserved = "", false
		}
		h.Step = stepFirewall
		return true, nil
	case stepFirewall:
		if h.FirewallID != "" {
			if err := t.p.DeleteFirewall(ctx, h.FirewallID); err != nil && !errors.Is(err, cloud.ErrNotFound) {
				return false, cloudErr(err)
			}
			h.FirewallID, h.Egress = "", nil
		}
		t.to(h, StateAbsent, "", "destroyed")
		return true, nil
	}
	return false, fmt.Errorf("cloudvm: unknown destroying step %q", h.Step)
}
