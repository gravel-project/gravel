package wardogs

import (
	"fmt"
	"net/url"
	"strings"
)

// Route is one entry of the capabilities' route list, "METHOD /path/{param}".
type Route struct {
	Method string
	// Segments are the path's segments; a parameter keeps its braces ("{steamId}").
	Segments []string
}

// ParseRoute parses "GET /v1/players/{id}". The method is upper-cased; the path must be absolute.
func ParseRoute(s string) (Route, error) {
	method, path, ok := strings.Cut(strings.TrimSpace(s), " ")
	path = strings.TrimSpace(path)
	if !ok || method == "" || !strings.HasPrefix(path, "/") || strings.ContainsAny(path, " ?#") {
		return Route{}, fmt.Errorf("wardogs: route %q is not \"METHOD /path\"", s)
	}
	r := Route{Method: strings.ToUpper(method)}
	for _, seg := range strings.Split(strings.Trim(path, "/"), "/") {
		if seg == "" {
			return Route{}, fmt.Errorf("wardogs: route %q has an empty path segment", s)
		}
		r.Segments = append(r.Segments, seg)
	}
	return r, nil
}

func mustRoute(s string) Route {
	r, err := ParseRoute(s)
	if err != nil {
		panic(err)
	}
	return r
}

// String is the route as the server writes it.
func (r Route) String() string { return r.Method + " /" + strings.Join(r.Segments, "/") }

// Shape is the route with every parameter's name dropped: "GET /v1/players/{}". Two routes with
// the same shape are the same route under a renamed parameter.
func (r Route) Shape() string {
	segs := make([]string, len(r.Segments))
	for i, s := range r.Segments {
		if isParam(s) {
			s = "{}"
		}
		segs[i] = s
	}
	return r.Method + " /" + strings.Join(segs, "/")
}

// Params are the route's parameter names in order, without braces.
func (r Route) Params() []string {
	var out []string
	for _, s := range r.Segments {
		if isParam(s) {
			out = append(out, s[1:len(s)-1])
		}
	}
	return out
}

// Path fills the route's parameters in order, each escaped as one path segment.
func (r Route) Path(params ...string) (string, error) {
	if n := len(r.Params()); n != len(params) {
		return "", fmt.Errorf("wardogs: %s takes %d parameters, got %d", r, n, len(params))
	}
	var b strings.Builder
	i := 0
	for _, s := range r.Segments {
		b.WriteByte('/')
		if isParam(s) {
			if params[i] == "" {
				return "", fmt.Errorf("wardogs: %s: parameter %s is empty", r, s)
			}
			s = url.PathEscape(params[i])
			i++
		}
		b.WriteString(s)
	}
	return b.String(), nil
}

func isParam(seg string) bool {
	return len(seg) >= 2 && seg[0] == '{' && seg[len(seg)-1] == '}'
}
