// Package dotest is a stateful emulator of the part of DigitalOcean's v2 API that
// cloud/digitalocean uses: Droplets and their actions, volumes and their actions, firewalls,
// reserved IPs and their actions, SSH keys, tags, projects, regions, sizes and actions. It
// answers in the JSON shapes of godo's own types, so the provider is exercised through the real
// SDK, and it enforces the rules the provider relies on: a bearer token, 404 for an unknown id,
// one volume name per region, a volume attaching only in its region, no resize to a smaller
// disk, and one pending action per Droplet.
//
// Actions finish lazily: one is "in-progress" when made and completes (its effect applied) on
// the first request after its delay, zero by default. A test injects faults with Fault (the next
// requests to a path answer with a status) and FailNextAction (the next action of a type ends
// "errored"), and WithReleaseLock models a reserved IP that will not go right after its
// Droplet was destroyed.
package dotest

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/digitalocean/godo"
)

// Token is the bearer token the emulator accepts unless WithToken names another.
const Token = "test"

// Option configures the emulator.
type Option func(*Server)

// WithToken sets the bearer token the emulator accepts.
func WithToken(token string) Option { return func(s *Server) { s.token = token } }

// WithActionDelay makes every action (and every Droplet delete) take d before it completes.
func WithActionDelay(d time.Duration) Option { return func(s *Server) { s.delay = d } }

// WithMaxPerPage caps a list page below the API's 200, to exercise pagination.
func WithMaxPerPage(n int) Option { return func(s *Server) { s.maxPerPage = n } }

// WithProject adds a project to the account.
func WithProject(id, name string) Option {
	return func(s *Server) {
		s.projects = append(s.projects, godo.Project{ID: id, Name: name, Purpose: "Service or API", Environment: "Development"})
	}
}

// WithReleaseLock models what DigitalOcean did to a reserved IP whose Droplet was destroyed on
// 2026-10-11 (gravel#153). The Droplet's delete unassigns the address through an unassign_ip
// action instead of at once (FailNextAction("unassign_ip") makes that action error, as the live
// one did, and a second one completes it after twice the action delay), and locks the address:
// the next DELETE of it answers 204 and does nothing, the n after that answer 422
// unprocessable_entity "The reserved IP action is unprocessable.", and only then does a DELETE
// release it. Without this option a Droplet's delete unassigns its addresses at once and a
// DELETE always releases.
func WithReleaseLock(n int) Option {
	return func(s *Server) { s.releaseLock = max(n, 0); s.releaseLockOn = true }
}

// WithoutActionLinks makes a Droplet create answer without links.actions, as the API is allowed
// to, so a client has to watch the Droplet instead.
func WithoutActionLinks() Option { return func(s *Server) { s.noLinks = true } }

// DefaultProjectID is the account's default project, which every emulator has.
const DefaultProjectID = "00000000-0000-4000-8000-000000000001"

// Request is one request the emulator saw.
type Request struct {
	Method, Path, Query string
	Body                string
	// Authorized is whether it carried the right bearer token.
	Authorized bool
}

// Server is a running emulator.
type Server struct {
	srv        *httptest.Server
	token      string
	delay      time.Duration
	maxPerPage int
	noLinks    bool
	// releaseLock is WithReleaseLock's count of refused DELETEs; releaseLockOn says it was set.
	releaseLock   int
	releaseLockOn bool

	mu          sync.Mutex
	seq         int
	ipSeq       int
	regions     []godo.Region
	sizes       []godo.Size
	images      []godo.Image
	droplets    map[int]*droplet
	volumes     map[string]*godo.Volume
	firewalls   map[string]*godo.Firewall
	reserved    map[string]*reservedIP
	keys        map[int]*godo.Key
	tags        map[string]bool
	projects    []godo.Project
	assigned    map[string][]string // project id → URNs
	actions     map[int]*action
	faults      []*fault
	failActions map[string]int
	requests    []Request
	remaining   int
}

type droplet struct {
	godo.Droplet
	deleteAt time.Time // set while being destroyed
}

