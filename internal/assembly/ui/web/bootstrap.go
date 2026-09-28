// SPDX-License-Identifier: Apache-2.0

package web

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// The path a page asks for the token.
//
// It exists for the development flow. The server prints a link with the token in its
// fragment, and that link points at the server, which serves a built interface. In
// development the interface is served by the dev server on another port, and a token
// pasted across from one address to another is a step somebody has to remember every
// time they start something.
//
// So a page with no token asks for it here rather than failing every request it makes.
// The window that serves the built interface does not need this — its link already
// carries the token — which is why this is one small handler rather than a
// configuration switch.
const bootstrapPath = "/v1/bootstrap"

// handleBootstrap hands the token to a page that has not got one.
//
// It is deliberately outside the token check, because its whole purpose is to be the
// thing a page reaches before it has a token. Everything else the server answers is
// behind the check.
//
// What that costs is worth being plain about: the token stops being a barrier against
// other programs on this machine, because any of them can now fetch it. What it is
// still a barrier against is a page in a browser that did not come from this server,
// which is the case the token was added for — the local web server that some other
// process started, listening on the same address. The server binds the loopback
// interface only and never a routable one, so "other programs on this machine" is the
// whole of the remaining exposure, and a process on this machine can read this one's
// memory anyway.
func (s *Server) handleBootstrap(c *gin.Context) {
	// The token is read out of the configuration rather than being stored twice. A
	// second copy is a second thing to keep in step, and the configuration is what the
	// link was built from.
	token := s.config.Token
	if token == "" {
		// A server with no token has no link to hand out, and saying so is more use
		// than an empty object that would make the page try again forever.
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "this server was started without a token",
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token})
}
