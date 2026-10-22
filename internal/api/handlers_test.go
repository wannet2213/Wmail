package api_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"wmail/internal/api"
	"wmail/internal/config"
	"wmail/internal/db"
	"wmail/internal/models"
	"wmail/internal/service"
)

const pw = "rahasia-123"

type env struct {
	db     *db.DB
	api    *api.API
	router http.Handler
	mbA    int64 // bob: receive only
	mbB    int64 // bob: no access
}

func setup(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{DBPath: filepath.Join(dir, "test.db"), DataDir: filepath.Join(dir, "att")}
	d, err := db.InitDB(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })

	if _, err := d.CreateUser("root", pw, true); err != nil {
		t.Fatal(err)
	}
	bob, err := d.CreateUser("bob", pw, false)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := d.CreateMailbox("a@x.test", "Kotak A")
	b, _ := d.CreateMailbox("b@x.test", "Kotak B")
	if err := d.SetGrant(bob, a, false); err != nil {
		t.Fatal(err)
	}
	_ = d.SetSetting("inbound_secret", "s3cret")

	web := fstest.MapFS{"index.html": {Data: []byte("<html>wmail</html>")}}
	a2 := api.NewAPI(cfg, d)
	return &env{db: d, api: a2, router: api.SetupRouter(a2, web), mbA: a, mbB: b}
}

func (e *env) do(t *testing.T, cookie *http.Cookie, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}

func (e *env) login(t *testing.T, user string) *http.Cookie {
	t.Helper()
	rec := e.do(t, nil, "POST", "/api/auth/login", map[string]string{"username": user, "password": pw})
	if rec.Code != 200 {
		t.Fatalf("login %s: %d %s", user, rec.Code, rec.Body)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "wmail_session" {
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
				t.Fatalf("session cookie flags wrong: %+v", c)
			}
			return c
		}
	}
	t.Fatal("no session cookie")
	return nil
}

func path(p string, mb int64) string { return p + "?mailbox=" + itoa(mb) }
func itoa(i int64) string            { b, _ := json.Marshal(i); return string(b) }

func TestLoginRequiredAndRateLimit(t *testing.T) {
	e := setup(t)
	if rec := e.do(t, nil, "GET", "/api/me", nil); rec.Code != 401 {
		t.Fatalf("anonymous /api/me: want 401, got %d", rec.Code)
	}
	for i := 0; i < 5; i++ {
		if rec := e.do(t, nil, "POST", "/api/auth/login", map[string]string{"username": "bob", "password": "salah"}); rec.Code != 401 {
			t.Fatalf("bad login %d: want 401, got %d", i, rec.Code)
		}
	}
	if rec := e.do(t, nil, "POST", "/api/auth/login", map[string]string{"username": "bob", "password": pw}); rec.Code != 429 {
		t.Fatalf("after 5 failures: want 429, got %d", rec.Code)
	}
}

