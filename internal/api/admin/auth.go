package admin

import (
	"errors"
	"net/http"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"

	"github.com/gin-gonic/gin"
)

type loginRequest struct {
	Password string `json:"password"`
}

type loginResponse struct {
	OK        bool      `json:"ok"`
	ExpiresAt time.Time `json:"expires_at"`
}

type meResponse struct {
	Authenticated bool   `json:"authenticated"`
	Subject       string `json:"subject,omitempty"`
}

// LoginHandler verifies the supplied password against ROUTER_ADMIN_PASSWORD
// and, on success, sets a signed HttpOnly session cookie. Returns 503 when
// admin login isn't configured so the dashboard can render a "set
// ROUTER_ADMIN_PASSWORD to enable login" hint instead of a 401 loop.
func LoginHandler(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !authSvc.AdminLoginEnabled() {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"error": "admin_login_disabled",
				"hint":  "set ROUTER_ADMIN_PASSWORD on the router to enable dashboard login",
			})
			return
		}
		var req loginRequest
		if err := c.ShouldBindJSON(&req); err != nil || req.Password == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "missing_password"})
			return
		}
		if err := authSvc.VerifyAdminPassword(req.Password); err != nil {
			observability.FromGin(c).Info("Admin login rejected: wrong password")
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid_credentials"})
			return
		}
		token, expiresAt, err := authSvc.IssueAdminSession()
		if err != nil {
			observability.FromGin(c).Error("Failed to issue admin session", "err", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "session_issue_failed"})
			return
		}
		setAdminSessionCookie(c, token, expiresAt)
		observability.FromGin(c).Info("Admin login succeeded")
		c.JSON(http.StatusOK, loginResponse{OK: true, ExpiresAt: expiresAt})
	}
}

// LogoutHandler clears the admin session cookie. Always returns 200 — the
// caller wanted to be logged out, and they are.
func LogoutHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		clearAdminSessionCookie(c)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

// MeHandler reports whether the current request carries a valid admin
// session cookie. Used by the dashboard on initial load to decide between
// rendering the app shell vs. redirecting to /login.
func MeHandler(authSvc *auth.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !authSvc.AdminLoginEnabled() {
			c.JSON(http.StatusOK, meResponse{Authenticated: false})
			return
		}
		cookie, err := c.Cookie(auth.AdminSessionCookieName)
		if err != nil || cookie == "" {
			c.JSON(http.StatusOK, meResponse{Authenticated: false})
			return
		}
		principal, err := authSvc.VerifyAdminSession(cookie)
		if err != nil {
			if !errors.Is(err, auth.ErrAdminSessionInvalid) {
				observability.FromGin(c).Error("Admin session verify errored", "err", err)
			}
			clearAdminSessionCookie(c)
			c.JSON(http.StatusOK, meResponse{Authenticated: false})
			return
		}
		c.JSON(http.StatusOK, meResponse{Authenticated: true, Subject: principal.Subject})
	}
}

// setAdminSessionCookie writes the session cookie. Secure is set when the
// request arrived over TLS so local-dev http://localhost still works.
func setAdminSessionCookie(c *gin.Context, token string, expiresAt time.Time) {
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge < 0 {
		maxAge = 0
	}
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(auth.AdminSessionCookieName, token, maxAge, "/", "", isHTTPS(c), true)
}

func clearAdminSessionCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(auth.AdminSessionCookieName, "", -1, "/", "", isHTTPS(c), true)
}

// isHTTPS reports whether to mark the cookie Secure. Trusts X-Forwarded-Proto
// because the router typically sits behind a reverse proxy in self-hosted
// deploys (Cloud Run, fronting nginx, etc.).
func isHTTPS(c *gin.Context) bool {
	if c.Request.TLS != nil {
		return true
	}
	if proto := c.GetHeader("X-Forwarded-Proto"); proto == "https" {
		return true
	}
	return false
}
