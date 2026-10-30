package api

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"wmail/internal/db"
	"wmail/internal/models"
)

const (
	sessionCookie = "wmail_session"
	sessionTTL    = 30 * 24 * time.Hour
	maxFails      = 5
	failWindow    = 15 * time.Minute
)

type authedHandler func(http.ResponseWriter, *http.Request, *models.User)

func (a *API) currentUser(r *http.Request) *models.User {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	u, err := a.db.UserBySession(c.Value)
	if err != nil {
		return nil
	}
	return u
}

func (a *API) auth(h authedHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := a.currentUser(r)
		if u == nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r, u)
	})
}

func (a *API) admin(h authedHandler) http.Handler {
	return a.auth(func(w http.ResponseWriter, r *http.Request, u *models.User) {
		if !u.IsAdmin {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h(w, r, u)
	})
}

// clientIP trusts CF-Connecting-IP only when the request arrives via the local cloudflared.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if cf := r.Header.Get("CF-Connecting-IP"); cf != "" {
			return cf
		}
	}
	return host
}

// ipBucket groups IPv6 by /64: one host usually owns the whole block, so counting
// single addresses would hand it 2^64 fresh login budgets.
func ipBucket(ip string) string {
	p := net.ParseIP(ip)
	if p == nil || p.To4() != nil {
		return ip
	}
	return p.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

func (a *API) setSessionCookie(w http.ResponseWriter, r *http.Request, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteStrictMode,
	})
}

func (a *API) startSession(w http.ResponseWriter, r *http.Request, u *models.User) error {
	token, err := a.db.CreateSession(u.ID, sessionTTL)
	if err != nil {
		return err
	}
	a.setSessionCookie(w, r, token, int(sessionTTL.Seconds()))
	return nil
}

func (a *API) Login(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Password string }
	if !decode(w, r, &req) {
		return
	}
	ip := ipBucket(clientIP(r))
	// Reserve the attempt before hashing so concurrent requests share the same budget.
	if !a.limiter.attempt(ip) {
		http.Error(w, "Terlalu banyak percobaan login. Coba lagi dalam 15 menit.", http.StatusTooManyRequests)
		return
	}
	u, err := a.db.Authenticate(req.Username, req.Password)
	if err != nil {
		http.Error(w, "Username atau password salah.", http.StatusUnauthorized)
		return
	}
	a.limiter.refund(ip)
	if err := a.startSession(w, r, u); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (a *API) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = a.db.DeleteSession(c.Value)
	}
	a.setSessionCookie(w, r, "", -1)
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func (a *API) Me(w http.ResponseWriter, r *http.Request, u *models.User) {
	mailboxes, err := a.db.MailboxesForUser(u)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": u, "mailboxes": mailboxes})
}

func (a *API) ChangePassword(w http.ResponseWriter, r *http.Request, u *models.User) {
	var req struct{ Current, New string }
	if !decode(w, r, &req) {
		return
	}
	// Keyed by account, not IP: whoever holds the session gets 5 guesses per window, from anywhere.
	key := "pw:" + strconv.FormatInt(u.ID, 10)
	if !a.limiter.attempt(key) {
		http.Error(w, "Terlalu banyak percobaan. Coba lagi dalam 15 menit.", http.StatusTooManyRequests)
		return
	}
	if _, err := a.db.Authenticate(u.Username, req.Current); err != nil {
		http.Error(w, "Password lama salah.", http.StatusForbidden)
		return
	}
	a.limiter.refund(key)
	if err := a.db.SetPassword(u.ID, req.New); err != nil {
		writeDBError(w, err)
		return
	}
	// SetPassword signed out every session, including this one; issue a fresh one.
	if err := a.startSession(w, r, u); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// SetSignature saves the signature added below what u writes, from any mailbox.
func (a *API) SetSignature(w http.ResponseWriter, r *http.Request, u *models.User) {
	var req struct{ Signature string }
	if !decode(w, r, &req) {
		return
	}
	if len(req.Signature) > 4000 {
		http.Error(w, "Tanda tangan terlalu panjang (maks. 4000 karakter).", http.StatusBadRequest)
		return
	}
	if err := a.db.SetSignature(u.ID, req.Signature); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func writeDBError(w http.ResponseWriter, err error) {
	if errors.Is(err, db.ErrInvalidInput) || errors.Is(err, db.ErrLastAdmin) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// ponytail: in-memory counter per IP (login) or account (password change), unbounded map; fine for a home server, add eviction if it ever faces real traffic.
type loginLimiter struct {
	mu    sync.Mutex
	fails map[string]*failEntry
}

type failEntry struct {
	n     int
	first time.Time
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{fails: map[string]*failEntry{}} }

// attempt checks and counts one attempt atomically; a correct password calls refund.
func (l *loginLimiter) attempt(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.fails[key]
	if e == nil || time.Since(e.first) >= failWindow {
		e = &failEntry{first: time.Now()}
		l.fails[key] = e
	}
	if e.n >= maxFails {
		return false
	}
	e.n++
	return true
}

// refund returns only this request's slot. It never clears the count: a correct password
// for one account must not erase failures made against other accounts from the same key.
func (l *loginLimiter) refund(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e := l.fails[key]; e != nil && e.n > 0 {
		e.n--
	}
}
