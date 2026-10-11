package wardogsim

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gravel-project/gravel/games/wardogs"
)

// The server's own rules (games/wardogs/doc.go), kept as the live server keeps them.
const (
	maxBody        = 64 << 10
	perMinute      = 600
	strikesToLock  = 3
	lockFor        = time.Minute
	maxMessage     = 256
	auditRing      = 500
	defaultAuditN  = 50
	shapeHealth    = "GET /v1/health"
	shapeCaps      = "GET /v1/capabilities"
	peerIdle       = 10 * time.Minute
	zeroTimeString = "0001-01-01T00:00:00.000Z"
)

// peer is one client address: its failed-token strikes, its lock, and its request count this
// minute.
type peer struct {
	strikes []time.Time
	locked  time.Time
	minute  time.Time
	count   int
	seen    time.Time
}

// reply is a handler's answer: a status and either a value to encode or raw JSON.
type reply struct {
	status int
	value  any
	raw    []byte
	header http.Header
}

func ok(v any) reply { return reply{status: http.StatusOK, value: v} }

func fail(status int, code, message string) reply {
	return reply{status: status, value: map[string]any{"error": map[string]string{"code": code, "message": message}}}
}

// request is what a handler gets: the request, its route parameters and its body.
type request struct {
	r      *http.Request
	params []string
	body   []byte
	now    time.Time
}

// handler answers one route; it runs with the server's lock held and the world moved on to now.
type handler func(*Server, request) reply

// handlers are the routes the simulator answers, by shape; every route a recorded build
// advertises must be here (the tests check).
var handlers = map[string]handler{
	"GET /v1/health":                      (*Server).health,
	"GET /v1/status":                      (*Server).status,
	"GET /v1/players":                     (*Server).players,
	"GET /v1/rotation":                    (*Server).rotation,
	"GET /v1/bans":                        (*Server).bans,
	"GET /v1/reserved-slots":              (*Server).reservedSlots,
	"GET /v1/server-id":                   file("v1/server-id.json"),
	"GET /v1/sponsor":                     (*Server).sponsor,
	"GET /v1/audit":                       (*Server).auditLog,
	"GET /v1/catalog/maps":                file("v1/catalog/maps.json"),
	"GET /v1/catalog/lightings":           file("v1/catalog/lightings.json"),
	"GET /v1/catalog/experiences":         file("v1/catalog/experiences.json"),
	"GET /v1/catalog/maps/{}/experiences": mapFile("experiences"),
	"GET /v1/catalog/maps/{}/alternators": mapFile("alternators"),
	"GET /v1/config":                      (*Server).config,
	"POST /v1/config/validate":            (*Server).validate,
	"PUT /v1/config":                      (*Server).putConfig,
	"POST /v1/players/{}/kick":            (*Server).kick,
	"POST /v1/players/{}/kill":            (*Server).kill,
	"POST /v1/players/{}/message":         (*Server).message,
	"PATCH /v1/players/{}":                (*Server).move,
	"POST /v1/broadcast":                  (*Server).broadcast,
	"POST /v1/bans":                       (*Server).ban,
	"DELETE /v1/bans/{}":                  (*Server).unban,
	"POST /v1/match/end":                  (*Server).endMatchRoute,
	"POST /v1/match/restart":              (*Server).restartMatch,
	"POST /v1/match/map":                  (*Server).changeMap,
	"PUT /v1/world/lighting":              (*Server).setLighting,
	"GET /v1/capabilities":                func(s *Server, _ request) reply { return reply{status: http.StatusOK, raw: s.caps} },
}

// Handler is the RCON API.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serve) }

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	addr := peerAddr(r.RemoteAddr)
	shape, params := s.route(r.Method, r.URL.EscapedPath())
	public := shape == shapeHealth || shape == shapeCaps

	s.mu.Lock()
	s.inFlight++
	p := s.peer(addr, now)
	rep, authEvent := s.admit(p, r, public, now)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}()

	if rep.status == 0 {
		rep = s.dispatch(r, shape, params, now)
	}
	s.mu.Lock()
	if authEvent != "" {
		s.record(now, r, authEvent, "")
	}
	s.record(now, r, "HTTP", r.Method+" "+r.URL.Path+" -> "+strconv.Itoa(rep.status))
	s.mu.Unlock()
	write(w, rep)
}

