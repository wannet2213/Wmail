package db

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"wmail/internal/models"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

var (
	ErrDuplicate    = errors.New("duplicate message")
	ErrBadLogin     = errors.New("invalid username or password")
	ErrLastAdmin    = errors.New("cannot remove the last admin")
	ErrInvalidInput = errors.New("invalid input")
)

type DB struct {
	conn *sqlDB
}

// IsPostgres reports whether dsn names a PostgreSQL server rather than a SQLite file.
func IsPostgres(dsn string) bool {
	return strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://")
}

// InitDB opens a SQLite file, or PostgreSQL when dsn is a postgres:// URL, and brings
// its schema up to date.
func InitDB(dsn string) (*DB, error) {
	pg := IsPostgres(dsn)
	var conn *sql.DB
	var err error
	if pg {
		if host, err := plaintextRemote(dsn); err != nil {
			return nil, err
		} else if host != "" {
			return nil, fmt.Errorf("koneksi PostgreSQL ke %s bisa berjalan tanpa enkripsi: tambahkan sslmode=verify-full ke URL "+
				"(sslmode=require juga diterima: terenkripsi, tapi sertifikat server tidak diperiksa)", host)
		}
		conn, err = sql.Open("pgx", dsn)
	} else {
		if err := os.MkdirAll(filepath.Dir(dsn), 0755); err != nil {
			return nil, fmt.Errorf("failed to create db dir: %w", err)
		}
		// _time_format/_timezone make every datetime round-trip as canonical UTC text
		// ("2006-01-02 15:04:05.999999999-07:00"). Without them the driver writes
		// t.String(), and times parsed from Date headers with a numeric zone (e.g.
		// "+0700 +0700") come back unparseable — one such row kills every listing.
		conn, err = sql.Open("sqlite", dsn+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_time_format=sqlite&_timezone=UTC")
	}
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	if err := conn.Ping(); err != nil {
		return nil, fmt.Errorf("failed to reach database: %w", err)
	}

	d := &DB{conn: &sqlDB{conn, pg}}
	if err := d.migrate(); err != nil {
		return nil, fmt.Errorf("failed to migrate db: %w", err)
	}
	return d, nil
}

func (d *DB) Close() error {
	return d.conn.Close()
}

// plaintextRemote returns a host outside this machine that the connection could reach without
// TLS. pgx's default sslmode=prefer falls back to plaintext when TLS fails, and anyone on the
// network path can make it fail.
func plaintextRemote(dsn string) (string, error) {
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return "", err
	}
	tries := append([]*pgconn.FallbackConfig{{Host: cfg.Host, TLSConfig: cfg.TLSConfig}}, cfg.Fallbacks...)
	for _, t := range tries {
		local := strings.HasPrefix(t.Host, "/") || t.Host == "localhost" || net.ParseIP(t.Host).IsLoopback()
		if t.TLSConfig == nil && !local {
			return t.Host, nil
		}
	}
	return "", nil
}

// sqlDB and sqlTx let every query be written once with ? placeholders: on Postgres
// they are rewritten to $1, $2, ... before reaching the driver.
type sqlDB struct {
	*sql.DB
	pg bool
}

type sqlTx struct {
	*sql.Tx
	pg bool
}

func (c *sqlDB) Exec(q string, args ...any) (sql.Result, error) {
	return c.DB.Exec(rebind(c.pg, q), args...)
}
func (c *sqlDB) Query(q string, args ...any) (*sql.Rows, error) {
	return c.DB.Query(rebind(c.pg, q), args...)
}
func (c *sqlDB) QueryRow(q string, args ...any) *sql.Row {
	return c.DB.QueryRow(rebind(c.pg, q), args...)
}
func (c *sqlDB) Begin() (*sqlTx, error) {
	tx, err := c.DB.Begin()
	return &sqlTx{tx, c.pg}, err
}

func (t *sqlTx) Exec(q string, args ...any) (sql.Result, error) {
	return t.Tx.Exec(rebind(t.pg, q), args...)
}
func (t *sqlTx) Query(q string, args ...any) (*sql.Rows, error) {
	return t.Tx.Query(rebind(t.pg, q), args...)
}
func (t *sqlTx) QueryRow(q string, args ...any) *sql.Row {
	return t.Tx.QueryRow(rebind(t.pg, q), args...)
}

// rebind numbers the ? placeholders outside quoted literals for Postgres.
func rebind(pg bool, q string) string {
	if !pg || !strings.Contains(q, "?") {
		return q
	}
	var b strings.Builder
	n, quoted := 0, false
	for i := 0; i < len(q); i++ {
		switch c := q[i]; {
		case c == '\'':
			quoted = !quoted
		case c == '?' && !quoted:
			n++
			b.WriteString("$" + strconv.Itoa(n))
			continue
		}
		b.WriteByte(q[i])
	}
	return b.String()
}

// migrations run in order; PRAGMA user_version records how many have been applied.
type migration struct {
	sql string                 // executed verbatim when set
	run func(tx *sql.Tx) error // Go step, runs in the same transaction as sql
}

