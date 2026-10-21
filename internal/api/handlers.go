package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
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
		req.To, req.Cc, req.Subject = r.FormValue("to"), r.FormValue("cc"), r.FormValue("subject")
		req.Body, req.InReplyTo = r.FormValue("body"), r.FormValue("inReplyTo")
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
	subject := strings.TrimSpace(req.Subject)
	if subject == "" {
		subject = "(no subject)"
	}
	body := strings.ReplaceAll(strings.TrimSpace(req.Body), "\r\n", "\n") // multipart forms send CRLF

	inReplyTo := service.CleanMsgID(req.InReplyTo)
	html := renderHTMLBody(body)

	result, err := a.mailer.Send(service.SendOptions{
		FromAddress: mb.Address,
		FromName:    mb.DisplayName,
		To:          toList,
		Cc:          ccList,
		Subject:     subject,
		Text:        body,
		HTML:        html,
		InReplyTo:   inReplyTo,
		Attachments: attachments,
	})
	if err != nil {
		http.Error(w, "Gagal mengirim email: "+err.Error(), http.StatusBadGateway)
		return
	}

	sent := models.Email{
		MailboxID:    mb.ID,
		Provider:     result.Provider,
		MessageID:    result.MessageID,
		InReplyTo:    inReplyTo,
		FromName:     mb.DisplayName,
		FromEmail:    mb.Address,
		ToRecipients: toList,
		CcRecipients: ccList,
		Subject:      subject,
		Snippet:      service.CleanSnippet(body, 120),
		BodyText:     body,
		BodyHTML:     html,
		Folder:       "sent",
		CreatedAt:    time.Now(),
	}
	if err := os.MkdirAll(a.cfg.DataDir, 0750); err == nil {
		for _, at := range attachments {
			if saved, err := service.SaveAttachmentFile(a.cfg.DataDir, at.Filename, at.ContentType, at.Content); err == nil {
				sent.Attachments = append(sent.Attachments, saved)
			}
		}
	}
	if err := a.db.SaveEmail(&sent); err != nil {
		http.Error(w, "Email terkirim tetapi gagal disimpan: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "provider": result.Provider, "email": sent})
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
