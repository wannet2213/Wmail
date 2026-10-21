package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"net/mail"
	"os"
	"strconv"
	"strings"
	"time"

	"wmail/internal/config"
	"wmail/internal/db"
	"wmail/internal/models"
	"wmail/internal/service"
)

type API struct {
	cfg     *config.Config
	db      *db.DB
	mailer  *service.Mailer
	inbound *service.InboundService
	limiter *loginLimiter
}

func NewAPI(cfg *config.Config, database *db.DB) *API {
	return &API{
		cfg:     cfg,
		db:      database,
		mailer:  service.NewMailer(database),
		inbound: service.NewInboundService(cfg.DataDir, database),
		limiter: newLoginLimiter(),
	}
}

func (a *API) Inbound() *service.InboundService { return a.inbound }

// mailboxParam resolves ?mailbox=<id> and enforces that u may read it.
func (a *API) mailboxParam(w http.ResponseWriter, r *http.Request, u *models.User) (*models.Mailbox, bool) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("mailbox"), 10, 64)
	mb, _ := a.db.Access(u, id)
	if mb == nil {
		http.Error(w, "mailbox tidak ditemukan atau tidak ada akses", http.StatusForbidden)
		return nil, false
	}
	return mb, true
}

const readOnlyMsg = "Mailbox ini hanya bisa dibaca (hak terima saja)."

// emailFor loads {id} and answers 404 (not 403) when u lacks access, so ids don't leak.
// manage=true additionally requires a send+receive grant: a receive-only grant is read-only.
func (a *API) emailFor(w http.ResponseWriter, r *http.Request, u *models.User, manage bool) (*models.Email, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid email id", http.StatusBadRequest)
		return nil, false
	}
	email, err := a.db.GetEmail(id)
	if err == nil {
		if mb, canManage := a.db.Access(u, email.MailboxID); mb != nil {
			if manage && !canManage {
				http.Error(w, readOnlyMsg, http.StatusForbidden)
				return nil, false
			}
			return email, true
		}
	}
	http.Error(w, "email not found", http.StatusNotFound)
	return nil, false
}