var migrations = []migration{
	{sql: `
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
	);`},
	{sql: `
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
	CREATE UNIQUE INDEX idx_emails_mailbox_msgid ON emails(mailbox_id, message_id) WHERE message_id != '';`},
	{run: normalizeTimestamps},
	{run: linkThreads},
	{run: addColumns},
	{run: userSignatures},
	{sql: `
	-- Lists walk a mailbox newest-first and filter on these columns from the index alone.
	-- Reading them from the table is slow: every row stores its bodies before them.
	CREATE INDEX IF NOT EXISTS idx_emails_view ON emails(mailbox_id, created_at, folder, is_archived, is_deleted, is_starred, is_unread, thread_id);
	DROP INDEX IF EXISTS idx_emails_thread;
	CREATE INDEX idx_emails_thread ON emails(mailbox_id, thread_id, created_at, folder, is_archived, is_deleted, is_starred);
	CREATE INDEX IF NOT EXISTS idx_emails_scheduled ON emails(created_at) WHERE folder = 'scheduled';
	CREATE INDEX IF NOT EXISTS idx_emails_sent_by ON emails(provider, created_at) WHERE folder = 'sent';
	CREATE INDEX IF NOT EXISTS idx_emails_unread ON emails(mailbox_id, thread_id)
		WHERE folder = 'inbox' AND is_unread = 1 AND is_archived = 0 AND is_deleted = 0;
	CREATE INDEX IF NOT EXISTS idx_emails_trash ON emails(mailbox_id) WHERE is_deleted = 1;
	DROP INDEX IF EXISTS idx_emails_folder;
	DROP INDEX IF EXISTS idx_emails_is_deleted;
	DROP INDEX IF EXISTS idx_emails_is_starred;
	DROP INDEX IF EXISTS idx_emails_created_at;
	DROP INDEX IF EXISTS idx_emails_mailbox;

	-- Full-text search index, kept in step with emails by triggers.
	CREATE VIRTUAL TABLE IF NOT EXISTS emails_fts USING fts5(
		subject, from_name, from_email, to_recipients, body_text,
		content = 'emails', content_rowid = 'id', tokenize = 'unicode61 remove_diacritics 2');
	CREATE TRIGGER IF NOT EXISTS emails_fts_insert AFTER INSERT ON emails BEGIN
		INSERT INTO emails_fts(rowid, subject, from_name, from_email, to_recipients, body_text)
		VALUES (new.id, new.subject, new.from_name, new.from_email, new.to_recipients, new.body_text);
	END;
	CREATE TRIGGER IF NOT EXISTS emails_fts_delete AFTER DELETE ON emails BEGIN
		INSERT INTO emails_fts(emails_fts, rowid, subject, from_name, from_email, to_recipients, body_text)
		VALUES ('delete', old.id, old.subject, old.from_name, old.from_email, old.to_recipients, old.body_text);
	END;
	CREATE TRIGGER IF NOT EXISTS emails_fts_update AFTER UPDATE OF subject, from_name, from_email, to_recipients, body_text ON emails BEGIN
		INSERT INTO emails_fts(emails_fts, rowid, subject, from_name, from_email, to_recipients, body_text)
		VALUES ('delete', old.id, old.subject, old.from_name, old.from_email, old.to_recipients, old.body_text);
		INSERT INTO emails_fts(rowid, subject, from_name, from_email, to_recipients, body_text)
		VALUES (new.id, new.subject, new.from_name, new.from_email, new.to_recipients, new.body_text);
	END;
	INSERT INTO emails_fts(emails_fts) VALUES ('rebuild');
	ANALYZE;`},
}

