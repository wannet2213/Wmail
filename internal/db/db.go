package db

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"wmail/internal/models"

	_ "modernc.org/sqlite"
)

var (
	ErrDuplicate    = errors.New("duplicate message")
	ErrBadLogin     = errors.New("invalid username or password")
	ErrLastAdmin    = errors.New("cannot remove the last admin")
	ErrInvalidInput = errors.New("invalid input")
)

type DB struct {
	conn *sql.DB
}

func InitDB(dbPath string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create db dir: %w", err)
	}

	conn, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite: %w", err)
	}
	if err := conn.Ping(); err != nil {
		return nil, fmt.Errorf("failed to ping sqlite: %w", err)
	}

	d := &DB{conn: conn}
	if err := d.migrate(); err != nil {
		return nil, fmt.Errorf("failed to migrate db: %w", err)
	}
	return d, nil
}

func (d *DB) Close() error {
	return d.conn.Close()
}

// migrations run in order; PRAGMA user_version records how many have been applied.
var migrations = []string{
	`
	CREATE TABLE IF NOT EXISTS emails (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		message_id TEXT,
		in_reply_to TEXT,
		thread_id TEXT,
		from_name TEXT,
		from_email TEXT NOT NULL,
		to_recipients TEXT NOT NULL,
		cc_recipients TEXT,
		subject TEXT NOT NULL,
		snippet TEXT,
		body_text TEXT,
		body_html TEXT,
		is_unread INTEGER DEFAULT 1,
		is_starred INTEGER DEFAULT 0,
		is_archived INTEGER DEFAULT 0,
		is_deleted INTEGER DEFAULT 0,
		folder TEXT DEFAULT 'inbox',
		category TEXT,
		label TEXT,
		has_attachments INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_emails_folder ON emails(folder);
	CREATE INDEX IF NOT EXISTS idx_emails_is_deleted ON emails(is_deleted);
	CREATE INDEX IF NOT EXISTS idx_emails_is_starred ON emails(is_starred);
	CREATE INDEX IF NOT EXISTS idx_emails_created_at ON emails(created_at DESC);
	CREATE TABLE IF NOT EXISTS attachments (
		id TEXT PRIMARY KEY,
		email_id INTEGER NOT NULL,
		filename TEXT NOT NULL,
		content_type TEXT NOT NULL,
		size_bytes INTEGER NOT NULL,
		file_path TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (email_id) REFERENCES emails(id) ON DELETE CASCADE
	);
	CREATE INDEX IF NOT EXISTS idx_attachments_email_id ON attachments(email_id);
	CREATE TABLE IF NOT EXISTS settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`,
	`
	CREATE TABLE users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT NOT NULL UNIQUE COLLATE NOCASE,
		password_hash TEXT NOT NULL,
		is_admin INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE sessions (
		token_hash TEXT PRIMARY KEY,
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		expires_at INTEGER NOT NULL
	);
	CREATE TABLE mailboxes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		address TEXT NOT NULL UNIQUE COLLATE NOCASE,
		display_name TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE user_mailboxes (
		user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		mailbox_id INTEGER NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
		can_send INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (user_id, mailbox_id)
	);
	ALTER TABLE emails ADD COLUMN mailbox_id INTEGER REFERENCES mailboxes(id) ON DELETE CASCADE;
	ALTER TABLE emails ADD COLUMN provider TEXT NOT NULL DEFAULT '';
	CREATE INDEX idx_emails_mailbox ON emails(mailbox_id);
	CREATE UNIQUE INDEX idx_emails_mailbox_msgid ON emails(mailbox_id, message_id) WHERE message_id != '';`,
}

