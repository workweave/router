package admin

import (
	"errors"
	"net"
	"net/http"
	"os"
	"time"

	"workweave/router/internal/auth"
	"workweave/router/internal/observability"

	"github.com/gin-gonic/gin"
)

// remotePeerIP returns the immediate TCP peer's IP so rate limiting can't be bypassed by spoofing
// X-Forwarded-For. Returns the unstripped address if the peer lacks "host:port" shape.
func remotePeerIP(c *gin.Context) string {
	addr := c.Request.RemoteAddr
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

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

// LoginHandler verifies the password against ROUTER_ADMIN_PASSWORD and sets a signed HttpOnly session cookie.
// Returns 503 when admin login isn't configured so the dashboard can render a hint instead of a 401 loop.
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
		// Use raw TCP peer (not c.ClientIP) — X-Forwarded-For is attacker-controlled when the
		// router is reached directly, letting a brute-forcer rotate IPs and bypass the per-IP cap.
		peerIP := remotePeerIP(c)
		if err := authSvc.VerifyAdminPasswordFromIP(peerIP, req.Password); err != nil {
			if errors.Is(err, auth.ErrAdminLoginRateLimited) {
				observability.FromGin(c).Info("Admin login rejected: rate limited", "remote_ip", peerIP)
				c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
					"error": "too_many_attempts",
					"hint":  "wait a few minutes before trying again",
				})
				return
			}
			observability.FromGin(c).Info("Admin login rejected: wrong password", "remote_ip", peerIP)
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

func LogoutHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		clearAdminSessionCookie(c)
		c.JSON(http.StatusOK, gin.H{"ok": true})
	}
}

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

// cookieSecure controls whether admin session cookies are minted with the Secure flag. Default true so
// self-hosters never accidentally serve plaintext cookies; dev behind non-HTTPS can set
// ROUTER_COOKIE_INSECURE=true. Not derived from X-Forwarded-Proto: that header is attacker-controlled
// when the router is reached directly, and a wrong value silently downgrades cookie transport.
var cookieSecure = os.Getenv("ROUTER_COOKIE_INSECURE") != "true"

func setAdminSessionCookie(c *gin.Context, token string, expiresAt time.Time) {
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge < 0 {
		maxAge = 0
	}
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(auth.AdminSessionCookieName, token, maxAge, "/", "", cookieSecure, true)
}

func clearAdminSessionCookie(c *gin.Context) {
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(auth.AdminSessionCookieName, "", -1, "/", "", cookieSecure, true)
}