// admit applies the rate limit and, on a protected route, the token and the three-strike lock.
// A zero reply lets the request through; authEvent is the admin log's word for the token check.
func (s *Server) admit(p *peer, r *http.Request, public bool, now time.Time) (reply, string) {
	if now.Sub(p.minute) >= time.Minute {
		p.minute, p.count = now, 0
	}
	p.count++
	if p.count > perMinute {
		rep := fail(http.StatusTooManyRequests, "rate_limited", "too many requests from this address")
		rep.header = http.Header{"Retry-After": {strconv.Itoa(retrySeconds(p.minute.Add(time.Minute).Sub(now)))}}
		return rep, ""
	}
	if public {
		return reply{}, ""
	}
	if now.Before(p.locked) {
		rep := fail(http.StatusTooManyRequests, "rate_limited", "too many failed authentication attempts")
		rep.header = http.Header{"Retry-After": {strconv.Itoa(retrySeconds(p.locked.Sub(now)))}}
		return rep, "AUTH_LOCKED"
	}
	auth := r.Header.Get("Authorization")
	token, bearer := strings.CutPrefix(auth, "Bearer ")
	if bearer && tokenEqual(token, s.opt.Token) {
		return reply{}, "AUTH_OK"
	}
	kept := p.strikes[:0]
	for _, at := range p.strikes {
		if now.Sub(at) < lockFor {
			kept = append(kept, at)
		}
	}
	p.strikes = append(kept, now)
	if len(p.strikes) >= strikesToLock {
		p.locked, p.strikes = now.Add(lockFor), nil
		rep := fail(http.StatusTooManyRequests, "rate_limited", "too many failed authentication attempts")
		rep.header = http.Header{"Retry-After": {strconv.Itoa(retrySeconds(lockFor))}}
		return rep, "AUTH_FAIL"
	}
	if auth == "" {
		return fail(http.StatusUnauthorized, "credential_missing", "a bearer token is required"), "AUTH_FAIL"
	}
	return fail(http.StatusUnauthorized, "credential_invalid", "the bearer token is not valid"), "AUTH_FAIL"
}