type reservedIP struct {
	ip, region, project string
	dropletID           int
	// The lock WithReleaseLock puts on an address whose Droplet was destroyed: swallow is a
	// DELETE still to be answered 204 with no effect, refuse the 422s after it.
	swallow bool
	refuse  int
}

type action struct {
	godo.Action
	dropletID int // the Droplet it holds a pending event on, if any
	due       time.Time
	fail      bool
	apply     func()
}

type fault struct {
	method, path string
	status, left int
}

// New starts an emulator that stops when the test ends.
func New(t testing.TB, opts ...Option) *Server {
	t.Helper()
	s := &Server{
		token:       Token,
		seq:         100000,
		droplets:    map[int]*droplet{},
		volumes:     map[string]*godo.Volume{},
		firewalls:   map[string]*godo.Firewall{},
		reserved:    map[string]*reservedIP{},
		keys:        map[int]*godo.Key{},
		tags:        map[string]bool{},
		assigned:    map[string][]string{},
		actions:     map[int]*action{},
		failActions: map[string]int{},
		remaining:   5000,
		projects:    []godo.Project{{ID: DefaultProjectID, Name: "Default", IsDefault: true, Purpose: "Just trying out DigitalOcean", Environment: "Development"}},
	}
	s.regions, s.sizes, s.images = catalog()
	for _, o := range opts {
		o(s)
	}
	s.srv = httptest.NewServer(s.handler())
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the emulator's base URL, for Config.BaseURL.
func (s *Server) URL() string { return s.srv.URL }

// Fault makes the next times requests matching method ("" for any) and path answer status.
func (s *Server) Fault(method, path string, status, times int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, &fault{method: method, path: path, status: status, left: times})
}

// FailNextAction makes the next action of this type end "errored", with no effect. The types are
// the API's: "create", "power_on", "power_off", "shutdown", "resize", "attach_volume",
// "detach_volume", "assign_ip", "unassign_ip".
func (s *Server) FailNextAction(actionType string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failActions[actionType]++
}

// Requests are the requests the emulator has seen, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// ProjectResources are the URNs assigned to a project, in order.
func (s *Server) ProjectResources(projectID string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.assigned[projectID])
}

// AddSSHKey registers a key as if through the control panel and returns its id.
func (s *Server) AddSSHKey(name, publicKey string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := &godo.Key{ID: s.id(), Name: name, PublicKey: publicKey, Fingerprint: fingerprint(publicKey)}
	s.keys[k.ID] = k
	return k.ID
}

// Counts is how many of each resource the account holds.
type Counts struct {
	Droplets, Volumes, Firewalls, ReservedIPs, SSHKeys int
}

// Counts counts the account's resources, after settling what is due (a destroyed Droplet past
// its delay is gone).
func (s *Server) Counts() Counts {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settle()
	return Counts{
		Droplets: len(s.droplets), Volumes: len(s.volumes), Firewalls: len(s.firewalls),
		ReservedIPs: len(s.reserved), SSHKeys: len(s.keys),
	}
}

// Droplet returns a Droplet as the API holds it.
func (s *Server) Droplet(id int) (godo.Droplet, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settle()
	d, ok := s.droplets[id]
	if !ok {
		return godo.Droplet{}, false
	}
	return d.Droplet, true
}