// pgMigrations is the PostgreSQL schema, which starts where SQLite's migration 7 left
// off. A schema change needs a step in both lists; schema_version plays user_version.
var pgMigrations = []migration{
	{sql: `
	-- SQLite's instr, so the queries shared by both databases need no variants.
	CREATE FUNCTION instr(s text, sub text) RETURNS integer LANGUAGE sql IMMUTABLE AS 'SELECT strpos(s, sub)';
	CREATE TABLE users (
		id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
		username TEXT NOT NULL,
		password_hash TEXT NOT NULL,
		is_admin INTEGER NOT NULL DEFAULT 0,
		created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
		signature TEXT NOT NULL DEFAULT ''
	);
	CREATE UNIQUE INDEX users_username ON users (lower(username));
	CREATE TABLE sessions (
		token_hash TEXT PRIMARY KEY,
		user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		expires_at BIGINT NOT NULL
	);
	CREATE TABLE mailboxes (
		id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
		address TEXT NOT NULL,
		display_name TEXT NOT NULL DEFAULT ''
	);
	CREATE UNIQUE INDEX mailboxes_address ON mailboxes (lower(address));
	CREATE TABLE user_mailboxes (
		user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		mailbox_id BIGINT NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
		can_send INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (user_id, mailbox_id)
	);
	CREATE TABLE settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE emails (
		id BIGINT GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
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
		created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
		mailbox_id BIGINT REFERENCES mailboxes(id) ON DELETE CASCADE,
		provider TEXT NOT NULL DEFAULT '',
		bcc_recipients TEXT,
		-- ponytail: a tsvector must stay under 1 MB, so only the first 200,000 characters
		-- are searchable; SQLite's FTS5 has no such cap. 'simple' keeps diacritics, which
		-- FTS5 strips: "kafe" does not find "café" here.
		search tsvector GENERATED ALWAYS AS (to_tsvector('simple', left(coalesce(subject, '') || ' ' ||
			coalesce(from_name, '') || ' ' || coalesce(from_email, '') || ' ' || coalesce(to_recipients, '') || ' ' ||
			coalesce(body_text, ''), 200000))) STORED
	);
	CREATE UNIQUE INDEX idx_emails_mailbox_msgid ON emails(mailbox_id, message_id) WHERE message_id != '';
	CREATE INDEX idx_emails_view ON emails(mailbox_id, created_at, folder, is_archived, is_deleted, is_starred, is_unread, thread_id);
	CREATE INDEX idx_emails_thread ON emails(mailbox_id, thread_id, created_at, folder, is_archived, is_deleted, is_starred);
	CREATE INDEX idx_emails_scheduled ON emails(created_at) WHERE folder = 'scheduled';
	CREATE INDEX idx_emails_sent_by ON emails(provider, created_at) WHERE folder = 'sent';
	CREATE INDEX idx_emails_unread ON emails(mailbox_id, thread_id)
		WHERE folder = 'inbox' AND is_unread = 1 AND is_archived = 0 AND is_deleted = 0;
	CREATE INDEX idx_emails_trash ON emails(mailbox_id) WHERE is_deleted = 1;
	CREATE INDEX idx_emails_search ON emails USING gin (search);
	CREATE TABLE attachments (
		id TEXT PRIMARY KEY,
		email_id BIGINT NOT NULL REFERENCES emails(id) ON DELETE CASCADE,
		filename TEXT NOT NULL,
		content_type TEXT NOT NULL,
		size_bytes BIGINT NOT NULL,
		file_path TEXT NOT NULL,
		created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX idx_attachments_email_id ON attachments(email_id);`},
}

func (d *DB) migrate() error {
	steps, version := migrations, 0
	if d.conn.pg {
		steps = pgMigrations
		if _, err := d.conn.Exec(`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
			return err
		}
		if err := d.conn.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version); err != nil {
			return err
		}
	} else if err := d.conn.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(steps); i++ {
		tx, err := d.conn.Begin()
		if err != nil {
			return err
		}
		m := steps[i]
		if m.sql != "" {
			if _, err := tx.Exec(m.sql); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %d: %w", i+1, err)
			}
		}
		if m.run != nil {
			if err := m.run(tx.Tx); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %d: %w", i+1, err)
			}
		}
		record := fmt.Sprintf("PRAGMA user_version = %d", i+1)
		if d.conn.pg {
			record = fmt.Sprintf("INSERT INTO schema_version VALUES (%d)", i+1)
		}
		if _, err := tx.Exec(record); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// storedTimeLayouts covers every datetime representation found in databases
// written by older builds (the driver stored t.String() verbatim) plus the
// canonical format written since _time_format=sqlite.
var storedTimeLayouts = []string{
	"2006-01-02 15:04:05.999999999-07:00", // canonical (current)
	"2006-01-02T15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999 -0700 MST", // t.String() of named-zone times
	"2006-01-02 15:04:05 -0700 -0700",         // t.String() of numeric-zone times (Date headers)
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999", // SQLite CURRENT_TIMESTAMP (UTC)
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// parseStoredTime accepts any layout above; zoneless input means UTC. The
// monotonic suffix of t.String() ("m=+...") is stripped first.
func parseStoredTime(s string) (time.Time, bool) {
	if i := strings.Index(s, " m="); i > 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	for _, layout := range storedTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// normalizeTimestamps rewrites emails timestamps to canonical UTC. Old rows
// mixed zones and formats; the numeric-zone variant ("... +0700 +0700", from
// inbound Date headers) cannot be scanned back into time.Time at all, which
// made every folder listing fail. Binding time.Time lets the driver write the
// canonical format. Rows in an unrecognized format are left untouched.
func normalizeTimestamps(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT id, created_at, updated_at FROM emails`)
	if err != nil {
		return err
	}
	type emailTimes struct {
		id               int64
		created, updated time.Time
	}
	var fixes []emailTimes
	for rows.Next() {
		var id int64
		var created, updated sql.NullString
		if err := rows.Scan(&id, &created, &updated); err != nil {
			rows.Close()
			return err
		}
		c, okC := parseStoredTime(created.String)
		u, okU := parseStoredTime(updated.String)
		if !okC || !okU {
			log.Printf("[db] created_at/updated_at email %d tidak dikenali, dibiarkan: %q / %q", id, created.String, updated.String)
			continue
		}
		fixes = append(fixes, emailTimes{id, c, u})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, f := range fixes {
		if _, err := tx.Exec(`UPDATE emails SET created_at = ?, updated_at = ? WHERE id = ?`, f.created, f.updated, f.id); err != nil {
			return err
		}
	}
	return nil
}

// linkThreads groups mail stored before conversations existed. Oldest first, so each
// reply finds the thread of the message it answers. Timestamps are not scanned: a row
// migration 3 could not repair must not stop the server from starting.
func linkThreads(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT id, COALESCE(mailbox_id, 0), COALESCE(message_id, ''), COALESCE(in_reply_to, ''),
		subject, from_email, COALESCE(folder, '') FROM emails ORDER BY created_at, id`)
	if err != nil {
		return err
	}
	var list []models.Email
	for rows.Next() {
		var m models.Email
		if err := rows.Scan(&m.ID, &m.MailboxID, &m.MessageID, &m.InReplyTo, &m.Subject, &m.FromEmail, &m.Folder); err != nil {
			rows.Close()
			return err
		}
		list = append(list, m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for i := range list {
		if _, err := tx.Exec(`UPDATE emails SET thread_id = ? WHERE id = ?`, threadFor(tx, &list[i]), list[i].ID); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS idx_emails_thread ON emails(mailbox_id, thread_id)`)
	return err
}

// hasColumn lets table-altering migrations skip work already done, so they stay safe
// to run again on a database a newer build touched.
func hasColumn(tx *sql.Tx, table, col string) (bool, error) {
	var n int
	err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, col).Scan(&n)
	return n > 0, err
}

