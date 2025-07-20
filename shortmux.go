// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package shortmux

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"path"
	"runtime"
	"slices"
	"strings"
	"sync"
)

// ServeMux is an HTTP request multiplexer.
// It matches the URL of each incoming request against a list of registered
// patterns and calls the handler for the pattern that
// most closely matches the URL.
//
// # Patterns
//
// Patterns can match the method, host and path of a request.
// Some examples:
//
//   - "/index.html" matches the path "/index.html" for any host and method.
//   - "GET /static/" matches a GET request whose path begins with "/static/".
//   - "example.com/" matches any request to the host "example.com".
//   - "example.com/{$}" matches requests with host "example.com" and path "/".
//   - "/b/{bucket}/o/{objectname...}" matches paths whose first segment is "b"
//     and whose third segment is "o". The name "bucket" denotes the second
//     segment and "objectname" denotes the remainder of the path.
//
// In general, a pattern looks like
//
//	[METHOD ][HOST]/[PATH]
//
// All three parts are optional; "/" is a valid pattern.
// If METHOD is present, it must be followed by at least one space or tab.
//
// Literal (that is, non-wildcard) parts of a pattern match
// the corresponding parts of a request case-sensitively.
//
// A pattern with no method matches every method. A pattern
// with the method GET matches both GET and HEAD requests.
// Otherwise, the method must match exactly.
//
// A pattern with no host matches every host.
// A pattern with a host matches URLs on that host only.
//
// A path can include wildcard segments of the form {NAME} or {NAME...}.
// For example, "/b/{bucket}/o/{objectname...}".
// The wildcard name must be a valid Go identifier.
// Wildcards must be full path segments: they must be preceded by a slash and followed by
// either a slash or the end of the string.
// For example, "/b_{bucket}" is not a valid pattern.
//
// Normally a wildcard matches only a single path segment,
// ending at the next literal slash (not %2F) in the request URL.
// But if the "..." is present, then the wildcard matches the remainder of the URL path, including slashes.
// (Therefore it is invalid for a "..." wildcard to appear anywhere but at the end of a pattern.)
// The match for a wildcard can be obtained by calling [Request.PathValue] with the wildcard's name.
// A trailing slash in a path acts as an anonymous "..." wildcard.
//
// The special wildcard {$} matches only the end of the URL.
// For example, the pattern "/{$}" matches only the path "/",
// whereas the pattern "/" matches every path.
//
// For matching, both pattern paths and incoming request paths are unescaped segment by segment.
// So, for example, the path "/a%2Fb/100%25" is treated as having two segments, "a/b" and "100%".
// The pattern "/a%2fb/" matches it, but the pattern "/a/b/" does not.
//
// # Precedence
//
// If two or more patterns match a request, then the most specific pattern takes precedence.
// A pattern P1 is more specific than P2 if P1 matches a strict subset of P2’s requests;
// that is, if P2 matches all the requests of P1 and more.
// If neither is more specific, then the patterns conflict.
// There is one exception to this rule, for backwards compatibility:
// if two patterns would otherwise conflict and one has a host while the other does not,
// then the pattern with the host takes precedence.
// If a pattern passed to [ServeMux.Handle] or [ServeMux.HandleFunc] conflicts with
// another pattern that is already registered, those functions panic.
//
// As an example of the general rule, "/images/thumbnails/" is more specific than "/images/",
// so both can be registered.
// The former matches paths beginning with "/images/thumbnails/"
// and the latter will match any other path in the "/images/" subtree.
//
// As another example, consider the patterns "GET /" and "/index.html":
// both match a GET request for "/index.html", but the former pattern
// matches all other GET and HEAD requests, while the latter matches any
// request for "/index.html" that uses a different method.
// The patterns conflict.
//
// # Trailing-slash redirection
//
// Consider a [ServeMux] with a handler for a subtree, registered using a trailing slash or "..." wildcard.
// If the ServeMux receives a request for the subtree root without a trailing slash,
// it redirects the request by adding the trailing slash.
// This behavior can be overridden with a separate registration for the path without
// the trailing slash or "..." wildcard. For example, registering "/images/" causes ServeMux
// to redirect a request for "/images" to "/images/", unless "/images" has
// been registered separately.
//
// # Request sanitizing
//
// ServeMux also takes care of sanitizing the URL request path and the Host
// header, stripping the port number and redirecting any request containing . or
// .. segments or repeated slashes to an equivalent, cleaner URL.
// Escaped path elements such as "%2e" for "." and "%2f" for "/" are preserved
// and aren't considered separators for request routing.
//
// # Compatibility
//
// The pattern syntax and matching behavior of ServeMux changed significantly
// in Go 1.22. To restore the old behavior, set the GODEBUG environment variable
// to "httpmuxgo121=1". This setting is read once, at program startup; changes
// during execution will be ignored.
//
// The backwards-incompatible changes include:
//   - Wildcards are just ordinary literal path segments in 1.21.
//     For example, the pattern "/{x}" will match only that path in 1.21,
//     but will match any one-segment path in 1.22.
//   - In 1.21, no pattern was rejected, unless it was empty or conflicted with an existing pattern.
//     In 1.22, syntactically invalid patterns will cause [ServeMux.Handle] and [ServeMux.HandleFunc] to panic.
//     For example, in 1.21, the patterns "/{"  and "/a{x}" match themselves,
//     but in 1.22 they are invalid and will cause a panic when registered.
//   - In 1.22, each segment of a pattern is unescaped; this was not done in 1.21.
//     For example, in 1.22 the pattern "/%61" matches the path "/a" ("%61" being the URL escape sequence for "a"),
//     but in 1.21 it would match only the path "/%2561" (where "%25" is the escape for the percent sign).
//   - When matching patterns to paths, in 1.22 each segment of the path is unescaped; in 1.21, the entire path is unescaped.
//     This change mostly affects how paths with %2F escapes adjacent to slashes are treated.
//     See https://go.dev/issue/21955 for details.
type ServeMux struct {
	mu    sync.RWMutex
	tree  routingNode
	index routingIndex
}