func (s *Server) id() int {
	s.seq++
	return s.seq
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v2/regions", s.listRegions)
	mux.HandleFunc("GET /v2/sizes", s.listSizes)
	mux.HandleFunc("GET /v2/actions/{id}", s.getAction)

	mux.HandleFunc("GET /v2/droplets", s.listDroplets)
	mux.HandleFunc("POST /v2/droplets", s.createDroplet)
	mux.HandleFunc("GET /v2/droplets/{id}", s.getDroplet)
	mux.HandleFunc("DELETE /v2/droplets/{id}", s.deleteDroplet)
	mux.HandleFunc("POST /v2/droplets/{id}/actions", s.dropletAction)

	mux.HandleFunc("GET /v2/volumes", s.listVolumes)
	mux.HandleFunc("POST /v2/volumes", s.createVolume)
	mux.HandleFunc("GET /v2/volumes/{id}", s.getVolume)
	mux.HandleFunc("DELETE /v2/volumes/{id}", s.deleteVolume)
	mux.HandleFunc("POST /v2/volumes/{id}/actions", s.volumeAction)

	mux.HandleFunc("GET /v2/firewalls", s.listFirewalls)
	mux.HandleFunc("POST /v2/firewalls", s.createFirewall)
	mux.HandleFunc("GET /v2/firewalls/{id}", s.getFirewall)
	mux.HandleFunc("PUT /v2/firewalls/{id}", s.updateFirewall)
	mux.HandleFunc("DELETE /v2/firewalls/{id}", s.deleteFirewall)

	mux.HandleFunc("GET /v2/reserved_ips", s.listReservedIPs)
	mux.HandleFunc("POST /v2/reserved_ips", s.createReservedIP)
	mux.HandleFunc("GET /v2/reserved_ips/{ip}", s.getReservedIP)
	mux.HandleFunc("DELETE /v2/reserved_ips/{ip}", s.deleteReservedIP)
	mux.HandleFunc("POST /v2/reserved_ips/{ip}/actions", s.reservedIPAction)

	mux.HandleFunc("GET /v2/account/keys", s.listKeys)
	mux.HandleFunc("POST /v2/account/keys", s.createKey)

	mux.HandleFunc("POST /v2/tags", s.createTag)
	mux.HandleFunc("GET /v2/tags/{name}", s.getTag)
	mux.HandleFunc("POST /v2/tags/{name}/resources", s.tagResources)

	mux.HandleFunc("GET /v2/projects", s.listProjects)
	mux.HandleFunc("POST /v2/projects/{id}/resources", s.assignResources)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		authorized := r.Header.Get("Authorization") == "Bearer "+s.token

		s.mu.Lock()
		defer s.mu.Unlock()
		s.requests = append(s.requests, Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(body), Authorized: authorized})
		s.remaining--
		w.Header().Set("RateLimit-Limit", "5000")
		w.Header().Set("RateLimit-Remaining", strconv.Itoa(max(s.remaining, 0)))
		w.Header().Set("RateLimit-Reset", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
		if !authorized {
			apiError(w, http.StatusUnauthorized, "unauthorized", "Unable to authenticate you")
			return
		}
		for _, f := range s.faults {
			if f.left > 0 && (f.method == "" || f.method == r.Method) && f.path == r.URL.Path {
				f.left--
				apiError(w, f.status, "fault", http.StatusText(f.status))
				return
			}
		}
		s.settle()
		mux.ServeHTTP(w, r)
	})
}

// settle completes every action whose time has come, then removes destroyed Droplets.
func (s *Server) settle() {
	now := time.Now()
	ids := make([]int, 0, len(s.actions))
	for id := range s.actions {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		a := s.actions[id]
		if a.Status != godo.ActionInProgress || now.Before(a.due) {
			continue
		}
		if a.fail {
			a.Status = "errored"
		} else {
			if a.apply != nil {
				a.apply()
			}
			a.Status = godo.ActionCompleted
		}
		a.CompletedAt = &godo.Timestamp{Time: now}
	}
	for id, d := range s.droplets {
		if !d.deleteAt.IsZero() && !now.Before(d.deleteAt) {
			delete(s.droplets, id)
		}
	}
}

// newAction records an in-progress action; apply runs when it completes.
func (s *Server) newAction(typ, resourceType string, resourceID, dropletID int, region string, apply func()) *action {
	a := &action{
		Action: godo.Action{
			ID: s.id(), Status: godo.ActionInProgress, Type: typ,
			StartedAt:  &godo.Timestamp{Time: time.Now()},
			ResourceID: resourceID, ResourceType: resourceType, RegionSlug: region,
		},
		dropletID: dropletID,
		due:       time.Now().Add(s.delay),
		apply:     apply,
	}
	if s.failActions[typ] > 0 {
		s.failActions[typ]--
		a.fail = true
	}
	s.actions[a.ID] = a
	return a
}

