package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
)

// followEvery is how often `gravel events --follow` asks for what is new (ADR-0028: no push in
// core; a client polls the log by cursor).
var followEvery = 5 * time.Second

// now is the clock `events --since` counts back from; tests replace it.
var now = time.Now

// fleetCmd prints the fleet as GetFleetSummary has it: the totals, the open alarms, then each
// server, trouble first. It exits 1 when any server is down, so a script or a cron can alert.
func fleetCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("fleet", stderr)
	hf := addHubFlag(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: gravel fleet [--hub URL]")
		return exitUsage
	}
	c, err := openHub(hf)
	if err != nil {
		fmt.Fprintln(stderr, "gravel fleet:", err)
		return exitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	res, err := c.servers().GetFleetSummary(ctx, connect.NewRequest(&hubv1.GetFleetSummaryRequest{}))
	if err != nil {
		return c.fail(stderr, "fleet", err)
	}
	f := res.Msg
	t := f.GetTotals()
	fmt.Fprintf(stdout, "%s as of %s\n", c.hub, stamp(f.GetAsOf()))
	fmt.Fprintf(stdout, "%d servers: %d up, %d degraded, %d down, %d idle, %d unknown; %d of %d players\n",
		t.GetServers(), t.GetUp(), t.GetDegraded(), t.GetDown(), t.GetIdle(), t.GetUnknown(), t.GetPlayers(), t.GetCapacity())
	if len(f.GetAlarms()) > 0 {
		fmt.Fprintln(stdout)
		tw := table(stdout)
		fmt.Fprintln(tw, "ALARM\tSEVERITY\tSERVER\tSINCE\tDETAIL")
		for _, a := range f.GetAlarms() {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.GetKind(), a.GetSeverity(), dash(a.GetServerId()), stamp(a.GetSince()), a.GetDetail())
		}
		if code := flush(tw, stderr); code != exitOK {
			return code
		}
	}
	if len(f.GetServers()) > 0 {
		fmt.Fprintln(stdout)
		tw := table(stdout)
		fmt.Fprintln(tw, "SERVER\tGAME\tHEALTH\tSINCE\tPLAYERS\tMAP\tREASON")
		for _, s := range f.GetServers() {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.GetId(), s.GetGameId(), s.GetHealth(), stamp(s.GetHealthSince()),
				players(s.GetPlayers(), s.GetMaxPlayers(), s.GetObservedAt() != nil), dash(s.GetMap()), s.GetReason())
		}
		if code := flush(tw, stderr); code != exitOK {
			return code
		}
	}
	if t.GetDown() > 0 {
		return exitFailed
	}
	return exitOK
}

func serversUsage(w io.Writer) {
	fmt.Fprint(w, `usage: gravel servers <command> [--hub URL]

  list            every server: its game, where it runs, its state and players
  status SERVER   one server's last observation
`)
}

func serversCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		serversUsage(stderr)
		return exitUsage
	}
	switch args[0] {
	case "list":
		return serversList(ctx, args[1:], stdout, stderr)
	case "status":
		return serversStatus(ctx, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		serversUsage(stdout)
		return exitOK
	}
	fmt.Fprintf(stderr, "gravel servers: unknown command %q\n", args[0])
	serversUsage(stderr)
	return exitUsage
}

func serversList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("servers list", stderr)
	hf := addHubFlag(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: gravel servers list [--hub URL]")
		return exitUsage
	}
	c, err := openHub(hf)
	if err != nil {
		fmt.Fprintln(stderr, "gravel servers list:", err)
		return exitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	res, err := c.servers().ListServers(ctx, connect.NewRequest(&hubv1.ListServersRequest{}))
	if err != nil {
		return c.fail(stderr, "servers list", err)
	}
	tw := table(stdout)
	fmt.Fprintln(tw, "SERVER\tNAME\tGAME\tLOCATION\tDRIVER\tSTATE\tPLAYERS\tMAP")
	for _, s := range res.Msg.GetServers() {
		st := s.GetStatus()
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.GetId(), s.GetName(), s.GetGameId(), dash(s.GetLocation()), s.GetDriver(),
			st.GetState(), players(st.GetPlayers(), st.GetMaxPlayers(), st.GetObservedAt() != nil), dash(st.GetMap()))
	}
	return flush(tw, stderr)
}

func serversStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("servers status", stderr)
	hf := addHubFlag(fs)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: gravel servers status [--hub URL] SERVER")
		return exitUsage
	}
	c, err := openHub(hf)
	if err != nil {
		fmt.Fprintln(stderr, "gravel servers status:", err)
		return exitFailed
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	res, err := c.servers().GetServerStatus(ctx, connect.NewRequest(&hubv1.GetServerStatusRequest{ServerId: fs.Arg(0)}))
	if connect.CodeOf(err) == connect.CodeNotFound {
		fmt.Fprintf(stderr, "gravel servers status: %s has no server %q\n", c.hub, fs.Arg(0))
		return exitFailed
	}
	if err != nil {
		return c.fail(stderr, "servers status", err)
	}
	s := res.Msg.GetServer()
	st := s.GetStatus()
	tw := table(stdout)
	fmt.Fprintf(tw, "server\t%s (%s)\n", s.GetId(), s.GetName())
	fmt.Fprintf(tw, "game\t%s\n", s.GetGameId())
	fmt.Fprintf(tw, "location\t%s\n", dash(s.GetLocation()))
	fmt.Fprintf(tw, "driver\t%s\n", s.GetDriver())
	fmt.Fprintf(tw, "trust\t%s\n", s.GetTrust())
	fmt.Fprintf(tw, "state\t%s\n", st.GetState())
	fmt.Fprintf(tw, "reachable\t%s\n", yesNo(st.GetReachable()))
	fmt.Fprintf(tw, "observed\t%s\n", stamp(st.GetObservedAt()))
	if st.GetObservedAt() != nil {
		fmt.Fprintf(tw, "build\t%s\n", dash(st.GetBuild()))
		fmt.Fprintf(tw, "players\t%s\n", players(st.GetPlayers(), st.GetMaxPlayers(), true))
		fmt.Fprintf(tw, "map\t%s\n", dash(st.GetMap()))
		if st.GetRotationMap() != "" || st.GetNextRotationMap() != "" {
			fmt.Fprintf(tw, "rotation\t%s, next %s\n", dash(st.GetRotationMap()), dash(st.GetNextRotationMap()))
		}
		for _, team := range st.GetTeams() {
			fmt.Fprintf(tw, "team\t%s %d\n", team.GetName(), team.GetScore())
		}
		fmt.Fprintf(tw, "can\t%s\n", orNone(st.GetCapabilities()))
	}
	if j := st.GetJoin(); j != nil {
		fmt.Fprintf(tw, "join\t%s %s (%s)\n", j.GetMethod(), j.GetValue(), j.GetHint())
		if j.GetPassword() != "" {
			fmt.Fprintf(tw, "join password\t%s\n", j.GetPassword())
		}
	}
	return flush(tw, stderr)
}

// eventsCmd prints the hub's events log (ADR-0028), oldest first: the newest --limit, or every
// one since --since; --follow then keeps printing what comes, from the log's head.
func eventsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("events", stderr)
	hf := addHubFlag(fs)
	follow := fs.Bool("follow", false, "keep printing new events, asking every 5 s, until interrupted")
	var kinds stringList
	fs.Var(&kinds, "kind", "a kind (server.down) or a group (server); repeatable")
	since := fs.String("since", "", "the events since a duration ago (1h, 30m) or a time (2026-10-14T02:00:00Z)")
	severity := fs.String("severity", "", "info, notice, warning or critical: this severity and above")
	server := fs.String("server", "", "one server's events")
	limit := fs.Int("limit", 20, "without --since, how many of the newest events to print first (0 for none)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 || *limit < 0 {
		fmt.Fprintln(stderr, "usage: gravel events [--follow] [--kind KIND]... [--since 1h|TIME] [--severity S] [--server ID] [--limit N] [--hub URL]")
		return exitUsage
	}
	var from time.Time
	if *since != "" {
		t, err := parseSince(*since)
		if err != nil {
			fmt.Fprintln(stderr, "gravel events:", err)
			return exitUsage
		}
		from = t
	}
	c, err := openHub(hf)
	if err != nil {
		fmt.Fprintln(stderr, "gravel events:", err)
		return exitFailed
	}
	q := &hubv1.ListEventsRequest{Kinds: kinds, MinSeverity: *severity, ServerId: *server}
	head, err := backlog(ctx, c, q, from, *limit, stdout)
	if err != nil {
		return c.fail(stderr, "events", err)
	}
	if !*follow {
		return exitOK
	}
	return followEvents(ctx, c, q, head, stdout, stderr)
}

