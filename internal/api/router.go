package api

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// SetupRouter serves the API plus the embedded web UI (web contains only public files).
func SetupRouter(a *API, web fs.FS) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/auth/login", a.Login)
	mux.HandleFunc("POST /api/auth/logout", a.Logout)
	mux.HandleFunc("POST /api/inbound/raw", a.InboundRaw)

	mux.Handle("GET /api/me", a.auth(a.Me))
	mux.Handle("POST /api/me/password", a.auth(a.ChangePassword))
	mux.Handle("GET /api/stats", a.auth(a.GetStats))
	mux.Handle("GET /api/emails", a.auth(a.ListEmails))
	mux.Handle("GET /api/emails/{id}", a.auth(a.GetEmail))
	mux.Handle("PATCH /api/emails/{id}", a.auth(a.UpdateEmail))
	mux.Handle("DELETE /api/emails/{id}", a.auth(a.DeleteEmail))
	mux.Handle("POST /api/emails/send", a.auth(a.SendEmail))
	mux.Handle("POST /api/emails/empty-trash", a.auth(a.EmptyTrash))
	mux.Handle("POST /api/emails/{id}/unschedule", a.auth(a.Unschedule))
	mux.Handle("GET /api/attachments/{id}", a.auth(a.GetAttachment))

	mux.Handle("GET /api/admin/overview", a.admin(a.AdminOverview))
	mux.Handle("POST /api/admin/users", a.admin(a.AdminCreateUser))
	mux.Handle("PATCH /api/admin/users/{id}", a.admin(a.AdminUpdateUser))
	mux.Handle("DELETE /api/admin/users/{id}", a.admin(a.AdminDeleteUser))
	mux.Handle("POST /api/admin/mailboxes", a.admin(a.AdminCreateMailbox))
	mux.Handle("PATCH /api/admin/mailboxes/{id}", a.admin(a.AdminUpdateMailbox))
	mux.Handle("DELETE /api/admin/mailboxes/{id}", a.admin(a.AdminDeleteMailbox))
	mux.Handle("PUT /api/admin/grants", a.admin(a.AdminSetGrant))
	mux.Handle("GET /api/admin/settings", a.admin(a.AdminGetSettings))
	mux.Handle("POST /api/admin/settings", a.admin(a.AdminSaveSettings))

	mux.Handle("/api/", http.NotFoundHandler())
	mux.Handle("/", staticHandler(web))

	// Rejects state-changing browser requests from other origins (login CSRF, sibling
	// subdomains). Requests without Sec-Fetch-Site/Origin, like the Worker's push, pass.
	return securityHeaders(loggingMiddleware(http.NewCrossOriginProtection().Handler(mux)))
}

var assetRef = regexp.MustCompile(`((?:href|src)="assets/[^"?]+\.(?:js|css))"`)

// staticHandler serves the web files. Pages are served with ?v=<content hash> on their
// script and stylesheet URLs: Cloudflare tells browsers to keep .js/.css for 4 hours,
// so after an upgrade they ran the old scripts against the new pages.
func staticHandler(web fs.FS) http.Handler {
	h := sha256.New()
	_ = fs.WalkDir(web, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := fs.ReadFile(web, p)
			h.Write([]byte(p))
			h.Write(b)
		}
		return nil
	})
	ver := hex.EncodeToString(h.Sum(nil))[:12]

	pages := map[string][]byte{} // URL path -> page
	names, _ := fs.Glob(web, "*.html")
	for _, name := range names {
		if b, err := fs.ReadFile(web, name); err == nil {
			pages["/"+strings.TrimSuffix(name, "index.html")] = assetRef.ReplaceAll(b, []byte(`$1?v=`+ver+`"`))
		}
	}
	files := http.FileServerFS(web)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, ok := pages[r.URL.Path]
		if !ok {
			files.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(page)
	})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Printf("[%s] %s %s (%v)", r.Method, r.URL.Path, clientIP(r), time.Since(start))
		}
	})
}

// Email HTML is rendered in a sandboxed srcdoc iframe, which inherits this policy:
// remote images allowed, scripts only from our own files.
const csp = "default-src 'self'; img-src * data: blob:; style-src 'self' 'unsafe-inline'; " +
	"frame-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", csp)
		next.ServeHTTP(w, r)
	})
}
