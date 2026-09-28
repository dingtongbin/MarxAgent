// SPDX-License-Identifier: Apache-2.0

package web

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Three things are true of the router and are worth writing down, because each is a
// decision rather than a default:
//
//   - It does not log. Gin's own logger writes coloured text to standard output, and
//     the project's log fields are frozen: a set of named fields that everything is
//     supposed to agree on. A second, different log format on the same machine makes
//     the frozen set worthless, so logging happens through log/slog and nowhere else.
//   - It does not recover. A panic is recovered where it happens — in the core, by
//     the hook system — so that the reason is recorded where the work was. A blanket
//     recovery in front of that would turn a recorded fault into a blank 500.
//   - It does not trust a route that was not registered. Gin's default already
//     refuses those; it is said here because the alternative, serving the interface
//     for anything unmatched, is a decision somebody will otherwise wonder about.

// healthPath is the one address served without a token.
//
// It is named once so the route that serves it and the check that exempts it cannot
// drift apart. They did once: the handler said it was outside the token and the code
// refused it anyway.
const healthPath = "/healthz"

// newRouter builds the router.
//
// Release mode from the start rather than being switched to it in a build tag. The
// debug mode prints the whole route table on every start and puts a warning banner on
// every response, and a mode that only shows up in production is a mode nobody has
// ever looked at.
func newRouter(debug bool) *gin.Engine {
	if !debug {
		gin.SetMode(gin.ReleaseMode)
	}
	router := gin.New()
	// No trailing slash redirect: a path with or without one is the same resource, and
	// redirecting between them means a request can be sent twice. The one thing a
	// redirect is for is a canonical URL, and both of these are equally canonical.
	router.RedirectTrailingSlash = false
	return router
}

// mounted records which routes the server has, so a caller can be told what it can
// reach rather than discovering it by getting a 404.
type mounted struct {
	// Health is the one address served without a token.
	Health string
	// Stream is where the event stream is.
	Stream string
	// Interface is the built interface.
	Interface string
}

// routes attaches everything the server answers.
//
// The order here is the security order, and it is deliberate:
//
//   - The health check goes on first and never goes behind the check, because a
//     health check that needs a secret is a health check nobody can run.
//   - The token check goes on next, to everything else, rather than being attached to
//     each route. Attached per route it protects the routes somebody remembered; on
//     the whole engine it protects the routes somebody adds next week without
//     thinking about it, which is the point.
//   - The interface goes on last, as the fallback, so a path nobody claimed is a page
//     of the interface rather than a 404. That is what lets the interface own its own
//     paths, where a client-side route such as /session/3 is a real page to a browser
//     and a 404 to a server that only knows its files.
func (s *Server) routes(router *gin.Engine) mounted {
	// Both methods are registered because the router's tree does not do what the
	// standard library's mux does: a HEAD is not a GET here unless it is asked for,
	// and a health check that fails for a checker that sends HEAD is a health check
	// that has been declared broken by something that works.
	router.GET(healthPath, s.handleHealth)
	router.HEAD(healthPath, s.handleHealth)

	// Ahead of the token check on purpose: it is the call a page makes to get a token,
	// so requiring one would be a circle. Everything below it is behind the check.
	router.GET(bootstrapPath, s.handleBootstrap)

	router.Use(s.requireToken())

	stream := "/events"
	router.GET(stream, gin.WrapF(s.events.serve))

	// The interface asking what is on disk. It sits with the event stream rather than
	// with the caller's own routes because it is part of the interface's needs, and
	// because a build without the interface should not grow a filesystem browser.
	router.GET(browsePath, s.handleBrowse)
	router.GET(gitPath, s.handleGit)

	assets, err := s.assets()
	if err != nil {
		// A binary built without the interface still has to answer, and saying why is
		// more use than a bare 404 that looks like a routing mistake.
		router.NoRoute(func(c *gin.Context) {
			c.String(http.StatusNotFound,
				"the interface is not in this binary: %s", err.Error())
		})
		return mounted{Health: healthPath, Stream: stream, Interface: ""}
	}

	// The caller's own routes sit behind the same check as everything else rather
	// than each having to remember it.
	//
	// They are reached through the fallback rather than through a catch-all
	// wildcard, and that is deliberate. A wildcard route swallows every path that
	// reaches it, including `/` and every asset the interface is made of, so a
	// server built with a caller's handler attached would serve the caller's
	// answers to requests for the interface itself. The interface is put in front
	// of the fallback and the caller's addresses are recognised by their /v1
	// prefix, which is the prefix this project gives its own API.
	if s.config.Handler != nil {
		caller := s.config.Handler
		router.NoRoute(func(c *gin.Context) {
			if isCallerPath(c.Request.URL.Path) {
				caller.ServeHTTP(c.Writer, c.Request)
				return
			}
			assets.ServeHTTP(c.Writer, c.Request)
		})
	} else {
		router.NoRoute(gin.WrapH(assets))
	}
	return mounted{Health: healthPath, Stream: stream, Interface: "/"}
}

// callerPrefix is the address space the caller's routes live in.
//
// It is the same /v1 the bootstrap and the filesystem browser use, so everything
// the interface asks of the server is in one place and the interface itself is
// not.
const callerPrefix = "/v1/"

// isCallerPath reports whether a path belongs to the caller's routes.
func isCallerPath(path string) bool {
	return strings.HasPrefix(path, callerPrefix)
}

// requireToken refuses anything that does not present the token.
//
// The comparison is constant time because a token that can be discovered by timing
// the difference between a near miss and a miss is a token that can be discovered.
// The comparison covers the whole presented value, so a caller learns only whether
// they were right and never how close they were.
//
// A browser cannot set a header on a page request, which is why the token may also
// arrive as a cookie; the interface takes it from the fragment and sends it from then
// on.
func (s *Server) requireToken() gin.HandlerFunc {
	return func(c *gin.Context) {
		presented, ok := presentedToken(c.Request)
		if !ok || !matchesToken(presented, s.config.Token) {
			// One answer for no token, a wrong one and a malformed one, because three
			// different answers would tell a caller which of the three to fix.
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": "this server needs the token from the link it was started with",
			})
			return
		}
		c.Next()
	}
}

// presentedToken takes the token from wherever the caller put it.
func presentedToken(r *http.Request) (string, bool) {
	if header := r.Header.Get("Authorization"); header != "" {
		if after, found := strings.CutPrefix(header, "Bearer "); found {
			return strings.TrimSpace(after), true
		}
	}
	if cookie, err := r.Cookie(tokenCookie); err == nil && cookie.Value != "" {
		return cookie.Value, true
	}
	if query := r.URL.Query().Get("token"); query != "" {
		return query, true
	}
	return "", false
}

// handleHealth answers whether the server is up.
//
// It is outside the token, because what it reports is whether this process is
// serving, which is not the same question as whether the caller may ask anything
// else.
//
// There is no method check here because there is no other method to reach it: only
// GET and HEAD are registered, and anything else falls through to the route that
// catches what was not claimed — which is behind the token. A method that is not
// served at an address is answered by the address that is, and a 401 there says
// less about this process than it would be right to.
func (s *Server) handleHealth(c *gin.Context) {
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.Status(http.StatusOK)
	if c.Request.Method == http.MethodHead {
		return
	}
	_, _ = c.Writer.WriteString("ok\n")
}
