package service

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"wmail/internal/db"

	"github.com/google/uuid"
)

type MailAttachment struct {
	Filename    string
	ContentType string
	Content     []byte
}

type SendOptions struct {
	FromAddress string
	FromName    string
	To          []string
	Cc          []string
	Bcc         []string // envelope only: never written into the headers
	Subject     string
	Text        string
	HTML        string
	InReplyTo   string
	Attachments []MailAttachment
}

type SendResult struct {
	Provider  string `json:"provider"`
	MessageID string `json:"messageId"`
}

var ErrNoProvider = errors.New("belum ada provider pengiriman: jalankan `wmail config set resend_api_key` atau isi kredensial SES")

// ResendURL is a var so tests can point it at httptest.
var ResendURL = "https://api.resend.com"

type Mailer struct {
	db         *db.DB
	httpClient *http.Client
}

func NewMailer(database *db.DB) *Mailer {
	return &Mailer{db: database, httpClient: &http.Client{Timeout: 30 * time.Second}}
}

// Send picks a provider from the settings table on every call, so CLI changes apply immediately.
// auto: Resend until its free-tier quota is reached or it errors, then SES.
func (m *Mailer) Send(opts SendOptions) (*SendResult, error) {
	if len(opts.To) == 0 {
		return nil, fmt.Errorf("recipient 'to' cannot be empty")
	}
	opts.InReplyTo = CleanMsgID(opts.InReplyTo)
	s, err := m.db.GetSettings()
	if err != nil {
		return nil, err
	}
	resendOK := s["resend_api_key"] != ""
	sesOK := s["aws_region"] != "" && s["aws_access_key_id"] != "" && s["aws_secret_access_key"] != ""

	switch s["active_provider"] {
	case "resend":
		if !resendOK {
			return nil, ErrNoProvider
		}
		return m.sendViaResend(s["resend_api_key"], opts)
	case "ses":
		if !sesOK {
			return nil, ErrNoProvider
		}
		return sendViaSES(s, opts)
	}

	if resendOK && !(sesOK && m.resendQuotaReached(s)) {
		res, err := m.sendViaResend(s["resend_api_key"], opts)
		if err == nil || !sesOK {
			return res, err
		}
		log.Printf("[MAILER] Resend gagal, beralih ke SES: %v", err)
	}
	if sesOK {
		return sendViaSES(s, opts)
	}
	return nil, ErrNoProvider
}

// resendQuotaReached keeps us under Resend's free tier (100/day, 3000/month) by default.
// Set resend_daily_limit / resend_monthly_limit to 0 to disable.
func (m *Mailer) resendQuotaReached(s map[string]string) bool {
	limit := func(key string, def int) int {
		if v, err := strconv.Atoi(s[key]); err == nil {
			return v
		}
		return def
	}
	now := time.Now()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	if l := limit("resend_daily_limit", 95); l > 0 && m.db.CountSentByProvider("resend", day) >= l {
		return true
	}
	if l := limit("resend_monthly_limit", 2800); l > 0 && m.db.CountSentByProvider("resend", month) >= l {
		return true
	}
	return false
}

/* ---------- Resend ---------- */