func TestMailboxACL(t *testing.T) {
	e := setup(t)
	bob, root := e.login(t, "bob"), e.login(t, "root")

	var me struct{ Mailboxes []models.Mailbox }
	json.NewDecoder(e.do(t, bob, "GET", "/api/me", nil).Body).Decode(&me)
	if len(me.Mailboxes) != 1 || me.Mailboxes[0].ID != e.mbA || me.Mailboxes[0].CanSend {
		t.Fatalf("bob mailboxes: %+v", me.Mailboxes)
	}

	if rec := e.do(t, bob, "GET", path("/api/emails", e.mbA), nil); rec.Code != 200 {
		t.Fatalf("bob list A: %d", rec.Code)
	}
	if rec := e.do(t, bob, "GET", path("/api/emails", e.mbB), nil); rec.Code != 403 {
		t.Fatalf("bob list B: want 403, got %d", rec.Code)
	}
	if rec := e.do(t, root, "GET", path("/api/emails", e.mbB), nil); rec.Code != 200 {
		t.Fatalf("admin list B: %d", rec.Code)
	}
	send := map[string]any{"from": e.mbA, "to": "x@example.com", "subject": "hi", "body": "hi"}
	if rec := e.do(t, bob, "POST", "/api/emails/send", send); rec.Code != 403 {
		t.Fatalf("receive-only send: want 403, got %d", rec.Code)
	}
	if rec := e.do(t, bob, "GET", "/api/admin/overview", nil); rec.Code != 403 {
		t.Fatalf("bob admin: want 403, got %d", rec.Code)
	}

	// Admin grants send; bob may now send (provider missing -> 502, but past the ACL).
	if rec := e.do(t, root, "PUT", "/api/admin/grants", map[string]any{"userId": 2, "mailboxId": e.mbA, "access": "send"}); rec.Code != 200 {
		t.Fatalf("grant: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do(t, bob, "POST", "/api/emails/send", send); rec.Code != http.StatusBadGateway {
		t.Fatalf("send without provider: want 502, got %d %s", rec.Code, rec.Body)
	}
}

const rawMail = "From: Ani <ani@example.com>\r\n" +
	"To: a@x.test\r\n" +
	"Subject: =?utf-8?q?Laporan_bulan_ini_=E2=9C=93?=\r\n" +
	"Message-ID: <m1@example.com>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=XX\r\n\r\n" +
	"--XX\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Halo <script>alert(1)</script></p>\r\n" +
	"--XX\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=\"catatan.txt\"\r\n\r\nisi lampiran\r\n" +
	"--XX--\r\n"

func (e *env) inbound(t *testing.T, to, secret, raw string) int {
	req := httptest.NewRequest("POST", "/api/inbound/raw", strings.NewReader(raw))
	req.Header.Set("x-inbound-secret", secret)
	req.Header.Set("x-envelope-to", to)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec.Code
}

func TestInboundRaw(t *testing.T) {
	e := setup(t)
	if code := e.inbound(t, "a@x.test", "wrong", rawMail); code != 401 {
		t.Fatalf("bad secret: want 401, got %d", code)
	}
	if code := e.inbound(t, "nobody@x.test", "s3cret", rawMail); code != 404 {
		t.Fatalf("unknown mailbox: want 404, got %d", code)
	}
	for i, to := range []string{"A@X.test", "a+promo@x.test"} { // redelivery (Worker retry / KV drain) must not duplicate
		if code := e.inbound(t, to, "s3cret", rawMail); code != 200 {
			t.Fatalf("inbound %d: %d", i, code)
		}
	}

	bob := e.login(t, "bob")
	var list struct{ Emails []models.Email }
	json.NewDecoder(e.do(t, bob, "GET", path("/api/emails", e.mbA), nil).Body).Decode(&list)
	if len(list.Emails) != 1 {
		t.Fatalf("want 1 email, got %d", len(list.Emails))
	}
	m := list.Emails[0]
	if m.Subject != "Laporan bulan ini ✓" || m.FromEmail != "ani@example.com" || !m.HasAttachments {
		t.Fatalf("parsed wrong: %+v", m)
	}

	var full models.Email
	json.NewDecoder(e.do(t, bob, "GET", "/api/emails/"+itoa(m.ID), nil).Body).Decode(&full)
	if len(full.Attachments) != 1 {
		t.Fatalf("attachments: %+v", full.Attachments)
	}
	rec := e.do(t, bob, "GET", "/api/attachments/"+full.Attachments[0].ID, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "isi lampiran") ||
		!strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment") || rec.Header().Get("Content-Security-Policy") != "sandbox" {
		t.Fatalf("attachment: %d %v %s", rec.Code, rec.Header(), rec.Body)
	}

	// Another user's mailbox: email and attachment are invisible.
	e.db.DeleteGrant(2, e.mbA)
	if rec := e.do(t, bob, "GET", "/api/emails/"+itoa(m.ID), nil); rec.Code != 404 {
		t.Fatalf("revoked email: want 404, got %d", rec.Code)
	}
	if rec := e.do(t, bob, "GET", "/api/attachments/"+full.Attachments[0].ID, nil); rec.Code != 404 {
		t.Fatalf("revoked attachment: want 404, got %d", rec.Code)
	}
}