// tokenEqual compares in time that does not depend on where the strings differ.
func tokenEqual(got, want string) bool {
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// retrySeconds is a Retry-After in whole seconds, 1–60 as the server's are.
func retrySeconds(d time.Duration) int {
	return min(60, max(1, int(math.Ceil(d.Seconds()))))
}

// dispatch reads the body and runs the route's handler.
func (s *Server) dispatch(r *http.Request, shape string, params []string, now time.Time) reply {
	h, found := handlers[shape]
	if shape == "" || !found {
		return fail(http.StatusNotFound, "not_found", "no such route")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return fail(http.StatusBadRequest, "invalid_body", "the body could not be read")
	}
	if len(body) > maxBody {
		return fail(http.StatusRequestEntityTooLarge, "payload_too_large", "the body is over 65536 bytes")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advance(now)
	return h(s, request{r: r, params: params, body: body, now: now})
}

func write(w http.ResponseWriter, rep reply) {
	data := rep.raw
	if data == nil {
		// As the server writes it: indented, and "->" in the admin log as it is, not >.
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep.value); err != nil {
			rep.status, data = http.StatusInternalServerError, []byte(`{"error":{"code":"internal","message":"encoding failed"}}`)
		} else {
			data = bytes.TrimRight(buf.Bytes(), "\n")
		}
	}
	for k, vs := range rep.header {
		w.Header()[k] = vs
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rep.status)
	_, _ = w.Write(data)
}

// route is the advertised shape a request matches, with its parameters, or "".
func (s *Server) route(method, escapedPath string) (string, []string) {
	segs := strings.Split(strings.Trim(escapedPath, "/"), "/")
	for sh := range s.routes {
		r, err := wardogs.ParseRoute(sh)
		if err != nil || r.Method != method || len(r.Segments) != len(segs) {
			continue
		}
		var params []string
		match := true
		for i, seg := range r.Segments {
			if seg == "{}" {
				v, err := url.PathUnescape(segs[i])
				if err != nil || v == "" {
					match = false
					break
				}
				params = append(params, v)
			} else if seg != segs[i] {
				match = false
				break
			}
		}
		if match {
			return sh, params
		}
	}
	return "", nil
}

func (s *Server) peer(addr string, now time.Time) *peer {
	p := s.auth[addr]
	if p == nil {
		p = &peer{}
		s.auth[addr] = p
	}
	p.seen = now
	return p
}

// prunePeers forgets addresses idle for a while.
func (s *Server) prunePeers(now time.Time) {
	for a, p := range s.auth {
		if now.Sub(p.seen) > peerIdle && now.After(p.locked) {
			delete(s.auth, a)
		}
	}
}

func peerAddr(remote string) string {
	if host, _, err := net.SplitHostPort(remote); err == nil {
		return host
	}
	return remote
}

// record adds a line to the admin log, a ring of the newest 500.
func (s *Server) record(now time.Time, r *http.Request, event, detail string) {
	sum := sha256.Sum256([]byte(r.RemoteAddr + "\x00" + s.build))
	s.audit = append(s.audit, wardogs.AuditEntry{TimestampUTC: now.UTC().Format("2006-01-02T15:04:05.000Z"), Peer: r.RemoteAddr,
		SessionID: base64.RawURLEncoding.EncodeToString(sum[:16]), Event: event, Detail: detail})
	if n := len(s.audit); n > auditRing {
		s.audit = s.audit[n-auditRing:]
	}
}

// file answers with a recorded file as it is.
func file(name string) handler {
	return func(s *Server, _ request) reply {
		b, err := fs.ReadFile(s.files, name)
		if err != nil {
			return fail(http.StatusNotFound, "not_found", "not recorded")
		}
		return reply{status: http.StatusOK, raw: b}
	}
}

// mapFile answers with a map's recorded catalog file.
func mapFile(what string) handler {
	return func(s *Server, q request) reply {
		id := q.params[0]
		if !s.maps[id] || strings.ContainsAny(id, "/\\.") {
			return fail(http.StatusNotFound, "map_not_found", "no such map")
		}
		return file("v1/catalog/maps/"+id+"/"+what+".json")(s, q)
	}
}

func (s *Server) health(q request) reply {
	return ok(map[string]any{
		"connections":     map[string]any{"active": s.inFlight},
		"gameThreadQueue": map[string]any{"depth": 32, "inFlight": 0, "rejectedTotal": 0},
		"status":          "ok",
		"uptimeSeconds":   int64(q.now.Sub(s.booted).Seconds()),
	})
}

func (s *Server) status(_ request) reply {
	scores := make([]map[string]any, len(s.factions))
	for i, f := range s.factions {
		var score int64
		if i < len(s.match.scores) {
			score = s.match.scores[i]
		}
		scores[i] = map[string]any{"colorHex": f.ColorHex, "name": f.Name, "score": score}
	}
	exps := s.match.entry.Experiences
	if exps == nil {
		exps = []string{}
	}
	return ok(map[string]any{
		"alternator":    s.match.entry.Alternator,
		"experiences":   exps,
		"factionScores": scores,
		"lighting":      s.lighting,
		"map":           s.match.entry.level(),
		"players":       map[string]any{"current": len(s.online), "max": s.capacity()},
		"rotation":      map[string]any{"nextIndex": s.match.next, "nowIndex": s.match.index},
		"scoreTick":     map[string]any{"current": s.match.scorePeriod, "max": scorePeriodMax, "min": scorePeriodMin},
		"serverName":    s.written.serverName,
	})
}

func (s *Server) players(_ request) reply {
	list := make([]map[string]any, 0, len(s.online))
	for _, p := range s.online {
		list = append(list, map[string]any{"cash": p.cash, "deaths": p.deaths, "faction": s.factions[p.faction].Name,
			"kills": p.kills, "name": p.Name, "pingMs": p.PingMs, "steamId": p.SteamID})
	}
	return ok(map[string]any{"count": len(list), "players": list})
}

func (s *Server) rotation(_ request) reply {
	entries := make([]map[string]any, 0, len(s.written.rotation))
	for i, e := range s.written.rotation {
		var status any
		switch i {
		case s.match.index:
			status = "now"
		case s.match.next:
			status = "next"
		}
		exps := e.Experiences
		if exps == nil {
			exps = []string{}
		}
		entries = append(entries, map[string]any{"denied": !s.maps[e.Map], "experiences": exps, "index": i, "lighting": e.Lighting,
			"map": e.Map, "status": status, "zoneAlternator": e.Alternator})
	}
	mode := "ordered"
	if s.written.rotationRandom {
		mode = "random"
	}
	return ok(map[string]any{"count": len(entries), "enabled": s.written.rotationOn, "entries": entries, "mode": mode})
}

func (s *Server) bans(_ request) reply {
	list := []map[string]any{}
	for _, id := range s.written.banned {
		list = append(list, map[string]any{"bannedAtUtc": zeroTimeString, "bannedBy": "config", "reason": nil, "steamId": id})
	}
	for _, b := range s.rconBans {
		var reason any
		if b.reason != "" {
			reason = b.reason
		}
		list = append(list, map[string]any{"bannedAtUtc": b.at.UTC().Format("2006-01-02T15:04:05.000Z"), "bannedBy": "rcon", "reason": reason, "steamId": b.steamID})
	}
	return ok(map[string]any{"bans": list, "count": len(list)})
}

func (s *Server) reservedSlots(_ request) reply {
	ids := s.written.reserved
	if ids == nil {
		ids = []string{}
	}
	return ok(map[string]any{"count": len(ids), "reservedSlots": ids})
}

func (s *Server) sponsor(_ request) reply {
	return ok(map[string]any{"imageUrl": s.written.imageURL})
}

func (s *Server) auditLog(q request) reply {
	limit := defaultAuditN
	if v := q.r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > auditRing {
			return fail(http.StatusBadRequest, "invalid_limit", "limit must be 1–500")
		}
		limit = n
	}
	entries := s.audit[max(0, len(s.audit)-limit):]
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		var detail any
		if e.Detail != "" {
			detail = e.Detail
		}
		out = append(out, map[string]any{"detail": detail, "event": e.Event, "peer": e.Peer, "sessionId": e.SessionID, "timestampUtc": e.TimestampUTC})
	}
	return ok(map[string]any{"count": len(out), "entries": out, "limit": limit})
}