// addColumns keeps the Bcc list of sent mail. (It once also added a per-mailbox
// signature; userSignatures removes that from databases that got it.)
func addColumns(tx *sql.Tx) error {
	has, err := hasColumn(tx, "emails", "bcc_recipients")
	if err == nil && !has {
		_, err = tx.Exec(`ALTER TABLE emails ADD COLUMN bcc_recipients TEXT`)
	}
	return err
}

// userSignatures gives each user their own signature in place of the per-mailbox one:
// a shared mailbox like info@ is written from by several people, each signing their name.
func userSignatures(tx *sql.Tx) error {
	has, err := hasColumn(tx, "users", "signature")
	if err == nil && !has {
		_, err = tx.Exec(`ALTER TABLE users ADD COLUMN signature TEXT NOT NULL DEFAULT ''`)
	}
	if err != nil {
		return err
	}
	if has, err = hasColumn(tx, "mailboxes", "signature"); err == nil && has {
		_, err = tx.Exec(`ALTER TABLE mailboxes DROP COLUMN signature`)
	}
	return err
}

/* ---------- emails ---------- */

type querier interface {
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}

var replyPrefix = regexp.MustCompile(`(?i)^\s*((re|fwd?)\s*(\[\d+\])?\s*:\s*)+`)

// baseSubject drops the Re:/Fwd: prefixes replies and forwards add.
func baseSubject(s string) string { return strings.TrimSpace(replyPrefix.ReplaceAllString(s, "")) }

// threadFor picks the conversation m joins: the one holding the message its In-Reply-To
// names, else (incoming replies only) the newest thread with the same base subject that
// the sender took part in. The fallback is what links replies to mail sent through
// Resend, which replaces our Message-ID with its own. Anything else starts a thread.
func threadFor(q querier, m *models.Email) string {
	var thread string
	if m.InReplyTo != "" && q.QueryRow(`SELECT thread_id FROM emails WHERE mailbox_id = ? AND message_id = ? AND thread_id != ''`,
		m.MailboxID, m.InReplyTo).Scan(&thread) == nil {
		return thread
	}
	if base := baseSubject(m.Subject); m.Folder != "sent" && m.Folder != "scheduled" && base != "" && base != strings.TrimSpace(m.Subject) {
		// ponytail: only the newest 2000 messages are searched, so a reply to an older
		// conversation starts a new one; widen the window if that turns out to matter.
		rows, err := q.Query(`SELECT subject, thread_id FROM (
				SELECT subject, thread_id, from_email, to_recipients, cc_recipients, created_at FROM emails
				WHERE mailbox_id = ? AND thread_id != '' ORDER BY created_at DESC LIMIT 2000) AS recent
			WHERE instr(subject, ?) > 0
			AND (lower(from_email) = lower(?) OR instr(lower(to_recipients || COALESCE(cc_recipients, '')), lower(?)) > 0)
			ORDER BY created_at DESC LIMIT 20`, m.MailboxID, base, m.FromEmail, `"`+m.FromEmail+`"`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var subject string
				if rows.Scan(&subject, &thread) == nil && strings.EqualFold(baseSubject(subject), base) {
					return thread
				}
			}
		}
	}
	if m.MessageID != "" {
		return m.MessageID
	}
	return "<" + rand.Text() + "@wmail>"
}

const emailCols = `id, COALESCE(mailbox_id, 0), provider, message_id, in_reply_to, thread_id, from_name, from_email,
	to_recipients, cc_recipients, COALESCE(bcc_recipients, ''), subject, snippet, body_text,
	body_html, is_unread, is_starred, is_archived, is_deleted,
	folder, category, label, has_attachments, created_at, updated_at`

type scanner interface{ Scan(dest ...any) error }