func TestSendViaResend(t *testing.T) {
	e := setup(t)
	var got map[string]any
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer re_test" {
			w.WriteHeader(401)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(`{"id":"abc"}`))
	}))
	defer fake.Close()
	service.ResendURL = fake.URL
	e.db.SetSetting("resend_api_key", "re_test")

	root := e.login(t, "root")
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("from", itoa(e.mbA))
	mw.WriteField("to", "Tujuan <dest@example.com>")
	mw.WriteField("subject", "Uji")
	mw.WriteField("body", "Halo <b>")
	fw, _ := mw.CreateFormFile("attachments", "a.txt")
	fw.Write([]byte("lampiran"))
	mw.Close()
	req := httptest.NewRequest("POST", "/api/emails/send", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(root)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	if got["from"] != `"Kotak A" <a@x.test>` || got["html"] != "<p>Halo &lt;b&gt;</p>" {
		t.Fatalf("resend payload: %v", got)
	}

	// A reply quotes with "> " in the text part but must not show the marks in HTML.
	body.Reset()
	mw = multipart.NewWriter(&body)
	mw.WriteField("from", itoa(e.mbA))
	mw.WriteField("to", "dest@example.com")
	mw.WriteField("body", "Oke.\r\n\r\nPada Senin, Budi menulis:\r\n> Rapat?\r\n> > Kapan?")
	mw.Close()
	req = httptest.NewRequest("POST", "/api/emails/send", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(root)
	e.router.ServeHTTP(httptest.NewRecorder(), req)
	quote := `<blockquote class="gmail_quote" style="margin:0 0 0 .8ex;border-left:1px solid #ccc;padding-left:1ex">`
	want := `<p>Oke.</p><div class="gmail_quote"><div class="gmail_attr">Pada Senin, Budi menulis:</div>` + quote +
		`<p>Rapat?</p><div class="gmail_quote">` + quote + `<p>Kapan?</p></blockquote></div></blockquote></div>`
	if got["html"] != want || !strings.HasSuffix(got["text"].(string), "> > Kapan?") {
		t.Fatalf("reply html = %v\nwant %s", got["html"], want)
	}
	if list, _, _ := e.db.ListEmails(e.mbA, "sent", "", 50, 0); len(list) == 0 {
		t.Fatal("sent mail not stored")
	}
}

func TestAdminSettingsMasked(t *testing.T) {
	e := setup(t)
	root := e.login(t, "root")
	e.db.SetSetting("resend_api_key", "re_1234567890abcd")

	var s map[string]string
	json.NewDecoder(e.do(t, root, "GET", "/api/admin/settings", nil).Body).Decode(&s)
	if strings.Contains(s["resend_api_key"], "567890") || s["inbound_secret"] != "••••" {
		t.Fatalf("not masked: %v", s)
	}
	// Posting the masked value back must not overwrite the real key.
	if rec := e.do(t, root, "POST", "/api/admin/settings", map[string]string{"resend_api_key": s["resend_api_key"], "active_provider": "ses"}); rec.Code != 200 {
		t.Fatalf("save: %d %s", rec.Code, rec.Body)
	}
	if e.db.GetSetting("resend_api_key") != "re_1234567890abcd" || e.db.GetSetting("active_provider") != "ses" {
		t.Fatal("settings save wrong")
	}
	if rec := e.do(t, root, "POST", "/api/admin/settings", map[string]string{"cf_api_token": "x"}); rec.Code != 400 {
		t.Fatalf("non-web key: want 400, got %d", rec.Code)
	}
}

func TestLimitersSurviveSuccessfulLogins(t *testing.T) {
	e := setup(t)
	login := func(user, pass string) int {
		return e.do(t, nil, "POST", "/api/auth/login", map[string]string{"username": user, "password": pass}).Code
	}
	for i := 0; i < 4; i++ {
		login("root", "salah")
	}
	if c := login("bob", pw); c != 200 { // attacker's own account
		t.Fatalf("own login: want 200, got %d", c)
	}
	if c := login("root", "salah"); c != 401 {
		t.Fatalf("5th failure: want 401, got %d", c)
	}
	if c := login("root", pw); c != 429 {
		t.Fatalf("a success on another account reset the budget: want 429, got %d", c)
	}

	e2 := setup(t)
	bob := e2.login(t, "bob")
	for i := 0; i < 5; i++ {
		e2.do(t, bob, "POST", "/api/me/password", map[string]string{"current": "salah", "new": "baru-12345"})
	}
	if rec := e2.do(t, bob, "POST", "/api/me/password", map[string]string{"current": pw, "new": "baru-12345"}); rec.Code != 429 {
		t.Fatalf("password change after 5 wrong guesses: want 429, got %d", rec.Code)
	}
}

func TestLoginLimitCoversWholeIPv6Block(t *testing.T) {
	e := setup(t)
	login := func(addr string) int {
		req := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"username":"root","password":"salah"}`))
		req.RemoteAddr = "127.0.0.1:5000" // cloudflared
		req.Header.Set("CF-Connecting-IP", addr)
		rec := httptest.NewRecorder()
		e.router.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 1; i <= 5; i++ {
		login(fmt.Sprintf("2001:db8:1:2::%x", i))
	}
	if c := login("2001:db8:1:2:ffff::9"); c != 429 {
		t.Fatalf("new address in the same /64: want 429, got %d", c)
	}
	if c := login("2001:db8:1:3::1"); c != 401 {
		t.Fatalf("another /64 has its own budget: want 401, got %d", c)
	}
}

func TestPlainHTTPViaCloudflareRedirects(t *testing.T) {
	e := setup(t)
	req := httptest.NewRequest("GET", "/settings.html?x=1", nil)
	req.Host = "mail.x.test"
	req.Header.Set("X-Forwarded-Proto", "http")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != "https://mail.x.test/settings.html?x=1" {
		t.Fatalf("want 308 to https, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if rec := e.do(t, nil, "GET", "/", nil); rec.Header().Get("Strict-Transport-Security") == "" {
		t.Fatal("HSTS header missing")
	}
}

func TestLoginLimiterHoldsUnderConcurrency(t *testing.T) {
	e := setup(t)
	var wg sync.WaitGroup
	codes := make(chan int, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- e.do(t, nil, "POST", "/api/auth/login", map[string]string{"username": "root", "password": "salah"}).Code
		}()
	}
	wg.Wait()
	close(codes)
	evaluated := 0
	for c := range codes {
		if c == 401 {
			evaluated++
		}
	}
	if evaluated > 5 {
		t.Fatalf("concurrent burst: %d passwords evaluated, cap is 5", evaluated)
	}
}

func TestInboundLongAttachmentNameIsStored(t *testing.T) {
	e := setup(t)
	long := strings.Repeat("a", 240) + ".txt"
	raw := "From: x@example.com\r\nTo: a@x.test\r\nSubject: s\r\nMessage-ID: <long@example.com>\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n" +
		"--B\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=\"ok.bin\"\r\n\r\nok\r\n" +
		"--B\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=\"" + long + "\"\r\n\r\nx\r\n--B--\r\n"
	if code := e.inbound(t, "a@x.test", "s3cret", raw); code != 200 {
		t.Fatalf("long attachment name: want 200, got %d", code)
	}
	root := e.login(t, "root")
	var list struct{ Emails []models.Email }
	json.NewDecoder(e.do(t, root, "GET", path("/api/emails", e.mbA), nil).Body).Decode(&list)
	var full models.Email
	json.NewDecoder(e.do(t, root, "GET", "/api/emails/"+itoa(list.Emails[0].ID), nil).Body).Decode(&full)
	if len(full.Attachments) != 2 || full.Attachments[1].Filename != long {
		t.Fatalf("attachments: %+v", full.Attachments)
	}
}

func TestCrossOriginRequestsRejected(t *testing.T) {
	e := setup(t)
	creds := `{"username":"bob","password":"` + pw + `"}`
	for name, hdr := range map[string][2]string{
		"sec-fetch cross-site": {"Sec-Fetch-Site", "cross-site"},
		"sec-fetch same-site":  {"Sec-Fetch-Site", "same-site"}, // sibling subdomain
		"foreign origin":       {"Origin", "https://evil.example"},
	} {
		req := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(creds))
		req.Header.Set("Content-Type", "text/plain")
		req.Header.Set(hdr[0], hdr[1])
		rec := httptest.NewRecorder()
		e.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 {
			t.Fatalf("%s: want 403 without cookie, got %d", name, rec.Code)
		}
	}
	req := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(creds))
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("same-origin login: want 200, got %d", rec.Code)
	}
}

func TestReceiveOnlyGrantIsReadOnly(t *testing.T) {
	e := setup(t)
	if code := e.inbound(t, "a@x.test", "s3cret", rawMail); code != 200 {
		t.Fatal("inbound failed")
	}
	bob := e.login(t, "bob")
	var list struct{ Emails []models.Email }
	json.NewDecoder(e.do(t, bob, "GET", path("/api/emails", e.mbA), nil).Body).Decode(&list)
	id := "/api/emails/" + itoa(list.Emails[0].ID)

	if rec := e.do(t, bob, "PATCH", id, map[string]bool{"isStarred": true}); rec.Code != 200 {
		t.Fatalf("receive-only star: want 200, got %d", rec.Code)
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"patch delete": e.do(t, bob, "PATCH", id, map[string]bool{"isDeleted": true}),
		"patch folder": e.do(t, bob, "PATCH", id, map[string]string{"folder": "spam"}),
		"hard delete":  e.do(t, bob, "DELETE", id, nil),
		"empty trash":  e.do(t, bob, "POST", path("/api/emails/empty-trash", e.mbA), nil),
	} {
		if rec.Code != http.StatusForbidden {
			t.Fatalf("receive-only %s: want 403, got %d", name, rec.Code)
		}
	}

	e.db.SetGrant(2, e.mbA, true) // bob now send+receive
	if rec := e.do(t, bob, "DELETE", id, nil); rec.Code != http.StatusConflict {
		t.Fatalf("hard delete outside trash: want 409, got %d", rec.Code)
	}
	if rec := e.do(t, bob, "PATCH", id, map[string]bool{"isDeleted": true}); rec.Code != 200 {
		t.Fatalf("move to trash: %d", rec.Code)
	}
	if rec := e.do(t, bob, "DELETE", id, nil); rec.Code != 200 {
		t.Fatalf("delete from trash: %d", rec.Code)
	}
}

func TestScheduledSend(t *testing.T) {
	e := setup(t)
	var sent []map[string]any
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		json.NewDecoder(r.Body).Decode(&got)
		sent = append(sent, got)
		w.Write([]byte(`{"id":"abc"}`))
	}))
	defer fake.Close()
	service.ResendURL = fake.URL
	e.db.SetSetting("resend_api_key", "re_test")
	root := e.login(t, "root")

	send := func(sendAt string) *httptest.ResponseRecorder {
		return e.do(t, root, "POST", "/api/emails/send", map[string]any{
			"from": e.mbA, "to": "dest@example.com", "bcc": "diam@example.com", "subject": "Nanti",
			"body": "Halo", "html": "<p><b>Halo</b></p>", "sendAt": sendAt,
		})
	}
	if rec := send(time.Now().Add(-time.Minute).Format(time.RFC3339)); rec.Code != 400 {
		t.Fatalf("a time in the past must be refused, got %d", rec.Code)
	}
	if rec := send(time.Now().Add(time.Hour).Format(time.RFC3339)); rec.Code != 200 {
		t.Fatalf("schedule: %d %s", rec.Code, rec.Body)
	}
	list, _, _ := e.db.ListEmails(e.mbA, "scheduled", "", 50, 0)
	if len(sent) != 0 || len(list) != 1 {
		t.Fatalf("scheduled mail must wait: sent %d, Terjadwal %d", len(sent), len(list))
	}

	e.api.SendDue() // not due yet
	_ = e.db.Reschedule(list[0].ID, time.Now().Add(-time.Second))
	e.api.SendDue()
	if len(sent) != 1 || sent[0]["html"] != "<p><b>Halo</b></p>" || fmt.Sprint(sent[0]["bcc"]) != "[diam@example.com]" {
		t.Fatalf("due mail not sent as written: %v", sent)
	}
	if list, _, _ = e.db.ListEmails(e.mbA, "sent", "", 50, 0); len(list) != 1 || list[0].MessageID != "<abc@resend>" {
		t.Fatalf("sent mail should leave Terjadwal for Terkirim: %+v", list)
	}

	// Cancelling hands the message back and removes it.
	send(time.Now().Add(time.Hour).Format(time.RFC3339))
	list, _, _ = e.db.ListEmails(e.mbA, "scheduled", "", 50, 0)
	if rec := e.do(t, root, "POST", "/api/emails/"+itoa(list[0].ID)+"/unschedule", nil); rec.Code != 200 {
		t.Fatalf("unschedule: %d %s", rec.Code, rec.Body)
	}
	if list, _, _ = e.db.ListEmails(e.mbA, "scheduled", "", 50, 0); len(list) != 0 {
		t.Fatalf("cancelled mail still scheduled: %+v", list)
	}
}

func TestSignatureIsPerUser(t *testing.T) {
	e := setup(t)
	bob, root := e.login(t, "bob"), e.login(t, "root")
	if rec := e.do(t, bob, "PUT", "/api/me/signature", map[string]string{"signature": "Salam,\r\nBob"}); rec.Code != 200 {
		t.Fatalf("set signature: %d %s", rec.Code, rec.Body)
	}
	sig := func(c *http.Cookie) string {
		var me struct{ User models.User }
		json.NewDecoder(e.do(t, c, "GET", "/api/me", nil).Body).Decode(&me)
		return me.User.Signature
	}
	if got := sig(bob); got != "Salam,\nBob" {
		t.Fatalf("bob's signature = %q", got)
	}
	if got := sig(root); got != "" {
		t.Fatalf("another user's signature leaked: %q", got)
	}
}