func (s *Server) config(_ request) reply {
	return ok(map[string]any{"revision": s.revision, "sections": s.rawSchema, "text": s.doc.String(), "warnings": []string{}, "writable": true})
}

// result is a validate's or a write's answer, in the server's shape.
func result(okay bool, revision string, c checked, changed []wardogs.SectionChange, outcomes []wardogs.Outcome) map[string]any {
	nonNil := func(v any) any {
		switch x := v.(type) {
		case []wardogs.KeyError:
			if x == nil {
				return []wardogs.KeyError{}
			}
		case []wardogs.Stripped:
			if x == nil {
				return []wardogs.Stripped{}
			}
		case []string:
			if x == nil {
				return []string{}
			}
		case []wardogs.SectionChange:
			if x == nil {
				return []wardogs.SectionChange{}
			}
		case []wardogs.Outcome:
			if x == nil {
				return []wardogs.Outcome{}
			}
		}
		return v
	}
	return map[string]any{"ok": okay, "revision": revision, "errors": nonNil(c.errors), "stripped": nonNil(c.stripped),
		"warnings": nonNil(c.warnings), "changed": nonNil(changed), "outcomes": nonNil(outcomes), "shadowed": []any{}, "conflict": []any{}}
}

func (s *Server) validate(q request) reply {
	c := s.check(string(q.body), s.doc)
	if len(c.errors) > 0 {
		return reply{status: http.StatusUnprocessableEntity, value: result(false, s.revision, c, nil, nil)}
	}
	changed := changes(s.doc, c.doc)
	return ok(result(true, s.revision, c, changed, s.outcomes(changed)))
}

