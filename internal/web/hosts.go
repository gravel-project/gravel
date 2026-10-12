package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/internal/web/templates"
)

// A cloud-VM server's machine on its page (ADR-0003): the owner and moderators see its state and
// cost and can park and start it; the owner can also resize and destroy it. Every request goes
// through a confirmation page and the hub's ServerHostService, which decides who may.

// hostStateLabels are what a reader sees for a host's state.
var hostStateLabels = map[string]string{
	"absent": "Not made", "creating": "Being made", "booting": "Booting", "starting": "Starting the game",
	"running": "Running", "parking": "Parking", "parked": "Parked", "resizing": "Resizing", "destroying": "Being destroyed",
}

// hostPanel is the machine as the server page shows it; nil when the reader may not see it or the
// server has none.
func (h *Handler) hostPanel(r *http.Request, serverID string, owner bool) *templates.HostPanel {
	ctx, _ := withBrowser(r)
	resp, err := h.hostAPI.GetServerHost(ctx, connect.NewRequest(&hubv1.GetServerHostRequest{ServerId: serverID}))
	if err != nil {
		return nil
	}
	x := resp.Msg.GetHost()
	p := &templates.HostPanel{State: x.GetState(), StateLabel: hostStateLabels[x.GetState()], Step: x.GetStep(), ErrorCode: x.GetErrorCode(),
		Size: x.GetSize(), TargetSize: x.GetTargetSize(), Address: x.GetAddress(), Reserved: x.GetAddressReserved(),
		HourlyUSD: x.GetHourlyPriceUsd(), MonthUSD: x.GetMonthCostUsd(), HoursMonth: float64(x.GetRunningSecondsMonth()) / 3600,
		BootSeconds: int(x.GetBootSeconds()), ReadySeconds: int(x.GetReadySeconds()), ParkAfter: time.Duration(x.GetParkAfterSeconds()) * time.Second}
	if p.StateLabel == "" {
		p.StateLabel = x.GetState()
	}
	if t := x.GetSince(); t != nil {
		p.Since = t.AsTime()
	}
	p.Scaling, p.TierPlayers, p.Pinned = x.GetScaling(), int(x.GetTierPlayers()), x.GetPinnedSize()
	p.PendingSize, p.PendingReason = x.GetPendingSize(), x.GetPendingReason()
	if t := x.GetPendingSince(); t != nil {
		p.PendingSince = t.AsTime()
	}
	for _, r := range x.GetLadder() {
		p.Ladder = append(p.Ladder, templates.HostRung{Size: r.GetSize(), Players: int(r.GetPlayers()), PriceUSD: r.GetPriceHourlyUsd(), Current: r.GetSize() == x.GetSize()})
	}
	resting := x.GetState() == "running" || x.GetState() == "parked" || x.GetState() == "absent"
	p.CanPark = x.GetDesired() == "running"
	p.CanUnpark = x.GetDesired() != "running" && resting
	p.CanResize = owner && x.GetState() == "running" && x.GetTargetSize() == ""
	p.CanDestroy = owner && (x.GetState() == "parked" || x.GetState() == "absent") && x.GetDesired() != "absent"
	p.CanPin = owner && x.GetState() == "running" && x.GetTargetSize() == "" && x.GetPinnedSize() == "" && x.GetScaling()
	p.CanUnpin = owner && x.GetPinnedSize() != ""
	p.CanSnapshot = owner && x.GetState() == "parked"
	for _, s := range x.GetSizes() {
		p.Sizes = append(p.Sizes, templates.HostSizeOption{Slug: s.GetSlug(),
			Label: fmt.Sprintf("%s · %d vCPU · %d GB · $%.3f/h", s.GetSlug(), s.GetVcpus(), s.GetMemoryMib()/1024, s.GetPriceHourlyUsd())})
	}
	return p
}

// hostQuestions are the confirmation's question and warning per action.
var hostQuestions = map[string][2]string{
	"park":     {"Park %s?", "Everyone on is warned, then disconnected. The machine is destroyed; its disk and address stay, so starting it again takes a few minutes."},
	"unpark":   {"Start %s?", "The machine is made again on its disk and the game starts; it takes a few minutes."},
	"resize":   {"Resize %s to %s?", "The game stops, the machine is resized and started again: everyone on is disconnected for a few minutes."},
	"pin":      {"Keep %s at %s?", "The scaling policy stops resizing it, and after a park it comes back to this size, until you let the policy decide again."},
	"unpin":    {"Let the policy size %s again?", "The scaling policy decides its size from who is on, at the next map change or when it is empty."},
	"snapshot": {"Snapshot %s's disk?", "The parked machine's disk, with the game's install, is copied into a snapshot a new server's disk can start from (servers.yaml vm.seed_snapshot). It bills by the gigabyte every month until it is deleted, and it holds this server's settings and secrets too."},
	"destroy":  {"Destroy %s's machine?", "The parked machine's leftovers go. Its disk with the game's install and its address stay unless you tick the box."},
}

