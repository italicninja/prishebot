package web

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// requireAuth is a Gin middleware that gates access to authenticated routes.
//
// Middleware in Gin works like a pipeline: each handler in the chain calls
// c.Next() to pass control to the next one, or c.Abort() to stop the chain.
// We use c.Set/c.MustGet to pass the resolved *Session to downstream handlers
// without making another cookie + store lookup.
func (s *Server) requireAuth(c *gin.Context) {
	sess := s.sessionFromCookie(c)
	if sess == nil {
		// Not logged in - redirect to the homepage (login page).
		// We use 302 (Found / temporary redirect) rather than 401 because
		// this is a browser-facing UI, not an API.
		c.Redirect(http.StatusFound, "/")
		c.Abort()
		return
	}

	// Stash the session in the Gin context so handlers don't need to look it up again.
	c.Set("session", sess)
	c.Next()
}