// ResendDo calls the Resend REST API; also used by the CLI for domain setup.
func ResendDo(client *http.Client, apiKey, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, ResendURL+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("resend request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("resend API error (%d): %s", resp.StatusCode, raw)
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (m *Mailer) sendViaResend(apiKey string, opts SendOptions) (*SendResult, error) {
	type att struct {
		Filename string `json:"filename"`
		Content  string `json:"content"`
	}
	payload := map[string]any{
		"from":    formatAddress(opts.FromName, opts.FromAddress),
		"to":      opts.To,
		"subject": opts.Subject,
		"text":    opts.Text,
		"html":    opts.HTML,
	}
	if len(opts.Cc) > 0 {
		payload["cc"] = opts.Cc
	}
	if len(opts.Bcc) > 0 {
		payload["bcc"] = opts.Bcc
	}
	if opts.InReplyTo != "" {
		payload["headers"] = map[string]string{"In-Reply-To": opts.InReplyTo, "References": opts.InReplyTo}
	}
	var atts []att
	for _, a := range opts.Attachments {
		atts = append(atts, att{Filename: a.Filename, Content: base64.StdEncoding.EncodeToString(a.Content)})
	}
	if len(atts) > 0 {
		payload["attachments"] = atts
	}

	var resp struct {
		ID string `json:"id"`
	}
	if err := ResendDo(m.httpClient, apiKey, "POST", "/emails", payload, &resp); err != nil {
		return nil, err
	}
	return &SendResult{Provider: "resend", MessageID: "<" + resp.ID + "@resend>"}, nil
}

/* ---------- Amazon SES (SMTP interface, no AWS SDK) ---------- */

// SESSMTPPassword derives the SES SMTP password from an IAM secret access key
// (AWS docs: "Obtaining Amazon SES SMTP credentials", version 4 algorithm).
func SESSMTPPassword(secretKey, region string) string {
	sign := func(key []byte, msg string) []byte {
		h := hmac.New(sha256.New, key)
		h.Write([]byte(msg))
		return h.Sum(nil)
	}
	sig := sign([]byte("AWS4"+secretKey), "11111111")
	for _, msg := range []string{region, "ses", "aws4_request", "SendRawEmail"} {
		sig = sign(sig, msg)
	}
	return base64.StdEncoding.EncodeToString(append([]byte{0x04}, sig...))
}

func sesHost(region string) string { return "email-smtp." + region + ".amazonaws.com" }

// CheckSES logs in to the SES SMTP endpoint without sending anything.
func CheckSES(region, keyID, secret string) error {
	host := sesHost(region)
	c, err := smtp.Dial(host + ":587")
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
		return err
	}
	if err := c.Auth(smtp.PlainAuth("", keyID, SESSMTPPassword(secret, region), host)); err != nil {
		return err
	}
	return c.Quit()
}

func sendViaSES(s map[string]string, opts SendOptions) (*SendResult, error) {
	region := s["aws_region"]
	host := sesHost(region)
	msgID := fmt.Sprintf("<%s@%s>", uuid.NewString(), domainOf(opts.FromAddress))
	raw, err := BuildMIME(opts, msgID)
	if err != nil {
		return nil, err
	}
	auth := smtp.PlainAuth("", s["aws_access_key_id"], SESSMTPPassword(s["aws_secret_access_key"], region), host)
	if err := smtp.SendMail(host+":587", auth, opts.FromAddress, append(append(append([]string{}, opts.To...), opts.Cc...), opts.Bcc...), raw); err != nil {
		return nil, fmt.Errorf("SES SMTP: %w", err)
	}
	return &SendResult{Provider: "ses", MessageID: msgID}, nil
}

// BuildMIME renders a multipart/mixed RFC 5322 message (text + html alternative, then attachments).
func BuildMIME(opts SendOptions, messageID string) ([]byte, error) {
	var buf bytes.Buffer
	mixed := multipart.NewWriter(&buf)

	// Backstop: a header value can never span lines, whatever the caller passed.
	h := func(k, v string) {
		fmt.Fprintf(&buf, "%s: %s\r\n", k, strings.NewReplacer("\r", "", "\n", "").Replace(v))
	}
	h("From", formatAddress(opts.FromName, opts.FromAddress))
	h("To", strings.Join(opts.To, ", "))
	if len(opts.Cc) > 0 {
		h("Cc", strings.Join(opts.Cc, ", "))
	}
	h("Subject", mime.QEncoding.Encode("utf-8", opts.Subject))
	h("Date", time.Now().Format(time.RFC1123Z))
	h("Message-ID", messageID)
	if opts.InReplyTo != "" {
		h("In-Reply-To", opts.InReplyTo)
		h("References", opts.InReplyTo)
	}
	h("MIME-Version", "1.0")
	h("Content-Type", "multipart/mixed; boundary="+mixed.Boundary())
	buf.WriteString("\r\n")

	altBody := &bytes.Buffer{}
	alt := multipart.NewWriter(altBody)
	for _, p := range []struct{ ct, body string }{{"text/plain", opts.Text}, {"text/html", opts.HTML}} {
		w, err := alt.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {p.ct + "; charset=utf-8"},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, err
		}
		qp := quotedprintable.NewWriter(w)
		qp.Write([]byte(p.body))
		qp.Close()
	}
	alt.Close()

	w, err := mixed.CreatePart(textproto.MIMEHeader{"Content-Type": {"multipart/alternative; boundary=" + alt.Boundary()}})
	if err != nil {
		return nil, err
	}
	w.Write(altBody.Bytes())

	for _, a := range opts.Attachments {
		ct := a.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		name := mime.QEncoding.Encode("utf-8", a.Filename)
		w, err := mixed.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {fmt.Sprintf("%s; name=%q", ct, name)},
			"Content-Disposition":       {fmt.Sprintf("attachment; filename=%q", name)},
			"Content-Transfer-Encoding": {"base64"},
		})
		if err != nil {
			return nil, err
		}
		enc := base64.StdEncoding.EncodeToString(a.Content)
		for len(enc) > 76 {
			w.Write([]byte(enc[:76] + "\r\n"))
			enc = enc[76:]
		}
		w.Write([]byte(enc + "\r\n"))
	}
	mixed.Close()
	return buf.Bytes(), nil
}

func formatAddress(name, addr string) string {
	return (&mail.Address{Name: name, Address: addr}).String()
}

func domainOf(addr string) string {
	if at := strings.LastIndexByte(addr, '@'); at >= 0 {
		return addr[at+1:]
	}
	return "wmail.local"
}

// CleanSnippet flattens text for list previews. Like Gmail it leaves quoted replies
// out: "> " lines and the "X menulis:" / "X wrote:" line just above them.
func CleanSnippet(text string, maxLen int) string {
	var kept []string
	inQuote := false
	for _, l := range strings.Split(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), ">") {
			kept, inQuote = append(kept, l), false
			continue
		}
		if !inQuote {
			for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
				kept = kept[:len(kept)-1]
			}
			if n := len(kept); n > 0 && strings.HasSuffix(strings.TrimSpace(kept[n-1]), ":") {
				kept = kept[:n-1]
			}
		}
		inQuote = true
	}
	text = strings.Join(strings.Fields(strings.Join(kept, "\n")), " ")
	if r := []rune(text); len(r) > maxLen {
		return string(r[:maxLen]) + "..."
	}
	return text
}

// CleanMsgID returns s if it is a single "<...>" msg-id without whitespace or control
// characters, otherwise "". In-Reply-To values come from clients and from decoded
// inbound Message-IDs, so they must never carry CR/LF into outbound headers.
func CleanMsgID(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 3 || len(s) > 998 || s[0] != '<' || s[len(s)-1] != '>' ||
		strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r == 0x7f }) || strings.Count(s, "<") != 1 {
		return ""
	}
	return s
}