func (s *Server) putConfig(q request) reply {
	match := strings.Trim(strings.TrimSpace(q.r.Header.Get("If-Match")), `"`)
	if match == "" {
		return fail(http.StatusPreconditionRequired, "precondition_required", "a write needs If-Match with the revision it is based on")
	}
	if match != s.revision {
		v := result(false, s.revision, checked{}, nil, nil)
		v["error"] = map[string]string{"code": "revision_mismatch", "message": "the document changed since revision " + match}
		return reply{status: http.StatusPreconditionFailed, value: v}
	}
	c := s.check(string(q.body), s.doc)
	if len(c.errors) > 0 {
		return reply{status: http.StatusUnprocessableEntity, value: result(false, s.revision, c, nil, nil)}
	}
	changed := changes(s.doc, c.doc)
	outcomes := s.outcomes(changed)
	if len(changed) > 0 {
		before := s.written.rotation
		s.setDoc(c.doc)
		// The rotation is rebuilt at once and used from the next map change.
		if !rotationsEqual(before, s.written.rotation) {
			s.match.next = s.nextIndex(min(s.match.index, max(0, len(s.written.rotation)-1)))
		}
	}
	return ok(result(true, s.revision, c, changed, outcomes))
}

func rotationsEqual(a, b []rotationEntry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Map != b[i].Map || a[i].Alternator != b[i].Alternator || a[i].Lighting != b[i].Lighting || strings.Join(a[i].Experiences, ",") != strings.Join(b[i].Experiences, ",") {
			return false
		}
	}
	return true
}

// decode reads a JSON body into out; an empty body is an empty object.
func decode(q request, out any) bool {
	if len(strings.TrimSpace(string(q.body))) == 0 {
		return true
	}
	return json.Unmarshal(q.body, out) == nil
}

var okBody = map[string]any{"ok": true}

func (s *Server) kick(q request) reply {
	var in struct {
		Reason string `json:"reason"`
	}
	if !decode(q, &in) {
		return fail(http.StatusBadRequest, "invalid_body", "the body is not JSON")
	}
	p := s.find(q.params[0])
	if p == nil {
		return fail(http.StatusNotFound, "player_not_found", "no such player is connected")
	}
	s.leave(p)
	s.away[p.N] = q.now.Add(time.Duration(5+s.rng.IntN(25)) * time.Minute)
	return ok(okBody)
}

func (s *Server) kill(q request) reply {
	p := s.find(q.params[0])
	if p == nil {
		return fail(http.StatusNotFound, "player_not_found", "no such player is connected")
	}
	p.deaths++
	s.queue(q.now, "", p.SteamID)
	return ok(okBody)
}

// text reads {"message": …} under the server's length cap.
func text(q request) (string, *reply) {
	var in struct {
		Message string `json:"message"`
	}
	if !decode(q, &in) {
		r := fail(http.StatusBadRequest, "invalid_body", "the body is not JSON")
		return "", &r
	}
	if strings.TrimSpace(in.Message) == "" {
		r := fail(http.StatusBadRequest, "invalid_body", "message is required")
		return "", &r
	}
	if utf8.RuneCountInString(in.Message) > maxMessage {
		r := fail(http.StatusBadRequest, "message_too_long", "the message is over 256 characters")
		return "", &r
	}
	return in.Message, nil
}