// NewServeMux allocates and returns a new [ServeMux].
func NewServeMux() *ServeMux {
	return &ServeMux{}
}

// cleanPath returns the canonical path for p, eliminating . and .. elements.
func cleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if p[0] != '/' {
		p = "/" + p
	}
	np := path.Clean(p)
	// path.Clean removes trailing slash except for root;
	// put the trailing slash back if necessary.
	if p[len(p)-1] == '/' && np != "/" {
		// Fast path for common case of p being the string we want:
		if len(p) == len(np)+1 && strings.HasPrefix(p, np) {
			np = p
		} else {
			np += "/"
		}
	}
	return np
}

// stripHostPort returns h without any trailing ":<port>".
func stripHostPort(h string) string {
	// If no port on host, return unchanged
	if !strings.Contains(h, ":") {
		return h
	}
	host, _, err := net.SplitHostPort(h)
	if err != nil {
		return h // on error, return unchanged
	}
	return host
}

// Handler returns the handler to use for the given request,
// consulting r.Method, r.Host, and r.URL.Path. It always returns
// a non-nil handler. If the path is not in its canonical form, the
// handler will be an internally-generated handler that redirects
// to the canonical path. If the host contains a port, it is ignored
// when matching handlers.
//
// The path and host are used unchanged for CONNECT requests.
//
// Handler also returns the registered pattern that matches the
// request or, in the case of internally-generated redirects,
// the path that will match after following the redirect.
//
// If there is no registered handler that applies to the request,
// Handler returns a “page not found” handler and an empty pattern.
func (mux *ServeMux) Handler(r *http.Request) (h http.Handler, pattern string) {
	h, p, _, _ := mux.findHandler(r)
	return h, p
}