// pending is whether a Droplet has an action in progress: DigitalOcean refuses another.
func (s *Server) pending(dropletID int) bool {
	for _, a := range s.actions {
		if a.dropletID == dropletID && a.Status == godo.ActionInProgress {
			return true
		}
	}
	return false
}

func (s *Server) getAction(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	a, ok := s.actions[id]
	if !ok {
		notFound(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"action": a.Action})
}

func (s *Server) listRegions(w http.ResponseWriter, r *http.Request) {
	s.writeList(w, r, "regions", s.regions)
}

func (s *Server) listSizes(w http.ResponseWriter, r *http.Request) {
	s.writeList(w, r, "sizes", s.sizes)
}

func (s *Server) region(slug string) (godo.Region, bool) {
	for _, r := range s.regions {
		if r.Slug == slug {
			return r, true
		}
	}
	return godo.Region{}, false
}

func (s *Server) size(slug string) (godo.Size, bool) {
	for _, sz := range s.sizes {
		if sz.Slug == slug {
			return sz, true
		}
	}
	return godo.Size{}, false
}

// writeList answers one page of a list the way the API does: the page's items under key, links
// to the other pages as absolute URLs, and the total in meta.
func writeListPage[T any](s *Server, w http.ResponseWriter, r *http.Request, key string, items []T) {
	q := r.URL.Query()
	per, _ := strconv.Atoi(q.Get("per_page"))
	if per < 1 {
		per = 20
	}
	per = min(per, 200)
	if s.maxPerPage > 0 {
		per = min(per, s.maxPerPage)
	}
	page, _ := strconv.Atoi(q.Get("page"))
	page = max(page, 1)
	pages := max((len(items)+per-1)/per, 1)
	lo := min((page-1)*per, len(items))
	hi := min(lo+per, len(items))
	link := func(n int) string {
		v := url.Values{}
		for k, vs := range q {
			v[k] = vs
		}
		v.Set("page", strconv.Itoa(n))
		v.Set("per_page", strconv.Itoa(per))
		return s.srv.URL + r.URL.Path + "?" + v.Encode()
	}
	p := &godo.Pages{}
	if page > 1 {
		p.First, p.Prev = link(1), link(page-1)
	}
	if page < pages {
		p.Next, p.Last = link(page+1), link(pages)
	}
	pageItems := items[lo:hi]
	if pageItems == nil {
		pageItems = []T{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		key:     pageItems,
		"links": godo.Links{Pages: p},
		"meta":  godo.Meta{Total: len(items)},
	})
}

func (s *Server) writeList(w http.ResponseWriter, r *http.Request, key string, items any) {
	switch v := items.(type) {
	case []godo.Region:
		writeListPage(s, w, r, key, v)
	case []godo.Size:
		writeListPage(s, w, r, key, v)
	case []godo.Droplet:
		writeListPage(s, w, r, key, v)
	case []godo.Volume:
		writeListPage(s, w, r, key, v)
	case []godo.Firewall:
		writeListPage(s, w, r, key, v)
	case []godo.ReservedIP:
		writeListPage(s, w, r, key, v)
	case []godo.Key:
		writeListPage(s, w, r, key, v)
	case []godo.Project:
		writeListPage(s, w, r, key, v)
	default:
		panic(fmt.Sprintf("dotest: no list for %T", items))
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// apiError is the API's error body: an id, a message and a request id.
func apiError(w http.ResponseWriter, status int, id, msg string) {
	writeJSON(w, status, map[string]string{"id": id, "message": msg, "request_id": randomHex(8)})
}

func notFound(w http.ResponseWriter) {
	apiError(w, http.StatusNotFound, "not_found", "The resource you were accessing could not be found.")
}

func unprocessable(w http.ResponseWriter, msg string) {
	apiError(w, http.StatusUnprocessableEntity, "unprocessable_entity", msg)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		apiError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func uuid() string {
	h := randomHex(16)
	return h[0:8] + "-" + h[8:12] + "-4" + h[13:16] + "-8" + h[17:20] + "-" + h[20:32]
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }
