package service

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wmail/internal/cf"
	"wmail/internal/db"
	"wmail/internal/models"

	"github.com/google/uuid"
	"github.com/jhillyerd/enmime/v2"
)

var (
	ErrUnknownMailbox = errors.New("no such mailbox")
	// ErrRejected marks a message that can never be ingested; retrying it is pointless.
	ErrRejected = errors.New("message rejected")
)

// maxKVAttempts bounds retries of a parked message before it is quarantined (1 hour at 5-minute drains).
const maxKVAttempts = 12

type InboundService struct {
	dataDir  string
	db       *db.DB
	attempts map[string]int // KV key -> failed drains; only the RunKVDrain goroutine touches it
}

func NewInboundService(dataDir string, database *db.DB) *InboundService {
	return &InboundService{dataDir: dataDir, db: database, attempts: map[string]int{}}
}

// Ingest parses a raw RFC 5322 message and stores it in the mailbox of envelopeTo.
// Cloudflare invokes the Worker once per envelope recipient, so one call = one mailbox.
func (s *InboundService) Ingest(raw []byte, envelopeTo string) (*models.Email, error) {
	mb, err := s.db.MailboxByAddress(envelopeTo)
	if local, domain, ok := strings.Cut(envelopeTo, "@"); err != nil && ok && strings.Contains(local, "+") {
		// Subaddressing: info+promo@domain delivers to info@domain.
		base, _, _ := strings.Cut(local, "+")
		mb, err = s.db.MailboxByAddress(base + "@" + domain)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnknownMailbox
	}
	if err != nil {
		return nil, err // transient DB error: let the Worker park and retry instead of bouncing
	}
	env, err := enmime.ReadEnvelope(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: parse: %v", ErrRejected, err)
	}

	msgID := strings.TrimSpace(env.GetHeader("Message-ID"))
	if msgID == "" {
		msgID = fmt.Sprintf("<%s@inbound>", uuid.NewString())
	}
	if s.db.EmailExists(mb.ID, msgID) {
		return nil, db.ErrDuplicate
	}

	email := models.Email{
		MailboxID: mb.ID,
		MessageID: msgID,
		InReplyTo: strings.TrimSpace(env.GetHeader("In-Reply-To")),
		Subject:   env.GetHeader("Subject"),
		BodyText:  env.Text,
		BodyHTML:  env.HTML,
		Snippet:   CleanSnippet(env.Text, 120),
		IsUnread:  true,
		Folder:    "inbox",
		CreatedAt: time.Now(),
	}
	if email.Subject == "" {
		email.Subject = "(no subject)"
	}
	if t, err := env.Date(); err == nil {
		email.CreatedAt = t
	}
	if from, err := env.AddressList("From"); err == nil && len(from) > 0 {
		email.FromName, email.FromEmail = from[0].Name, from[0].Address
	}
	if email.FromName == "" {
		email.FromName = email.FromEmail
	}
	if email.FromEmail == "" {
		email.FromEmail = "unknown"
	}
	email.ToRecipients = addressList(env, "To")
	email.CcRecipients = addressList(env, "Cc")
	if len(email.ToRecipients) == 0 {
		email.ToRecipients = []string{mb.Address}
	}

	if err := os.MkdirAll(s.dataDir, 0750); err != nil {
		return nil, fmt.Errorf("create attachments dir: %w", err)
	}
	for _, p := range append(env.Attachments, env.Inlines...) {
		att, err := SaveAttachmentFile(s.dataDir, p.FileName, p.ContentType, p.Content)
		if err != nil {
			removeFiles(email.Attachments)
			return nil, err
		}
		email.Attachments = append(email.Attachments, att)
	}

	if err := s.db.SaveEmail(&email); err != nil {
		removeFiles(email.Attachments)
		return nil, err
	}
	return &email, nil
}

func removeFiles(atts []models.Attachment) {
	for _, a := range atts {
		os.Remove(a.FilePath)
	}
}

func addressList(env *enmime.Envelope, key string) []string {
	list, err := env.AddressList(key)
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range list {
		out = append(out, a.Address)
	}
	return out
}

// SaveAttachmentFile writes content as <uuid> under dir. The sender-chosen name lives only in
// the DB row, so it can never make the write fail (length, NUL) or shape the path.
func SaveAttachmentFile(dir, filename, contentType string, content []byte) (models.Attachment, error) {
	name := filepath.Base(filename)
	if name == "" || name == "." || name == "/" {
		name = "attachment"
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	id := uuid.NewString()
	path := filepath.Join(dir, id)
	if err := os.WriteFile(path, content, 0640); err != nil {
		return models.Attachment{}, fmt.Errorf("write attachment: %w", err)
	}
	return models.Attachment{ID: id, Filename: name, ContentType: contentType, SizeBytes: int64(len(content)), FilePath: path, CreatedAt: time.Now()}, nil
}

/* ---------- KV fallback: mail the Worker parked while this server was unreachable ---------- */

func (s *InboundService) DrainKV() {
	set, err := s.db.GetSettings()
	if err != nil {
		return
	}
	account, ns, token := set["cf_account_id"], set["cf_kv_namespace_id"], set["cf_api_token"]
	if account == "" || ns == "" || token == "" {
		return
	}
	c := cf.New(token)
	keys, err := c.KVList(account, ns)
	if err != nil {
		log.Printf("[KV] list gagal: %v", err)
		return
	}
	for _, k := range keys {
		raw, err := c.KVGet(account, ns, k.Name)
		if err != nil {
			log.Printf("[KV] ambil %s gagal: %v", k.Name, err)
			continue
		}
		_, err = s.Ingest(raw, k.Metadata["to"])
		switch {
		case err == nil:
			log.Printf("[KV] email untuk %s diambil dari antrean cadangan", k.Metadata["to"])
		case errors.Is(err, ErrUnknownMailbox):
			log.Printf("[KV] dibuang: mailbox %q tidak ada", k.Metadata["to"])
		case errors.Is(err, db.ErrDuplicate):
		default:
			if s.attempts[k.Name]++; !errors.Is(err, ErrRejected) && s.attempts[k.Name] < maxKVAttempts {
				log.Printf("[KV] ingest %s gagal (%d/%d), dicoba lagi nanti: %v", k.Name, s.attempts[k.Name], maxKVAttempts, err)
				continue
			}
			// ponytail: attempt counts live in memory and restart from zero after a server restart.
			path, qerr := s.quarantine(raw)
			if qerr != nil {
				log.Printf("[KV] karantina %s gagal, dicoba lagi nanti: %v", k.Name, qerr)
				continue
			}
			log.Printf("[KV] %s gagal permanen (%v), disimpan di %s", k.Name, err, path)
		}
		delete(s.attempts, k.Name)
		if err := c.KVDelete(account, ns, k.Name); err != nil {
			log.Printf("[KV] hapus %s gagal: %v", k.Name, err)
		}
	}
}

// quarantine keeps an unprocessable message as an .eml next to the attachments dir for the operator.
func (s *InboundService) quarantine(raw []byte) (string, error) {
	dir := filepath.Join(filepath.Dir(s.dataDir), "kv-failed")
	if err := os.MkdirAll(dir, 0750); err != nil {
		return "", err
	}
	path := filepath.Join(dir, time.Now().Format("20060102-150405")+"-"+uuid.NewString()+".eml")
	return path, os.WriteFile(path, raw, 0640)
}

func (s *InboundService) RunKVDrain(ctx context.Context, every time.Duration) {
	s.DrainKV()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.DrainKV()
		}
	}
}