// findHandler finds a handler for a request.
// If there is a matching handler, it returns it and the pattern that matched.
// Otherwise it returns a Redirect or NotFound handler with the path that would match
// after the redirect.
func (mux *ServeMux) findHandler(r *http.Request) (h http.Handler, patStr string, _ *pattern, matches []string) {
	var n *routingNode
	host := r.URL.Host
	escapedPath := r.URL.EscapedPath()
	path := escapedPath
	// CONNECT requests are not canonicalized.
	if r.Method == "CONNECT" {
		// If r.URL.Path is /tree and its handler is not registered,
		// the /tree -> /tree/ redirect applies to CONNECT requests
		// but the path canonicalization does not.
		_, _, u := mux.matchOrRedirect(host, r.Method, path, r.URL)
		if u != nil {
			return http.RedirectHandler(u.String(), http.StatusMovedPermanently), u.Path, nil, nil
		}
		// Redo the match, this time with r.Host instead of r.URL.Host.
		// Pass a nil URL to skip the trailing-slash redirect logic.
		n, matches, _ = mux.matchOrRedirect(r.Host, r.Method, path, nil)
	} else {
		// All other requests have any port stripped and path cleaned
		// before passing to mux.handler.
		host = stripHostPort(r.Host)
		path = cleanPath(path)

		// If the given path is /tree and its handler is not registered,
		// redirect for /tree/.
		var u *url.URL
		n, matches, u = mux.matchOrRedirect(host, r.Method, path, r.URL)
		if u != nil {
			return http.RedirectHandler(u.String(), http.StatusMovedPermanently), u.Path, nil, nil
		}
		if path != escapedPath {
			// Redirect to cleaned path.
			patStr := ""
			if n != nil {
				patStr = n.pattern.String()
			}
			u := &url.URL{Path: path, RawQuery: r.URL.RawQuery}
			return http.RedirectHandler(u.String(), http.StatusMovedPermanently), patStr, nil, nil
		}
	}
	if n == nil {
		// We didn't find a match with the request method. To distinguish between
		// Not Found and Method Not Allowed, see if there is another pattern that
		// matches except for the method.
		allowedMethods := mux.matchingMethods(host, path)
		if len(allowedMethods) > 0 {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Allow", strings.Join(allowedMethods, ", "))
				http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			}), "", nil, nil
		}
		return http.NotFoundHandler(), "", nil, nil
	}
	return n.handler, n.pattern.String(), n.pattern, matches
}

// matchOrRedirect looks up a node in the tree that matches the host, method and path.
//
// If the url argument is non-nil, handler also deals with trailing-slash
// redirection: when a path doesn't match exactly, the match is tried again
// after appending "/" to the path. If that second match succeeds, the last
// return value is the URL to redirect to.
func (mux *ServeMux) matchOrRedirect(host, method, path string, u *url.URL) (_ *routingNode, matches []string, redirectTo *url.URL) {
	mux.mu.RLock()
	defer mux.mu.RUnlock()

	n, matches := mux.tree.match(host, method, path)
	// If we have an exact match, or we were asked not to try trailing-slash redirection,
	// or the URL already has a trailing slash, then we're done.
	if !exactMatch(n, path) && u != nil && !strings.HasSuffix(path, "/") {
		// If there is an exact match with a trailing slash, then redirect.
		path += "/"
		n2, _ := mux.tree.match(host, method, path)
		if exactMatch(n2, path) {
			return nil, nil, &url.URL{Path: cleanPath(u.Path) + "/", RawQuery: u.RawQuery}
		}
	}
	return n, matches, nil
}