func (d *DB) migrate() error {
	var version int
	if err := d.conn.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := d.conn.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

/* ---------- emails ---------- */

const emailCols = `id, COALESCE(mailbox_id, 0), provider, message_id, in_reply_to, thread_id, from_name, from_email,
	to_recipients, cc_recipients, subject, snippet, body_text,
	body_html, is_unread, is_starred, is_archived, is_deleted,
	folder, category, label, has_attachments, created_at, updated_at`

type scanner interface{ Scan(dest ...any) error }

func scanEmail(row scanner) (*models.Email, error) {
	var m models.Email
	var toJSON, ccJSON, msgID, inReplyTo, threadID, fromName, snippet, bodyText, bodyHTML, category, label sql.NullString
	var unread, starred, archived, deleted, hasAtt int
	err := row.Scan(
		&m.ID, &m.MailboxID, &m.Provider, &msgID, &inReplyTo, &threadID, &fromName, &m.FromEmail,
		&toJSON, &ccJSON, &m.Subject, &snippet, &bodyText,
		&bodyHTML, &unread, &starred, &archived, &deleted,
		&m.Folder, &category, &label, &hasAtt, &m.CreatedAt, &m.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	m.MessageID, m.InReplyTo, m.ThreadID, m.FromName = msgID.String, inReplyTo.String, threadID.String, fromName.String
	m.Snippet, m.BodyText, m.BodyHTML, m.Category, m.Label = snippet.String, bodyText.String, bodyHTML.String, category.String, label.String
	m.IsUnread, m.IsStarred, m.IsArchived, m.IsDeleted, m.HasAttachments = unread == 1, starred == 1, archived == 1, deleted == 1, hasAtt == 1
	if toJSON.String != "" {
		_ = json.Unmarshal([]byte(toJSON.String), &m.ToRecipients)
	}
	if ccJSON.String != "" {
		_ = json.Unmarshal([]byte(ccJSON.String), &m.CcRecipients)
	}
	return &m, nil
}

// SaveEmail inserts the email and its attachment rows. Returns ErrDuplicate when
// the mailbox already holds a message with the same Message-ID.
func (d *DB) SaveEmail(m *models.Email) error {
	toJSON, _ := json.Marshal(m.ToRecipients)
	ccJSON, _ := json.Marshal(m.CcRecipients)
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	m.UpdatedAt = time.Now()
	m.HasAttachments = m.HasAttachments || len(m.Attachments) > 0

	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`
	INSERT OR IGNORE INTO emails (
		mailbox_id, provider, message_id, in_reply_to, thread_id, from_name, from_email,
		to_recipients, cc_recipients, subject, snippet, body_text,
		body_html, is_unread, is_starred, is_archived, is_deleted,
		folder, category, label, has_attachments, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.MailboxID, m.Provider, m.MessageID, m.InReplyTo, m.ThreadID, m.FromName, m.FromEmail,
		string(toJSON), string(ccJSON), m.Subject, m.Snippet, m.BodyText,
		m.BodyHTML, boolToInt(m.IsUnread), boolToInt(m.IsStarred), boolToInt(m.IsArchived), boolToInt(m.IsDeleted),
		m.Folder, m.Category, m.Label, boolToInt(m.HasAttachments), m.CreatedAt, m.UpdatedAt,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrDuplicate
	}
	if m.ID, err = res.LastInsertId(); err != nil {
		return err
	}

	for i := range m.Attachments {
		a := &m.Attachments[i]
		a.EmailID = m.ID
		if a.CreatedAt.IsZero() {
			a.CreatedAt = time.Now()
		}
		if _, err := tx.Exec(`INSERT INTO attachments (id, email_id, filename, content_type, size_bytes, file_path, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, a.ID, a.EmailID, a.Filename, a.ContentType, a.SizeBytes, a.FilePath, a.CreatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) EmailExists(mailboxID int64, messageID string) bool {
	var n int
	_ = d.conn.QueryRow("SELECT COUNT(*) FROM emails WHERE mailbox_id = ? AND message_id = ?", mailboxID, messageID).Scan(&n)
	return n > 0
}

func (d *DB) GetEmail(id int64) (*models.Email, error) {
	m, err := scanEmail(d.conn.QueryRow("SELECT "+emailCols+" FROM emails WHERE id = ?", id))
	if err != nil {
		return nil, err
	}

	rows, err := d.conn.Query(`SELECT id, email_id, filename, content_type, size_bytes, file_path, created_at FROM attachments WHERE email_id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a models.Attachment
		if err := rows.Scan(&a.ID, &a.EmailID, &a.Filename, &a.ContentType, &a.SizeBytes, &a.FilePath, &a.CreatedAt); err == nil {
			m.Attachments = append(m.Attachments, a)
		}
	}
	return m, rows.Err()
}

func (d *DB) ListEmails(mailboxID int64, viewFolder, queryStr string, limit, offset int) ([]models.Email, int, error) {
	conditions := []string{"mailbox_id = ?"}
	args := []any{mailboxID}

	switch viewFolder {
	case "starred":
		conditions = append(conditions, "is_starred = 1 AND is_deleted = 0")
	case "snoozed", "sent", "drafts", "spam":
		conditions = append(conditions, "folder = ? AND is_deleted = 0")
		args = append(args, viewFolder)
	case "important":
		conditions = append(conditions, "(label = 'Important' OR is_starred = 1) AND is_deleted = 0")
	case "trash":
		conditions = append(conditions, "is_deleted = 1")
	case "all":
		conditions = append(conditions, "is_deleted = 0")
	default:
		if name, ok := strings.CutPrefix(viewFolder, "label:"); ok {
			conditions = append(conditions, "label = ? AND is_deleted = 0")
			args = append(args, name)
		} else if name, ok := strings.CutPrefix(viewFolder, "category:"); ok {
			conditions = append(conditions, "category = ? AND is_deleted = 0")
			args = append(args, name)
		} else {
			conditions = append(conditions, "folder = 'inbox' AND is_archived = 0 AND is_deleted = 0")
		}
	}

	if queryStr != "" {
		conditions = append(conditions, "(subject LIKE ? OR from_name LIKE ? OR from_email LIKE ? OR snippet LIKE ? OR body_text LIKE ?)")
		q := "%" + queryStr + "%"
		args = append(args, q, q, q, q, q)
	}
	where := "WHERE " + strings.Join(conditions, " AND ")

	var total int
	if err := d.conn.QueryRow("SELECT COUNT(*) FROM emails "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 50
	}

	rows, err := d.conn.Query("SELECT "+emailCols+" FROM emails "+where+" ORDER BY created_at DESC LIMIT ? OFFSET ?", append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	list := []models.Email{}
	for rows.Next() {
		m, err := scanEmail(rows)
		if err != nil {
			return nil, 0, err
		}
		list = append(list, *m)
	}
	return list, total, rows.Err()
}

func (d *DB) UpdateEmailStatus(id int64, req models.UpdateStatusRequest) error {
	var sets []string
	var args []any
	add := func(col string, v any) { sets = append(sets, col+" = ?"); args = append(args, v) }

	if req.IsUnread != nil {
		add("is_unread", boolToInt(*req.IsUnread))
	}
	if req.IsStarred != nil {
		add("is_starred", boolToInt(*req.IsStarred))
	}
	if req.IsArchived != nil {
		add("is_archived", boolToInt(*req.IsArchived))
	}
	if req.IsDeleted != nil {
		add("is_deleted", boolToInt(*req.IsDeleted))
	}
	if req.Folder != nil {
		add("folder", *req.Folder)
	}
	if req.Label != nil {
		add("label", *req.Label)
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = CURRENT_TIMESTAMP")
	_, err := d.conn.Exec("UPDATE emails SET "+strings.Join(sets, ", ")+" WHERE id = ?", append(args, id)...)
	return err
}

// deleteEmailsWhere removes matching emails and their attachment files on disk.
func (d *DB) deleteEmailsWhere(where string, args ...any) error {
	rows, err := d.conn.Query("SELECT file_path FROM attachments WHERE email_id IN (SELECT id FROM emails WHERE "+where+")", args...)
	if err != nil {
		return err
	}
	var files []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			files = append(files, p)
		}
	}
	rows.Close()

	if _, err := d.conn.Exec("DELETE FROM emails WHERE "+where, args...); err != nil {
		return err
	}
	for _, p := range files {
		_ = os.Remove(p)
	}
	return nil
}

func (d *DB) DeleteEmail(id int64) error {
	return d.deleteEmailsWhere("id = ?", id)
}

func (d *DB) EmptyTrash(mailboxID int64) error {
	return d.deleteEmailsWhere("mailbox_id = ? AND is_deleted = 1", mailboxID)
}

func (d *DB) GetAttachment(id string) (*models.Attachment, error) {
	var a models.Attachment
	err := d.conn.QueryRow(`SELECT id, email_id, filename, content_type, size_bytes, file_path, created_at FROM attachments WHERE id = ?`, id).
		Scan(&a.ID, &a.EmailID, &a.Filename, &a.ContentType, &a.SizeBytes, &a.FilePath, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (d *DB) GetStats(mailboxID int64) (*models.Stats, error) {
	s := &models.Stats{}
	err := d.conn.QueryRow(`SELECT
		COALESCE(SUM(folder = 'inbox' AND is_unread = 1 AND is_deleted = 0 AND is_archived = 0), 0),
		COALESCE(SUM(is_starred = 1 AND is_deleted = 0), 0),
		COALESCE(SUM(folder = 'sent' AND is_deleted = 0), 0),
		COALESCE(SUM(folder = 'drafts' AND is_deleted = 0), 0),
		COALESCE(SUM(is_deleted = 1), 0)
		FROM emails WHERE mailbox_id = ?`, mailboxID).
		Scan(&s.InboxUnread, &s.StarredCount, &s.SentCount, &s.DraftsCount, &s.TrashCount)
	return s, err
}

// CountSentByProvider counts sent messages handled by provider since t (used for Resend quota).
func (d *DB) CountSentByProvider(provider string, since time.Time) int {
	var n int
	_ = d.conn.QueryRow("SELECT COUNT(*) FROM emails WHERE folder = 'sent' AND provider = ? AND created_at >= ?", provider, since).Scan(&n)
	return n
}

/* ---------- settings ---------- */

func (d *DB) GetSettings() (map[string]string, error) {
	rows, err := d.conn.Query("SELECT key, value FROM settings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	s := map[string]string{}
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			s[k] = v
		}
	}
	return s, rows.Err()
}

func (d *DB) GetSetting(key string) string {
	var v string
	_ = d.conn.QueryRow("SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	return v
}

func (d *DB) SetSetting(key, value string) error {
	_, err := d.conn.Exec(`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`, key, value)
	return err
}

/* ---------- users & passwords ---------- */

const pbkdf2Iter = 600_000

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

func checkPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	enc := base64.RawStdEncoding
	salt, err2 := enc.DecodeString(parts[2])
	want, err3 := enc.DecodeString(parts[3])
	if err != nil || err2 != nil || err3 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash keeps Authenticate's timing the same for unknown usernames.
var dummyHash = sync.OnceValue(func() string { h, _ := hashPassword("wmail-dummy"); return h })

func validPassword(pw string) error {
	if len(pw) < 8 {
		return fmt.Errorf("%w: password minimal 8 karakter", ErrInvalidInput)
	}
	return nil
}

func (d *DB) CreateUser(username, password string, isAdmin bool) (int64, error) {
	username = strings.TrimSpace(username)
	if username == "" || strings.ContainsAny(username, " \t@") {
		return 0, fmt.Errorf("%w: username tidak valid", ErrInvalidInput)
	}
	if err := validPassword(password); err != nil {
		return 0, err
	}
	h, err := hashPassword(password)
	if err != nil {
		return 0, err
	}
	res, err := d.conn.Exec("INSERT INTO users (username, password_hash, is_admin) VALUES (?, ?, ?)", username, h, boolToInt(isAdmin))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) Authenticate(username, password string) (*models.User, error) {
	var u models.User
	var h string
	var admin int
	err := d.conn.QueryRow("SELECT id, username, password_hash, is_admin, created_at FROM users WHERE username = ?", strings.TrimSpace(username)).
		Scan(&u.ID, &u.Username, &h, &admin, &u.CreatedAt)
	if err != nil {
		checkPassword(dummyHash(), password)
		return nil, ErrBadLogin
	}
	if !checkPassword(h, password) {
		return nil, ErrBadLogin
	}
	u.IsAdmin = admin == 1
	return &u, nil
}

func (d *DB) GetUser(id int64) (*models.User, error) {
	return d.userWhere("id = ?", id)
}

func (d *DB) GetUserByName(username string) (*models.User, error) {
	return d.userWhere("username = ?", username)
}

func (d *DB) userWhere(where string, arg any) (*models.User, error) {
	var u models.User
	var admin int
	err := d.conn.QueryRow("SELECT id, username, is_admin, created_at FROM users WHERE "+where, arg).Scan(&u.ID, &u.Username, &admin, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	u.IsAdmin = admin == 1
	return &u, nil
}

func (d *DB) ListUsers() ([]models.User, error) {
	rows, err := d.conn.Query("SELECT id, username, is_admin, created_at FROM users ORDER BY username")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []models.User{}
	for rows.Next() {
		var u models.User
		var admin int
		if err := rows.Scan(&u.ID, &u.Username, &admin, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.IsAdmin = admin == 1
		list = append(list, u)
	}
	return list, rows.Err()
}

func (d *DB) CountAdmins() int {
	var n int
	_ = d.conn.QueryRow("SELECT COUNT(*) FROM users WHERE is_admin = 1").Scan(&n)
	return n
}

func (d *DB) isLastAdmin(id int64) bool {
	u, err := d.GetUser(id)
	return err == nil && u.IsAdmin && d.CountAdmins() <= 1
}

func (d *DB) DeleteUser(id int64) error {
	if d.isLastAdmin(id) {
		return ErrLastAdmin
	}
	_, err := d.conn.Exec("DELETE FROM users WHERE id = ?", id)
	return err
}

func (d *DB) SetAdmin(id int64, isAdmin bool) error {
	if !isAdmin && d.isLastAdmin(id) {
		return ErrLastAdmin
	}
	_, err := d.conn.Exec("UPDATE users SET is_admin = ? WHERE id = ?", boolToInt(isAdmin), id)
	return err
}

// SetPassword changes the password and signs the user out everywhere.
func (d *DB) SetPassword(id int64, password string) error {
	if err := validPassword(password); err != nil {
		return err
	}
	h, err := hashPassword(password)
	if err != nil {
		return err
	}
	if _, err := d.conn.Exec("UPDATE users SET password_hash = ? WHERE id = ?", h, id); err != nil {
		return err
	}
	_, err = d.conn.Exec("DELETE FROM sessions WHERE user_id = ?", id)
	return err
}

/* ---------- sessions (only sha256 of the token is stored) ---------- */

func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (d *DB) CreateSession(userID int64, ttl time.Duration) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	_, _ = d.conn.Exec("DELETE FROM sessions WHERE expires_at < ?", time.Now().Unix())
	_, err := d.conn.Exec("INSERT INTO sessions (token_hash, user_id, expires_at) VALUES (?, ?, ?)", TokenHash(token), userID, time.Now().Add(ttl).Unix())
	return token, err
}

func (d *DB) UserBySession(token string) (*models.User, error) {
	var id int64
	err := d.conn.QueryRow("SELECT user_id FROM sessions WHERE token_hash = ? AND expires_at > ?", TokenHash(token), time.Now().Unix()).Scan(&id)
	if err != nil {
		return nil, err
	}
	return d.GetUser(id)
}

func (d *DB) DeleteSession(token string) error {
	_, err := d.conn.Exec("DELETE FROM sessions WHERE token_hash = ?", TokenHash(token))
	return err
}

/* ---------- mailboxes & grants ---------- */

func NormalizeAddress(addr string) string {
	return strings.ToLower(strings.TrimSpace(addr))
}

func (d *DB) CreateMailbox(address, displayName string) (int64, error) {
	address = NormalizeAddress(address)
	if at := strings.IndexByte(address, '@'); at < 1 || at == len(address)-1 || strings.ContainsAny(address, " <>,;") {
		return 0, fmt.Errorf("%w: alamat email tidak valid", ErrInvalidInput)
	}
	res, err := d.conn.Exec("INSERT INTO mailboxes (address, display_name) VALUES (?, ?)", address, strings.TrimSpace(displayName))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) UpdateMailboxName(id int64, displayName string) error {
	_, err := d.conn.Exec("UPDATE mailboxes SET display_name = ? WHERE id = ?", strings.TrimSpace(displayName), id)
	return err
}

func (d *DB) DeleteMailbox(id int64) error {
	if err := d.deleteEmailsWhere("mailbox_id = ?", id); err != nil {
		return err
	}
	_, err := d.conn.Exec("DELETE FROM mailboxes WHERE id = ?", id)
	return err
}

func (d *DB) MailboxByAddress(address string) (*models.Mailbox, error) {
	var m models.Mailbox
	err := d.conn.QueryRow("SELECT id, address, display_name FROM mailboxes WHERE address = ?", NormalizeAddress(address)).Scan(&m.ID, &m.Address, &m.DisplayName)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (d *DB) ListMailboxes() ([]models.Mailbox, error) {
	return d.queryMailboxes("SELECT id, address, display_name, 1 FROM mailboxes ORDER BY address")
}

// MailboxesForUser returns what u may open; admins get everything with send rights.
func (d *DB) MailboxesForUser(u *models.User) ([]models.Mailbox, error) {
	if u.IsAdmin {
		return d.ListMailboxes()
	}
	return d.queryMailboxes(`SELECT m.id, m.address, m.display_name, g.can_send FROM mailboxes m
		JOIN user_mailboxes g ON g.mailbox_id = m.id WHERE g.user_id = ? ORDER BY m.address`, u.ID)
}

func (d *DB) queryMailboxes(q string, args ...any) ([]models.Mailbox, error) {
	rows, err := d.conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []models.Mailbox{}
	for rows.Next() {
		var m models.Mailbox
		var canSend int
		if err := rows.Scan(&m.ID, &m.Address, &m.DisplayName, &canSend); err != nil {
			return nil, err
		}
		m.CanSend = canSend == 1
		list = append(list, m)
	}
	return list, rows.Err()
}

// Access reports whether u may read mailboxID, and whether it may send from it.
func (d *DB) Access(u *models.User, mailboxID int64) (mb *models.Mailbox, canSend bool) {
	var m models.Mailbox
	var send int
	var err error
	if u.IsAdmin {
		err = d.conn.QueryRow("SELECT id, address, display_name, 1 FROM mailboxes WHERE id = ?", mailboxID).Scan(&m.ID, &m.Address, &m.DisplayName, &send)
	} else {
		err = d.conn.QueryRow(`SELECT m.id, m.address, m.display_name, g.can_send FROM mailboxes m
			JOIN user_mailboxes g ON g.mailbox_id = m.id WHERE g.user_id = ? AND m.id = ?`, u.ID, mailboxID).Scan(&m.ID, &m.Address, &m.DisplayName, &send)
	}
	if err != nil {
		return nil, false
	}
	m.CanSend = send == 1
	return &m, m.CanSend
}

func (d *DB) SetGrant(userID, mailboxID int64, canSend bool) error {
	_, err := d.conn.Exec(`INSERT INTO user_mailboxes (user_id, mailbox_id, can_send) VALUES (?, ?, ?)
		ON CONFLICT(user_id, mailbox_id) DO UPDATE SET can_send = excluded.can_send`, userID, mailboxID, boolToInt(canSend))
	return err
}

func (d *DB) DeleteGrant(userID, mailboxID int64) error {
	_, err := d.conn.Exec("DELETE FROM user_mailboxes WHERE user_id = ? AND mailbox_id = ?", userID, mailboxID)
	return err
}

func (d *DB) ListGrants() ([]models.Grant, error) {
	rows, err := d.conn.Query("SELECT user_id, mailbox_id, can_send FROM user_mailboxes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []models.Grant{}
	for rows.Next() {
		var g models.Grant
		var canSend int
		if err := rows.Scan(&g.UserID, &g.MailboxID, &canSend); err != nil {
			return nil, err
		}
		g.CanSend = canSend == 1
		list = append(list, g)
	}
	return list, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

/* ---------- known settings keys (shared by CLI `config` and the admin page) ---------- */

type SettingKey struct {
	Key, Desc   string
	Secret, Web bool // Secret: masked on display. Web: editable from the admin page.
}

var SettingKeys = []SettingKey{
	{Key: "domain", Desc: "Domain email, mis. contoh.com"},
	{Key: "public_url", Desc: "URL publik web (tunnel), mis. https://mail.contoh.com"},
	{Key: "active_provider", Desc: "auto | resend | ses", Web: true},
	{Key: "resend_api_key", Desc: "API key Resend (re_...)", Secret: true, Web: true},
	{Key: "resend_daily_limit", Desc: "Batas kirim Resend per hari sebelum beralih ke SES (0 = tanpa batas, default 95)", Web: true},
	{Key: "resend_monthly_limit", Desc: "Batas kirim Resend per bulan (0 = tanpa batas, default 2800)", Web: true},
	{Key: "aws_region", Desc: "Region SES, mis. ap-southeast-1", Web: true},
	{Key: "aws_access_key_id", Desc: "IAM Access Key ID (izin ses:SendRawEmail)", Web: true},
	{Key: "aws_secret_access_key", Desc: "IAM Secret Access Key", Secret: true, Web: true},
	{Key: "inbound_secret", Desc: "Secret bersama Worker <-> server", Secret: true},
	{Key: "cf_api_token", Desc: "Cloudflare API token", Secret: true},
	{Key: "cf_account_id", Desc: "Cloudflare account id"},
	{Key: "cf_zone_id", Desc: "Cloudflare zone id"},
	{Key: "cf_kv_namespace_id", Desc: "KV namespace antrean cadangan"},
	{Key: "cf_tunnel_id", Desc: "Cloudflare Tunnel id"},
	{Key: "cf_tunnel_token", Desc: "Token cloudflared", Secret: true},
}

func LookupSettingKey(key string) (SettingKey, bool) {
	for _, k := range SettingKeys {
		if k.Key == key {
			return k, true
		}
	}
	return SettingKey{}, false
}

const MaskMarker = "••••"

func MaskSecret(v string) string {
	if v == "" {
		return ""
	}
	if len(v) <= 8 {
		return MaskMarker
	}
	return v[:3] + MaskMarker + v[len(v)-4:]
}