// scanEmail reads emailCols, then any extra columns the query appends into extra.
func scanEmail(row scanner, extra ...any) (*models.Email, error) {
	var m models.Email
	var bccJSON string
	var toJSON, ccJSON, msgID, inReplyTo, threadID, fromName, snippet, bodyText, bodyHTML, category, label sql.NullString
	var unread, starred, archived, deleted, hasAtt int
	err := row.Scan(append([]any{
		&m.ID, &m.MailboxID, &m.Provider, &msgID, &inReplyTo, &threadID, &fromName, &m.FromEmail,
		&toJSON, &ccJSON, &bccJSON, &m.Subject, &snippet, &bodyText,
		&bodyHTML, &unread, &starred, &archived, &deleted,
		&m.Folder, &category, &label, &hasAtt, &m.CreatedAt, &m.UpdatedAt,
	}, extra...)...)
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
	if bccJSON != "" {
		_ = json.Unmarshal([]byte(bccJSON), &m.BccRecipients)
	}
	return &m, nil
}

// SaveEmail inserts the email and its attachment rows. Returns ErrDuplicate when
// the mailbox already holds a message with the same Message-ID.
func (d *DB) SaveEmail(m *models.Email) error {
	toJSON, _ := json.Marshal(m.ToRecipients)
	ccJSON, _ := json.Marshal(m.CcRecipients)
	bccJSON, _ := json.Marshal(m.BccRecipients)
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	m.UpdatedAt = time.Now()
	m.HasAttachments = m.HasAttachments || len(m.Attachments) > 0
	if m.ThreadID == "" {
		m.ThreadID = threadFor(d.conn, m)
	}

	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	err = tx.QueryRow(`
	INSERT INTO emails (
		mailbox_id, provider, message_id, in_reply_to, thread_id, from_name, from_email,
		to_recipients, cc_recipients, bcc_recipients, subject, snippet, body_text,
		body_html, is_unread, is_starred, is_archived, is_deleted,
		folder, category, label, has_attachments, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT DO NOTHING RETURNING id`,
		m.MailboxID, m.Provider, m.MessageID, m.InReplyTo, m.ThreadID, m.FromName, m.FromEmail,
		string(toJSON), string(ccJSON), string(bccJSON), m.Subject, m.Snippet, m.BodyText,
		m.BodyHTML, boolToInt(m.IsUnread), boolToInt(m.IsStarred), boolToInt(m.IsArchived), boolToInt(m.IsDeleted),
		m.Folder, m.Category, m.Label, boolToInt(m.HasAttachments), m.CreatedAt, m.UpdatedAt,
	).Scan(&m.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrDuplicate
	}
	if err != nil {
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

// listCols is emailCols without the bodies: a list shows subject and snippet only.
var listCols = strings.NewReplacer("body_text,", "'',", "body_html,", "'',").Replace(emailCols)

// MaxCount caps how many conversations a list counts. Above it the UI says "banyak",
// as Gmail does: an exact count of a huge mailbox costs a scan on every load.
const MaxCount = 1000

// viewFilter is the WHERE clause of a list view, written for table alias t so it can
// also filter the correlated subquery that keeps one row per conversation.
func (d *DB) viewFilter(t string, mailboxID int64, view, query string) (string, []any) {
	c := []string{t + ".mailbox_id = ?"}
	args := []any{mailboxID}
	switch view {
	case "starred":
		c = append(c, t+".is_starred = 1", t+".is_deleted = 0")
	case "snoozed", "sent", "drafts", "spam", "scheduled":
		c = append(c, t+".folder = ?", t+".is_deleted = 0")
		args = append(args, view)
	case "important":
		c = append(c, "("+t+".label = 'Important' OR "+t+".is_starred = 1)", t+".is_deleted = 0")
	case "archive":
		// Conversations whose received mail has all been archived (not spammed or snoozed).
		inbox := func(archived int) string {
			return fmt.Sprintf(`SELECT 1 FROM emails a WHERE a.mailbox_id = %[1]s.mailbox_id AND a.thread_id = %[1]s.thread_id
				AND a.folder = 'inbox' AND a.is_deleted = 0 AND a.is_archived = %[2]d`, t, archived)
		}
		c = append(c, t+".is_deleted = 0", "EXISTS ("+inbox(1)+")", "NOT EXISTS ("+inbox(0)+")")
	case "trash":
		c = append(c, t+".is_deleted = 1")
	case "all":
		c = append(c, t+".is_deleted = 0")
	default:
		if name, ok := strings.CutPrefix(view, "label:"); ok {
			c = append(c, t+".label = ?", t+".is_deleted = 0")
			args = append(args, name)
		} else if name, ok := strings.CutPrefix(view, "category:"); ok {
			c = append(c, t+".category = ?", t+".is_deleted = 0")
			args = append(args, name)
		} else {
			c = append(c, t+".folder = 'inbox'", t+".is_archived = 0", t+".is_deleted = 0")
		}
	}
	if d.conn.pg {
		if match := tsQuery(query); match != "" {
			c = append(c, t+".search @@ to_tsquery('simple', ?)")
			args = append(args, match)
		}
	} else if match := ftsQuery(query); match != "" {
		// The unary + keeps SQLite from driving the query by the matches' rowids, which
		// reads every matching row (bodies included) when a word is common. Walking the
		// index newest-first and checking membership stops after one page instead.
		c = append(c, "+"+t+".id IN (SELECT rowid FROM emails_fts WHERE emails_fts MATCH ?)")
		args = append(args, match)
	}
	return strings.Join(c, " AND "), args
}

// ftsQuery turns what was typed into an FTS5 query: every word must appear, and the
// last one may be unfinished. Each word is quoted, so FTS operators typed by the user
// are searched as text; an address like budi@luar.test becomes a phrase of its parts.
func ftsQuery(q string) string {
	var terms []string
	for _, w := range strings.Fields(q) {
		if w = strings.ReplaceAll(w, `"`, ""); w != "" {
			terms = append(terms, `"`+w+`"*`)
		}
	}
	return strings.Join(terms, " ")
}

// tsQuery is ftsQuery for Postgres: each word quoted as a prefix lexeme, all required.
func tsQuery(q string) string {
	var terms []string
	for _, w := range strings.Fields(q) {
		if w = strings.NewReplacer(`\`, "", "'", "''").Replace(w); w != "" {
			terms = append(terms, "'"+w+"':*")
		}
	}
	return strings.Join(terms, " & ")
}

// ListEmails returns one page of a view, one row per conversation as in Gmail: the
// conversation's newest message in the view. total counts conversations up to
// MaxCount+1. The query walks the mailbox newest-first through idx_emails_view and
// skips messages with a newer sibling in the view, so a page costs about a page of
// work however big the mailbox is.
func (d *DB) ListEmails(mailboxID int64, viewFolder, queryStr string, limit, offset int) ([]models.Email, int, error) {
	where, args := d.viewFilter("e", mailboxID, viewFolder, queryStr)
	newer, newerArgs := d.viewFilter("n", mailboxID, viewFolder, queryStr)

	var total int
	if err := d.conn.QueryRow(`SELECT COUNT(*) FROM (SELECT DISTINCT e.thread_id FROM emails e WHERE `+where+` LIMIT ?) AS v`,
		append(append([]any{}, args...), MaxCount+1)...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 200) // the client picks the page size; keep one request bounded

	rows, err := d.conn.Query(`SELECT `+listCols+`,
		(SELECT COUNT(*) FROM emails t WHERE t.mailbox_id = e.mailbox_id AND t.thread_id = e.thread_id AND t.is_deleted = e.is_deleted),
		EXISTS (SELECT 1 FROM emails t WHERE t.mailbox_id = e.mailbox_id AND t.thread_id = e.thread_id
			AND t.folder = 'inbox' AND t.is_archived = 0 AND t.is_deleted = 0)
		FROM emails e WHERE `+where+`
		AND NOT EXISTS (SELECT 1 FROM emails n WHERE `+newer+` AND n.thread_id = e.thread_id
			AND (n.created_at > e.created_at OR (n.created_at = e.created_at AND n.id > e.id)))
		ORDER BY e.created_at DESC, e.id DESC LIMIT ? OFFSET ?`,
		append(append(append([]any{}, args...), newerArgs...), limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	list := []models.Email{}
	for rows.Next() {
		var count int
		var inInbox bool // Postgres returns EXISTS as a boolean, SQLite as 0/1
		m, err := scanEmail(rows, &count, &inInbox)
		if err != nil {
			return nil, 0, err
		}
		m.ThreadCount, m.InInbox = count, inInbox
		list = append(list, *m)
	}
	return list, total, rows.Err()
}

// sameThread matches every message in the conversation of the email whose id is bound.
const sameThread = "(mailbox_id, thread_id) = (SELECT mailbox_id, thread_id FROM emails WHERE id = ?)"

// UpdateEmailStatus stars one message; every other change applies to its whole
// conversation, as in Gmail. Sent and scheduled mail keep their folder and read state:
// moving a conversation to the inbox must not turn a scheduled message into received mail.
func (d *DB) UpdateEmailStatus(id int64, req models.UpdateStatusRequest) error {
	if req.IsStarred != nil {
		if _, err := d.conn.Exec("UPDATE emails SET is_starred = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?", boolToInt(*req.IsStarred), id); err != nil {
			return err
		}
	}
	var sets []string
	var args []any
	add := func(col string, v any) { sets = append(sets, col+" = ?"); args = append(args, v) }
	keepSent := func(col string, v any) {
		sets = append(sets, col+" = CASE WHEN folder IN ('sent', 'scheduled') THEN "+col+" ELSE ? END")
		args = append(args, v)
	}

	if req.IsUnread != nil {
		keepSent("is_unread", boolToInt(*req.IsUnread))
	}
	if req.IsArchived != nil {
		add("is_archived", boolToInt(*req.IsArchived))
	}
	if req.IsDeleted != nil {
		add("is_deleted", boolToInt(*req.IsDeleted))
	}
	if req.Folder != nil {
		keepSent("folder", *req.Folder)
	}
	if req.Label != nil {
		add("label", *req.Label)
	}
	if len(sets) == 0 {
		return nil
	}
	sets = append(sets, "updated_at = CURRENT_TIMESTAMP")
	_, err := d.conn.Exec("UPDATE emails SET "+strings.Join(sets, ", ")+" WHERE "+sameThread, append(args, id)...)
	return err
}

// Thread returns the conversation m belongs to, oldest first, with attachments.
// Trashed messages show only in a trashed conversation and the rest only outside it.
func (d *DB) Thread(m *models.Email) ([]models.Email, error) {
	rows, err := d.conn.Query(`SELECT id FROM emails WHERE mailbox_id = ? AND thread_id = ? AND is_deleted = ? ORDER BY created_at, id`,
		m.MailboxID, m.ThreadID, boolToInt(m.IsDeleted))
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// ponytail: one GetEmail per message; batch the attachment query if threads get long.
	thread := []models.Email{}
	for _, id := range ids {
		e, err := d.GetEmail(id)
		if err != nil {
			return nil, err
		}
		thread = append(thread, *e)
	}
	return thread, nil
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

// DueScheduled lists scheduled mail whose time has come, oldest first. A scheduled
// message keeps its send time in created_at, so lists show and sort it by that time.
func (d *DB) DueScheduled(now time.Time) ([]int64, error) {
	rows, err := d.conn.Query(`SELECT id FROM emails WHERE folder = 'scheduled' AND is_deleted = 0 AND created_at <= ? ORDER BY created_at`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// MarkSent turns a delivered scheduled message into sent mail.
func (d *DB) MarkSent(id int64, provider, messageID string) error {
	now := time.Now()
	_, err := d.conn.Exec(`UPDATE emails SET folder = 'sent', provider = ?, message_id = ?, created_at = ?, updated_at = ? WHERE id = ?`,
		provider, messageID, now, now, id)
	return err
}

// Reschedule moves a scheduled message to a new send time.
func (d *DB) Reschedule(id int64, at time.Time) error {
	_, err := d.conn.Exec(`UPDATE emails SET created_at = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND folder = 'scheduled'`, at, id)
	return err
}

// Unschedule deletes a scheduled message that has not gone out yet.
func (d *DB) Unschedule(id int64) error {
	return d.deleteEmailsWhere("id = ? AND folder = 'scheduled'", id)
}

// DeleteEmail permanently removes the trashed messages of id's conversation.
func (d *DB) DeleteEmail(id int64) error {
	return d.deleteEmailsWhere("is_deleted = 1 AND "+sameThread, id)
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

// GetStats returns the counts the sidebar shows. Each reads only a partial index
// (idx_emails_unread, idx_emails_trash), so clients can poll it on a huge mailbox.
func (d *DB) GetStats(mailboxID int64) (*models.Stats, error) {
	s := &models.Stats{}
	err := d.conn.QueryRow(`SELECT
		(SELECT COUNT(DISTINCT thread_id) FROM emails
			WHERE mailbox_id = ? AND folder = 'inbox' AND is_unread = 1 AND is_archived = 0 AND is_deleted = 0),
		(SELECT COUNT(*) FROM emails WHERE mailbox_id = ? AND is_deleted = 1)`, mailboxID, mailboxID).
		Scan(&s.InboxUnread, &s.TrashCount)
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
	var id int64
	err = d.conn.QueryRow("INSERT INTO users (username, password_hash, is_admin) VALUES (?, ?, ?) RETURNING id", username, h, boolToInt(isAdmin)).Scan(&id)
	return id, err
}

func (d *DB) Authenticate(username, password string) (*models.User, error) {
	var u models.User
	var h string
	var admin int
	err := d.conn.QueryRow("SELECT id, username, password_hash, is_admin, created_at FROM users WHERE lower(username) = lower(?)", strings.TrimSpace(username)).
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
	return d.userWhere("lower(username) = lower(?)", username)
}

func (d *DB) userWhere(where string, arg any) (*models.User, error) {
	var u models.User
	var admin int
	err := d.conn.QueryRow("SELECT id, username, is_admin, signature, created_at FROM users WHERE "+where, arg).Scan(&u.ID, &u.Username, &admin, &u.Signature, &u.CreatedAt)
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

func (d *DB) SetSignature(userID int64, signature string) error {
	_, err := d.conn.Exec("UPDATE users SET signature = ? WHERE id = ?", strings.TrimSpace(strings.ReplaceAll(signature, "\r\n", "\n")), userID)
	return err
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

// ParseDomains splits the "domain" setting ("contoh.com, toko.id") into lowercase,
// unique names. The first one is the primary domain.
func ParseDomains(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

func (d *DB) Domains() []string { return ParseDomains(d.GetSetting("domain")) }

// FullAddress normalizes addr and gives a bare name ("sales") the primary domain,
// so nobody has to type "@domain" when adding or granting a mailbox.
func (d *DB) FullAddress(addr string) string {
	addr = NormalizeAddress(addr)
	if doms := d.Domains(); addr != "" && len(doms) > 0 && !strings.Contains(addr, "@") {
		addr += "@" + doms[0]
	}
	return addr
}

func (d *DB) CreateMailbox(address, displayName string) (int64, error) {
	address = d.FullAddress(address)
	if at := strings.IndexByte(address, '@'); at < 1 || at == len(address)-1 || strings.ContainsAny(address, " <>,;") {
		return 0, fmt.Errorf("%w: alamat email tidak valid", ErrInvalidInput)
	}
	var id int64
	err := d.conn.QueryRow("INSERT INTO mailboxes (address, display_name) VALUES (?, ?) RETURNING id", address, strings.TrimSpace(displayName)).Scan(&id)
	return id, err
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
	err := d.conn.QueryRow("SELECT id, address, display_name FROM mailboxes WHERE address = ?", d.FullAddress(address)).Scan(&m.ID, &m.Address, &m.DisplayName)
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

/* ---------- copies and snapshots ---------- */

// copyOrder lists the tables parents first, so foreign keys hold at every insert.
var copyOrder = []string{"users", "mailboxes", "user_mailboxes", "sessions", "settings", "emails", "attachments"}

// CopyTo copies every row into dst, SQLite or PostgreSQL, in one transaction: a failure
// leaves dst as it was. dst must be empty unless replace is set, which deletes its rows
// first. The source is read from one snapshot. Attachment files stay where rows point.
func (d *DB) CopyTo(dst *DB, replace bool) error {
	src, err := d.beginSnapshot()
	if err != nil {
		return err
	}
	defer src.Rollback()
	tx, err := dst.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if replace {
		for _, table := range slices.Backward(copyOrder) {
			if _, err := tx.Exec("DELETE FROM " + table); err != nil {
				return err
			}
		}
	}
	for _, table := range copyOrder {
		var n int
		if err := tx.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("tabel %s di database tujuan tidak kosong", table)
		}
		// Only the columns both sides have: Postgres adds a generated search column.
		cols, err := sharedColumns(src, tx, table)
		if err != nil {
			return err
		}
		rows, err := src.Query("SELECT " + strings.Join(cols, ", ") + " FROM " + table)
		if err != nil {
			return err
		}
		insert := "INSERT INTO " + table + " (" + strings.Join(cols, ", ") + ") VALUES (?" + strings.Repeat(", ?", len(cols)-1) + ")"
		vals, ptrs := make([]any, len(cols)), make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		// ponytail: one INSERT per row, ~2,500 emails/s (50k in 20 s locally); switch to COPY if big mailboxes need faster.
		for rows.Next() {
			if err = rows.Scan(ptrs...); err == nil {
				_, err = tx.Exec(insert, vals...)
			}
			if err != nil {
				rows.Close()
				return fmt.Errorf("%s: %w", table, err)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
	}
	if dst.conn.pg {
		// Rows kept their ids, so new rows must be numbered after them.
		for _, table := range []string{"users", "mailboxes", "emails"} {
			if _, err := tx.Exec(`SELECT setval(pg_get_serial_sequence('` + table + `', 'id'), COALESCE(MAX(id), 0) + 1, false) FROM ` + table); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// beginSnapshot starts a transaction whose reads all see the database at one moment.
func (d *DB) beginSnapshot() (*sqlTx, error) {
	if !d.conn.pg {
		return d.conn.Begin() // in WAL mode a SQLite read transaction already does
	}
	tx, err := d.conn.DB.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	return &sqlTx{tx, true}, err
}

func sharedColumns(a, b *sqlTx, table string) ([]string, error) {
	columns := func(t *sqlTx) ([]string, error) {
		rows, err := t.Query("SELECT * FROM " + table + " LIMIT 0")
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return rows.Columns()
	}
	ac, err := columns(a)
	if err != nil {
		return nil, err
	}
	bc, err := columns(b)
	return slices.DeleteFunc(ac, func(c string) bool { return !slices.Contains(bc, c) }), err
}

// Snapshot writes a consistent copy of the whole database, SQLite or PostgreSQL, to a new
// SQLite file at path.
func (d *DB) Snapshot(path string) error {
	if !d.conn.pg {
		_, err := d.conn.Exec("VACUUM INTO ?", path)
		return err
	}
	snap, err := InitDB(path)
	if err != nil {
		return err
	}
	defer snap.Close()
	return d.CopyTo(snap, false)
}

// AttachmentPaths maps every attachment id to its file on disk.
func (d *DB) AttachmentPaths() (map[string]string, error) {
	rows, err := d.conn.Query("SELECT id, file_path FROM attachments")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paths := map[string]string{}
	for rows.Next() {
		var id, path string
		if err := rows.Scan(&id, &path); err != nil {
			return nil, err
		}
		paths[id] = path
	}
	return paths, rows.Err()
}

// MoveAttachments points every attachment row at dir/<id>, the name SaveAttachmentFile gives files.
func (d *DB) MoveAttachments(dir string) error {
	_, err := d.conn.Exec("UPDATE attachments SET file_path = ? || id", filepath.Clean(dir)+string(filepath.Separator))
	return err
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
	{Key: "domain", Desc: "Domain email, pisahkan dengan koma; yang pertama domain utama. Mis. contoh.com, toko.id"},
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
	{Key: "backup_recipient", Desc: "Kunci publik age untuk backup (age1...); buat dengan: wmail backup key"},
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