// host asks for confirmation, then sends a host request through the API.
func (h *Handler) host(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	id := r.PathValue("id")
	action := r.PostFormValue("action")
	q, ok := hostQuestions[action]
	if !ok {
		h.fail(w, r, http.StatusBadRequest, "No such action", "That is not something the hub can do to a server's machine.")
		return
	}
	size := strings.TrimSpace(r.PostFormValue("size"))
	back := "/servers/" + id
	ctx, b := withBrowser(r)
	status, err := h.serverAPI.GetServerStatus(ctx, connect.NewRequest(&hubv1.GetServerStatusRequest{ServerId: id}))
	if err != nil {
		h.fail(w, r, http.StatusNotFound, "No such server", "This hub has no server by that name.")
		return
	}
	serverName, gameID := status.Msg.GetServer().GetName(), status.Msg.GetServer().GetGameId()

	if r.PostFormValue("confirm") != "1" {
		p, b2, err := h.page(r, "Confirm")
		if err != nil {
			h.serverError(w, r, err)
			return
		}
		withGame(&p.Page, gameID, h.gameName(r, gameID))
		cp := templates.HostConfirmPage{Page: p.Page, ServerID: id, ServerName: serverName, Action: action, Size: size, Warning: q[1],
			OfferForce: action == "resize", OfferPin: action == "resize" && r.PostFormValue("scaling") == "1", OfferDeleteVolume: action == "destroy"}
		if action == "resize" || action == "pin" {
			cp.Question = fmt.Sprintf(q[0], serverName, size)
		} else {
			cp.Question = fmt.Sprintf(q[0], serverName)
		}
		h.render(w, r, http.StatusOK, b2, templates.HostConfirm(cp), templates.HostConfirmContent(cp))
		return
	}

	snapshotName := ""
	switch action {
	case "park", "unpark":
		desired := map[string]string{"park": "parked", "unpark": "running"}[action]
		_, err = h.hostAPI.SetServerHostDesired(ctx, connect.NewRequest(&hubv1.SetServerHostDesiredRequest{ServerId: id, Desired: desired}))
	case "resize":
		_, err = h.hostAPI.ResizeServerHost(ctx, connect.NewRequest(&hubv1.ResizeServerHostRequest{ServerId: id, Size: size, Force: r.PostFormValue("force") == "1", Pin: r.PostFormValue("pin") == "1"}))
	case "pin":
		_, err = h.hostAPI.ResizeServerHost(ctx, connect.NewRequest(&hubv1.ResizeServerHostRequest{ServerId: id, Size: size, Pin: true}))
	case "unpin":
		_, err = h.hostAPI.UnpinServerHostSize(ctx, connect.NewRequest(&hubv1.UnpinServerHostSizeRequest{ServerId: id}))
	case "snapshot":
		var resp *connect.Response[hubv1.SnapshotServerHostVolumeResponse]
		resp, err = h.hostAPI.SnapshotServerHostVolume(ctx, connect.NewRequest(&hubv1.SnapshotServerHostVolumeRequest{ServerId: id}))
		if err == nil {
			snapshotName = resp.Msg.GetSnapshot().GetName()
		}
	case "destroy":
		del := r.PostFormValue("delete_volume") == "1"
		_, err = h.hostAPI.DestroyServerHost(ctx, connect.NewRequest(&hubv1.DestroyServerHostRequest{ServerId: id, DeleteVolume: del, ReleaseAddress: del}))
	}
	switch connect.CodeOf(err) {
	case connect.CodeUnauthenticated:
		h.apiError(w, r, b, err)
		return
	case connect.CodePermissionDenied:
		b.relay(w)
		h.fail(w, r, http.StatusForbidden, "Not yours to do", "Moderators can park and start a server; only the hub's owner can resize, pin, snapshot or destroy its machine.")
		return
	case connect.CodeNotFound, connect.CodeInvalidArgument, connect.CodeFailedPrecondition, connect.CodeUnavailable:
		h.setFlash(w, "err", "Nothing done: "+connectMessage(err)+".")
	default:
		if err != nil {
			h.apiError(w, r, b, err)
			return
		}
		h.setFlash(w, "ok", map[string]string{"park": "Parking the server.", "unpark": "Starting the server; it takes a few minutes.",
			"resize": "Resizing the server.", "pin": "The size is pinned.", "unpin": "The scaling policy decides the size again.",
			"snapshot": "Snapshot " + snapshotName + " made.", "destroy": "Destroying the machine."}[action])
	}
	b.relay(w)
	h.redirect(w, r, back)
}