func (s *Server) message(q request) reply {
	if _, bad := text(q); bad != nil {
		return *bad
	}
	if s.find(q.params[0]) == nil {
		return fail(http.StatusNotFound, "player_not_found", "no such player is connected")
	}
	return ok(okBody)
}

func (s *Server) broadcast(q request) reply {
	msg, bad := text(q)
	if bad != nil {
		return *bad
	}
	return ok(map[string]any{"message": msg, "ok": true, "pending": false})
}

func (s *Server) move(q request) reply {
	var in struct {
		Faction string `json:"faction"`
	}
	if !decode(q, &in) {
		return fail(http.StatusBadRequest, "invalid_body", "the body is not JSON")
	}
	p := s.find(q.params[0])
	if p == nil {
		return fail(http.StatusNotFound, "player_not_found", "no such player is connected")
	}
	for i, f := range s.factions {
		if strings.EqualFold(f.Name, in.Faction) {
			p.faction = i
			return ok(okBody)
		}
	}
	return fail(http.StatusBadRequest, "invalid_faction", "no such faction")
}

func (s *Server) ban(q request) reply {
	var in struct {
		SteamID wardogs.SteamID `json:"steamId"`
		Reason  string          `json:"reason"`
	}
	if !decode(q, &in) || in.SteamID == "" {
		return fail(http.StatusBadRequest, "invalid_body", "steamId is required")
	}
	id := string(in.SteamID)
	p := s.find(id)
	if p == nil {
		return fail(http.StatusNotFound, "player_not_found", "no such player is connected")
	}
	for _, b := range s.rconBans {
		if b.steamID == id {
			return fail(http.StatusConflict, "already_banned", "the player is banned")
		}
	}
	s.rconBans = append(s.rconBans, ban{steamID: id, reason: in.Reason, at: q.now})
	s.leave(p)
	return ok(okBody)
}

func (s *Server) unban(q request) reply {
	for i, b := range s.rconBans {
		if b.steamID == q.params[0] {
			s.rconBans = append(s.rconBans[:i], s.rconBans[i+1:]...)
			return ok(okBody)
		}
	}
	return fail(http.StatusNotFound, "ban_not_found", "no RCON ban for that player")
}

func (s *Server) endMatchRoute(q request) reply {
	s.endMatch(q.now)
	return ok(okBody)
}

func (s *Server) restartMatch(q request) reply {
	s.match.playing = false
	for _, p := range s.online {
		p.kills, p.deaths = 0, 0
	}
	s.match.scores = make([]int64, len(s.factions))
	s.startIfReady(q.now)
	return ok(okBody)
}

// changeMap ends the match on a rotation entry: {"index": n}, or the first entry of {"map": id}.
func (s *Server) changeMap(q request) reply {
	var in struct {
		Index *int   `json:"index"`
		Map   string `json:"map"`
	}
	if !decode(q, &in) || (in.Index == nil && in.Map == "") {
		return fail(http.StatusBadRequest, "invalid_body", "index or map is required")
	}
	idx := -1
	if in.Index != nil && *in.Index >= 0 && *in.Index < len(s.written.rotation) {
		idx = *in.Index
	}
	for i, e := range s.written.rotation {
		if idx < 0 && in.Map != "" && e.Map == in.Map {
			idx = i
		}
	}
	if idx < 0 {
		return fail(http.StatusBadRequest, "not_in_rotation", "the rotation has no such entry")
	}
	s.match.next = idx
	s.endMatch(q.now)
	return ok(okBody)
}

func (s *Server) setLighting(q request) reply {
	var in struct {
		Lighting string `json:"lighting"`
	}
	if !decode(q, &in) || !s.lightings[in.Lighting] {
		return fail(http.StatusBadRequest, "unknown_lighting", "no such lighting preset")
	}
	s.lighting = in.Lighting
	return ok(okBody)
}