// backlog prints the events before following, oldest first, and returns the log's head: those
// since from when it is set, else the newest limit. It pages back from the head.
func backlog(ctx context.Context, c *hubClient, q *hubv1.ListEventsRequest, from time.Time, limit int, w io.Writer) (int64, error) {
	var got []*hubv1.Event
	var head, before int64
	for {
		size := 500
		if from.IsZero() {
			size = max(1, min(limit-len(got), 500)) // 0 would mean 100
		}
		cctx, cancel := context.WithTimeout(ctx, requestTimeout)
		res, err := c.organization().ListEvents(cctx, connect.NewRequest(&hubv1.ListEventsRequest{
			BeforeId: before, Limit: int32(size), Kinds: q.Kinds, MinSeverity: q.MinSeverity, ServerId: q.ServerId, //nolint:gosec // at most 500
		}))
		cancel()
		if err != nil {
			return 0, err
		}
		if before == 0 {
			head = res.Msg.GetHeadId()
		}
		older := false
		for _, e := range res.Msg.GetEvents() { // newest first
			if !from.IsZero() && e.GetAt().AsTime().Before(from) {
				older = true
				break
			}
			if from.IsZero() && len(got) >= limit {
				break
			}
			got = append(got, e)
		}
		before = res.Msg.GetNextBeforeId()
		if older || before == 0 || (from.IsZero() && len(got) >= limit) {
			break
		}
	}
	slices.Reverse(got)
	for _, e := range got {
		printEvent(w, e)
	}
	return head, nil
}

// followEvents asks for the events after the cursor every followEvery, until ctx ends. A failed
// poll is reported and retried; a session that ended stops it.
func followEvents(ctx context.Context, c *hubClient, q *hubv1.ListEventsRequest, after int64, stdout, stderr io.Writer) int {
	tick := time.NewTicker(followEvery)
	defer tick.Stop()
	for {
		for { // drain: a busy log answers in pages of 500
			cctx, cancel := context.WithTimeout(ctx, requestTimeout)
			res, err := c.organization().ListEvents(cctx, connect.NewRequest(&hubv1.ListEventsRequest{
				AfterId: after, Limit: 500, Kinds: q.Kinds, MinSeverity: q.MinSeverity, ServerId: q.ServerId,
			}))
			cancel()
			if ctx.Err() != nil {
				return exitOK
			}
			if err != nil {
				if code := connect.CodeOf(err); code == connect.CodeUnauthenticated || code == connect.CodePermissionDenied {
					return c.fail(stderr, "events", err)
				}
				fmt.Fprintf(stderr, "gravel events: %s; trying again\n", message(err))
				break
			}
			events := res.Msg.GetEvents()
			if after == 0 { // the log was empty at the start: after_id 0 reads newest first
				slices.Reverse(events)
			}
			for _, e := range events {
				printEvent(stdout, e)
			}
			after = max(after, res.Msg.GetNextAfterId())
			if len(events) < 500 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return exitOK
		case <-tick.C:
		}
	}
}

// printEvent is one line: when, severity, kind, server, summary.
func printEvent(w io.Writer, e *hubv1.Event) {
	fmt.Fprintf(w, "%s  %-8s  %-30s  %-16s  %s\n", stamp(e.GetAt()), e.GetSeverity(), e.GetKind(), dash(e.GetServerId()), e.GetSummary())
}

// parseSince reads --since: a duration back from now, or an RFC 3339 time.
func parseSince(s string) (time.Time, error) {
	if d, err := time.ParseDuration(s); err == nil {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("--since %s: give a positive duration", s)
		}
		return now().Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("--since %q: give a duration (1h, 30m) or a time (2026-10-14T02:00:00Z)", s)
}

func table(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0) }

func flush(tw *tabwriter.Writer, stderr io.Writer) int {
	if err := tw.Flush(); err != nil {
		fmt.Fprintln(stderr, "gravel:", err)
		return exitFailed
	}
	return exitOK
}

// stamp is a time in UTC to the second; "-" when unset.
func stamp(t *timestamppb.Timestamp) string {
	if t == nil {
		return "-"
	}
	return t.AsTime().UTC().Format(time.RFC3339)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// players is "12/64", or "-" when the server has not answered.
func players(n, of int32, answered bool) string {
	if !answered {
		return "-"
	}
	return strconv.Itoa(int(n)) + "/" + strconv.Itoa(int(of))
}
