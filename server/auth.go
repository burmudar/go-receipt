package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	sessionCookie   = "rcpt_session"
	sessionLifetime = 30 * 24 * time.Hour
)

// auth is single-user password authentication with signed session cookies.
// The signing key exists only in memory, so a restart ends all sessions.
type auth struct {
	password []byte
	key      []byte
	secure   bool
	now      func() time.Time
}

func newAuth(password string, secure bool) (*auth, error) {
	if password == "" {
		return nil, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return &auth{password: []byte(password), key: key, secure: secure, now: time.Now}, nil
}

func (a *auth) sign(expiry string) string {
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(expiry))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *auth) valid(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	expiry, signature, ok := strings.Cut(cookie.Value, ".")
	if !ok || !hmac.Equal([]byte(signature), []byte(a.sign(expiry))) {
		return false
	}
	unix, err := strconv.ParseInt(expiry, 10, 64)
	return err == nil && a.now().Before(time.Unix(unix, 0))
}

func (a *auth) checkPassword(password string) bool {
	return subtle.ConstantTimeCompare([]byte(password), a.password) == 1
}

func (a *auth) setSession(w http.ResponseWriter) {
	expires := a.now().Add(sessionLifetime)
	expiry := strconv.FormatInt(expires.Unix(), 10)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    expiry + "." + a.sign(expiry),
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *auth) clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   a.secure,
		SameSite: http.SameSiteLaxMode,
	})
}