// exactMatch reports whether the node's pattern exactly matches the path.
// As a special case, if the node is nil, exactMatch return false.
//
// Before wildcards were introduced, it was clear that an exact match meant
// that the pattern and path were the same string. The only other possibility
// was that a trailing-slash pattern, like "/", matched a path longer than
// it, like "/a".
//
// With wildcards, we define an inexact match as any one where a multi wildcard
// matches a non-empty string. All other matches are exact.
// For example, these are all exact matches:
//
//	pattern   path
//	/a        /a
//	/{x}      /a
//	/a/{$}    /a/
//	/a/       /a/
//
// The last case has a multi wildcard (implicitly), but the match is exact because
// the wildcard matches the empty string.
//
// Examples of matches that are not exact:
//
//	pattern   path
//	/         /a
//	/a/{x...} /a/b
func exactMatch(n *routingNode, path string) bool {
	if n == nil {
		return false
	}
	// We can't directly implement the definition (empty match for multi
	// wildcard) because we don't record a match for anonymous multis.

	// If there is no multi, the match is exact.
	if !n.pattern.lastSegment().multi {
		return true
	}

	// If the path doesn't end in a trailing slash, then the multi match
	// is non-empty.
	if len(path) > 0 && path[len(path)-1] != '/' {
		return false
	}
	// Only patterns ending in {$} or a multi wildcard can
	// match a path with a trailing slash.
	// For the match to be exact, the number of pattern
	// segments should be the same as the number of slashes in the path.
	// E.g. "/a/b/{$}" and "/a/b/{...}" exactly match "/a/b/", but "/a/" does not.
	return len(n.pattern.segments) == strings.Count(path, "/")
}

// matchingMethods return a sorted list of all methods that would match with the given host and path.
func (mux *ServeMux) matchingMethods(host, path string) []string {
	// Hold the read lock for the entire method so that the two matches are done
	// on the same set of registered patterns.
	mux.mu.RLock()
	defer mux.mu.RUnlock()
	ms := map[string]bool{}
	mux.tree.matchingMethods(host, path, ms)
	// matchOrRedirect will try appending a trailing slash if there is no match.
	if !strings.HasSuffix(path, "/") {
		mux.tree.matchingMethods(host, path+"/", ms)
	}
	return slices.Sorted(maps.Keys(ms))
}

// ServeHTTP dispatches the request to the handler whose
// pattern most closely matches the request URL.
func (mux *ServeMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.RequestURI == "*" {
		if r.ProtoAtLeast(1, 1) {
			w.Header().Set("Connection", "close")
		}
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var h http.Handler
	h, r.Pattern, r.pat, matches = mux.findHandler(r)
	h.ServeHTTP(w, r)
}

// The four functions below all call ServeMux.register so that callerLocation
// always refers to user code.

// Handle registers the handler for the given pattern.
// If the given pattern conflicts, with one that is already registered, Handle
// panics.
func (mux *ServeMux) Handle(pattern string, handler http.Handler) {
	mux.register(pattern, handler)
}

// HandleFunc registers the handler function for the given pattern.
// If the given pattern conflicts, with one that is already registered, HandleFunc
// panics.
func (mux *ServeMux) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	mux.register(pattern, http.HandlerFunc(handler))
}

func (mux *ServeMux) register(pattern string, handler http.Handler) {
	if err := mux.registerErr(pattern, handler); err != nil {
		panic(err)
	}
}

func (mux *ServeMux) registerErr(patstr string, handler http.Handler) error {
	if patstr == "" {
		return errors.New("http: invalid pattern")
	}
	if handler == nil {
		return errors.New("http: nil handler")
	}
	if f, ok := handler.(http.HandlerFunc); ok && f == nil {
		return errors.New("http: nil handler")
	}

	pat, err := parsePattern(patstr)
	if err != nil {
		return fmt.Errorf("parsing %q: %w", patstr, err)
	}

	// Get the caller's location, for better conflict error messages.
	// Skip register and whatever calls it.
	_, file, line, ok := runtime.Caller(3)
	if !ok {
		pat.loc = "unknown location"
	} else {
		pat.loc = fmt.Sprintf("%s:%d", file, line)
	}

	mux.mu.Lock()
	defer mux.mu.Unlock()
	// Check for conflict.
	if err := mux.index.possiblyConflictingPatterns(pat, func(pat2 *pattern) error {
		if pat.conflictsWith(pat2) {
			d := describeConflict(pat, pat2)
			return fmt.Errorf("pattern %q (registered at %s) conflicts with pattern %q (registered at %s):\n%s",
				pat, pat.loc, pat2, pat2.loc, d)
		}
		return nil
	}); err != nil {
		return err
	}
	mux.tree.addPattern(pat, handler)
	mux.index.addPattern(pat)
	return nil
}
