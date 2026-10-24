package gateway

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Taseen007/MethodSplit/internal/config"
)

type Route struct {
	Method              string
	Class               string
	Consistency         string
	IdempotencyRequired bool
	CacheTTL            time.Duration
	segments            []segment
}

type segment struct {
	literal   string
	isParam   bool
	paramName string
}

func compileRoute(r config.Route) Route {
	return Route{
		Method:              strings.ToUpper(r.Method),
		Class:               r.Class,
		Consistency:         r.Consistency,
		IdempotencyRequired: r.Idempotency == config.IdempotencyRequired,
		CacheTTL:            r.CacheTTL.Duration(),
		segments:            parseSegments(r.Path),
	}
}

func parseSegments(path string) []segment {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	parts := strings.Split(trimmed, "/")
	segs := make([]segment, len(parts))
	for i, p := range parts {
		if strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}") {
			segs[i] = segment{isParam: true, paramName: strings.TrimSuffix(strings.TrimPrefix(p, "{"), "}")}
		} else {
			segs[i] = segment{literal: p}
		}
	}
	return segs
}

func matchSegments(segs []segment, path string) (map[string]string, bool) {
	trimmed := strings.Trim(path, "/")
	var parts []string
	if trimmed != "" {
		parts = strings.Split(trimmed, "/")
	}
	if len(parts) != len(segs) {
		return nil, false
	}
	var params map[string]string
	for i, s := range segs {
		if s.isParam {
			if params == nil {
				params = make(map[string]string, len(segs))
			}
			params[s.paramName] = parts[i]
		} else if s.literal != parts[i] {
			return nil, false
		}
	}
	return params, true
}

type RouteTable struct {
	routes []Route
}

func NewRouteTable(routes []config.Route) *RouteTable {
	rt := &RouteTable{routes: make([]Route, 0, len(routes))}
	for _, r := range routes {
		rt.routes = append(rt.routes, compileRoute(r))
	}
	return rt
}

type MatchResult struct {
	Route      Route
	Params     map[string]string
	Found      bool     // matched method + path
	PathExists bool     // path matches some route, but method doesn't -> 405
	Allowed    []string // Allow header values, only set when PathExists
}

// Match finds the route for method+path. HEAD is matched against the GET
// route for the same path (net/http's ServeMux does the same for the
// backend; the gateway mirrors it so a HEAD request gets the same class,
// consistency, and pool as the equivalent GET).
func (rt *RouteTable) Match(method, path string) MatchResult {
	lookupMethod := strings.ToUpper(method)
	if lookupMethod == http.MethodHead {
		lookupMethod = http.MethodGet
	}

	allowedSet := make(map[string]bool)
	for _, r := range rt.routes {
		params, ok := matchSegments(r.segments, path)
		if !ok {
			continue
		}
		allowedSet[r.Method] = true
		if r.Method == lookupMethod {
			return MatchResult{Route: r, Params: params, Found: true, PathExists: true}
		}
	}

	if len(allowedSet) == 0 {
		return MatchResult{}
	}
	if allowedSet[http.MethodGet] {
		allowedSet[http.MethodHead] = true
	}
	allowed := make([]string, 0, len(allowedSet))
	for m := range allowedSet {
		allowed = append(allowed, m)
	}
	sort.Strings(allowed)
	return MatchResult{PathExists: true, Allowed: allowed}
}