func (a *API) GetStats(w http.ResponseWriter, r *http.Request, u *models.User) {
	mb, ok := a.mailboxParam(w, r, u)
	if !ok {
		return
	}
	stats, err := a.db.GetStats(mb.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (a *API) ListEmails(w http.ResponseWriter, r *http.Request, u *models.User) {
	mb, ok := a.mailboxParam(w, r, u)
	if !ok {
		return
	}
	q := r.URL.Query()
	folder := q.Get("folder")
	if folder == "" {
		folder = "inbox"
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	emails, total, err := a.db.ListEmails(mb.ID, folder, q.Get("q"), limit, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"emails": emails, "total": total, "folder": folder})
}

func (a *API) GetEmail(w http.ResponseWriter, r *http.Request, u *models.User) {
	email, ok := a.emailFor(w, r, u, false)
	if !ok {
		return
	}
	// Loaded before marking read: the client opens the messages that were still unread.
	thread, err := a.db.Thread(email)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, m := range thread {
		if m.IsUnread {
			read := false
			_ = a.db.UpdateEmailStatus(email.ID, models.UpdateStatusRequest{IsUnread: &read})
			break
		}
	}
	email.IsUnread = false
	writeJSON(w, http.StatusOK, struct {
		*models.Email
		Thread []models.Email `json:"thread"`
	}{email, thread})
}

func (a *API) UpdateEmail(w http.ResponseWriter, r *http.Request, u *models.User) {
	var req models.UpdateStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json payload", http.StatusBadRequest)
		return
	}
	// Read and star flags are fine for readers; moving or deleting needs a send+receive grant.
	manage := req.IsDeleted != nil || req.IsArchived != nil || req.Folder != nil || req.Label != nil
	email, ok := a.emailFor(w, r, u, manage)
	if !ok {
		return
	}
	if err := a.db.UpdateEmailStatus(email.ID, req); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func (a *API) DeleteEmail(w http.ResponseWriter, r *http.Request, u *models.User) {
	email, ok := a.emailFor(w, r, u, true)
	if !ok {
		return
	}
	if !email.IsDeleted {
		http.Error(w, "Pindahkan email ke Sampah dulu sebelum dihapus permanen.", http.StatusConflict)
		return
	}
	if err := a.db.DeleteEmail(email.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func (a *API) EmptyTrash(w http.ResponseWriter, r *http.Request, u *models.User) {
	mb, ok := a.mailboxParam(w, r, u)
	if !ok {
		return
	}
	if !mb.CanSend {
		http.Error(w, readOnlyMsg, http.StatusForbidden)
		return
	}
	if err := a.db.EmptyTrash(mb.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func (a *API) SendEmail(w http.ResponseWriter, r *http.Request, u *models.User) {
	r.Body = http.MaxBytesReader(w, r.Body, 40<<20) // Resend's message limit
	var req models.SendRequest
	var attachments []service.MailAttachment

	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "failed to parse multipart form: "+err.Error(), http.StatusBadRequest)
			return
		}
		req.To, req.Cc, req.Bcc, req.Subject = r.FormValue("to"), r.FormValue("cc"), r.FormValue("bcc"), r.FormValue("subject")
		req.Body, req.HTML, req.Quote = r.FormValue("body"), r.FormValue("html"), r.FormValue("quote")
		req.InReplyTo, req.SendAt = r.FormValue("inReplyTo"), r.FormValue("sendAt")
		req.From, _ = strconv.ParseInt(r.FormValue("from"), 10, 64)
		for _, fh := range r.MultipartForm.File["attachments"] {
			f, err := fh.Open()
			if err != nil {
				http.Error(w, "failed to read attachment", http.StatusBadRequest)
				return
			}
			content, err := io.ReadAll(f)
			f.Close()
			if err != nil {
				http.Error(w, "failed to read attachment", http.StatusBadRequest)
				return
			}
			attachments = append(attachments, service.MailAttachment{Filename: fh.Filename, ContentType: fh.Header.Get("Content-Type"), Content: content})
		}
	} else if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	mb, canSend := a.db.Access(u, req.From)
	if mb == nil || !canSend {
		http.Error(w, "Mailbox ini tidak diizinkan untuk mengirim email.", http.StatusForbidden)
		return
	}
	toList, err := parseAddresses(req.To)
	if err != nil || len(toList) == 0 {
		http.Error(w, "Alamat penerima (To) tidak valid.", http.StatusBadRequest)
		return
	}
	ccList, err := parseAddresses(req.Cc)
	if err != nil {
		http.Error(w, "Alamat Cc tidak valid.", http.StatusBadRequest)
		return
	}
	bccList, err := parseAddresses(req.Bcc)
	if err != nil {
		http.Error(w, "Alamat Bcc tidak valid.", http.StatusBadRequest)
		return
	}
	var sendAt time.Time
	if req.SendAt != "" {
		if sendAt, err = time.Parse(time.RFC3339, req.SendAt); err != nil || !sendAt.After(time.Now()) {
			http.Error(w, "Waktu jadwal tidak valid atau sudah lewat.", http.StatusBadRequest)
			return
		}
	}
	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		subject = "(no subject)"
	}
	crlf := strings.NewReplacer("\r\n", "\n") // multipart forms send CRLF
	body := strings.TrimSpace(crlf.Replace(req.Body))
	quote := strings.TrimSpace(crlf.Replace(req.Quote))
	html := strings.TrimSpace(req.HTML)
	if html == "" {
		html = renderHTMLBody(body)
	}
	text := body
	if quote != "" {
		text = strings.TrimSpace(body + "\n\n" + quote)
		html += renderHTMLBody(quote)
	}

	email := models.Email{
		MailboxID:     mb.ID,
		InReplyTo:     service.CleanMsgID(req.InReplyTo),
		FromName:      mb.DisplayName,
		FromEmail:     mb.Address,
		ToRecipients:  toList,
		CcRecipients:  ccList,
		BccRecipients: bccList,
		Subject:       subject,
		Snippet:       service.CleanSnippet(body, 120),
		BodyText:      text,
		BodyHTML:      html,
		Folder:        "sent",
		CreatedAt:     time.Now(),
	}
	if !sendAt.IsZero() {
		// Stored now, sent by RunScheduled when the time comes.
		email.Folder, email.CreatedAt = "scheduled", sendAt
	} else {
		result, err := a.mailer.Send(sendOptions(&email, attachments))
		if err != nil {
			http.Error(w, "Gagal mengirim email: "+err.Error(), http.StatusBadGateway)
			return
		}
		email.Provider, email.MessageID = result.Provider, result.MessageID
	}

	if err := os.MkdirAll(a.cfg.DataDir, 0750); err == nil {
		for _, at := range attachments {
			if saved, err := service.SaveAttachmentFile(a.cfg.DataDir, at.Filename, at.ContentType, at.Content); err == nil {
				email.Attachments = append(email.Attachments, saved)
			}
		}
	}
	if err := a.db.SaveEmail(&email); err != nil {
		http.Error(w, "Email terkirim tetapi gagal disimpan: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "provider": email.Provider, "email": email})
}

func sendOptions(e *models.Email, attachments []service.MailAttachment) service.SendOptions {
	return service.SendOptions{
		FromAddress: e.FromEmail,
		FromName:    e.FromName,
		To:          e.ToRecipients,
		Cc:          e.CcRecipients,
		Bcc:         e.BccRecipients,
		Subject:     e.Subject,
		Text:        e.BodyText,
		HTML:        e.BodyHTML,
		InReplyTo:   e.InReplyTo,
		Attachments: attachments,
	}
}

// Unschedule cancels a scheduled message and returns it, so the client can reopen it.
func (a *API) Unschedule(w http.ResponseWriter, r *http.Request, u *models.User) {
	email, ok := a.emailFor(w, r, u, true)
	if !ok {
		return
	}
	if email.Folder != "scheduled" {
		http.Error(w, "Email ini sudah tidak terjadwal.", http.StatusConflict)
		return
	}
	if err := a.db.Unschedule(email.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, email)
}

// SendDue sends every scheduled message whose time has come. A failed one is retried
// 10 minutes later and stays in Terjadwal, where its owner can cancel it.
func (a *API) SendDue() {
	ids, err := a.db.DueScheduled(time.Now())
	if err != nil {
		log.Printf("[JADWAL] gagal membaca antrean: %v", err)
		return
	}
	for _, id := range ids {
		e, err := a.db.GetEmail(id)
		if err != nil {
			continue
		}
		var atts []service.MailAttachment
		for _, at := range e.Attachments {
			if content, err := os.ReadFile(at.FilePath); err == nil {
				atts = append(atts, service.MailAttachment{Filename: at.Filename, ContentType: at.ContentType, Content: content})
			}
		}
		result, err := a.mailer.Send(sendOptions(e, atts))
		if err != nil {
			// ponytail: retries every 10 minutes for good; add a give-up limit if permanent failures pile up.
			log.Printf("[JADWAL] email %d gagal dikirim, dicoba lagi 10 menit lagi: %v", id, err)
			_ = a.db.Reschedule(id, time.Now().Add(10*time.Minute))
			continue
		}
		if err := a.db.MarkSent(id, result.Provider, result.MessageID); err != nil {
			log.Printf("[JADWAL] email %d terkirim tetapi gagal ditandai: %v", id, err)
		}
	}
}

func (a *API) RunScheduled(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		a.SendDue()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *API) InboundRaw(w http.ResponseWriter, r *http.Request) {
	secret := a.db.GetSetting("inbound_secret")
	if secret == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("x-inbound-secret")), []byte(secret)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 30<<20))
	if err != nil {
		http.Error(w, "message too large", http.StatusRequestEntityTooLarge)
		return
	}
	_, err = a.inbound.Ingest(raw, r.Header.Get("x-envelope-to"))
	switch {
	case err == nil, errors.Is(err, db.ErrDuplicate):
		writeJSON(w, http.StatusOK, map[string]bool{"success": true})
	case errors.Is(err, service.ErrUnknownMailbox):
		http.Error(w, "no such mailbox", http.StatusNotFound)
	case errors.Is(err, service.ErrRejected):
		// Permanent: the Worker rejects instead of parking it for endless retries.
		http.Error(w, "message rejected: "+err.Error(), http.StatusUnprocessableEntity)
	default:
		http.Error(w, "failed to store message: "+err.Error(), http.StatusInternalServerError)
	}
}

func (a *API) GetAttachment(w http.ResponseWriter, r *http.Request, u *models.User) {
	att, err := a.db.GetAttachment(r.PathValue("id"))
	if err == nil {
		var email *models.Email
		if email, err = a.db.GetEmail(att.EmailID); err == nil {
			if mb, _ := a.db.Access(u, email.MailboxID); mb == nil {
				err = errors.New("no access")
			}
		}
	}
	if err != nil {
		http.Error(w, "attachment not found", http.StatusNotFound)
		return
	}

	file, err := os.Open(att.FilePath)
	if err != nil {
		http.Error(w, "file not found on storage", http.StatusNotFound)
		return
	}
	defer file.Close()

	// Only raster images open inline; everything else downloads. The sandbox CSP
	// keeps a hostile file from running script on our origin either way.
	disposition := "attachment"
	switch att.ContentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		disposition = "inline"
	}
	w.Header().Set("Content-Type", att.ContentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": att.Filename}))
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Content-Length", strconv.FormatInt(att.SizeBytes, 10))
	_, _ = io.Copy(w, file)
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// parseAddresses splits on , or ; and returns bare addresses; empty input is not an error.
func parseAddresses(input string) ([]string, error) {
	var res []string
	for _, p := range strings.FieldsFunc(input, func(r rune) bool { return r == ',' || r == ';' }) {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		addr, err := mail.ParseAddress(p)
		if err != nil {
			return nil, err
		}
		res = append(res, addr.Address)
	}
	return res, nil
}

var escapeHTML = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace

// renderHTMLBody turns plain text into HTML the way Gmail sends it: runs of "> " lines
// become (nested) blockquotes instead of showing the ">" marks, and the "X menulis:"
// line just above a quote joins it in a gmail_quote div, which readers fold away.
func renderHTMLBody(body string) string {
	var out strings.Builder
	var para []string
	flush := func() {
		if len(para) > 0 {
			out.WriteString("<p>" + strings.Join(para, "<br>") + "</p>")
			para = nil
		}
	}
	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines); {
		if !strings.HasPrefix(lines[i], ">") {
			if strings.TrimSpace(lines[i]) == "" {
				flush()
			} else {
				para = append(para, escapeHTML(lines[i]))
			}
			i++
			continue
		}
		var quoted []string
		for ; i < len(lines) && strings.HasPrefix(lines[i], ">"); i++ {
			quoted = append(quoted, strings.TrimPrefix(lines[i][1:], " "))
		}
		attr := ""
		if n := len(para); n > 0 && strings.HasSuffix(para[n-1], ":") {
			attr, para = `<div class="gmail_attr">`+para[n-1]+`</div>`, para[:n-1]
		}
		flush()
		out.WriteString(`<div class="gmail_quote">` + attr +
			`<blockquote class="gmail_quote" style="margin:0 0 0 .8ex;border-left:1px solid #ccc;padding-left:1ex">` +
			renderHTMLBody(strings.Join(quoted, "\n")) + `</blockquote></div>`)
	}
	flush()
	return out.String()
}
