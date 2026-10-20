package db_test

import (
	"database/sql"
	"errors"
	"net/mail"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"wmail/internal/db"
	"wmail/internal/models"
)

func TestUsersSessionsMailboxes(t *testing.T) {
	d, err := db.InitDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := d.CreateUser("ana", "short", true); !errors.Is(err, db.ErrInvalidInput) {
		t.Fatalf("short password accepted: %v", err)
	}
	id, err := d.CreateUser("ana", "password-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Authenticate("ANA", "password-1"); err != nil {
		t.Fatalf("login (case-insensitive username): %v", err)
	}
	if _, err := d.Authenticate("ana", "password-2"); !errors.Is(err, db.ErrBadLogin) {
		t.Fatalf("wrong password accepted: %v", err)
	}

	token, _ := d.CreateSession(id, time.Hour)
	if u, err := d.UserBySession(token); err != nil || u.ID != id {
		t.Fatalf("session lookup: %v", err)
	}
	if err := d.SetPassword(id, "password-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.UserBySession(token); err == nil {
		t.Fatal("session survived password change")
	}
	if err := d.DeleteUser(id); !errors.Is(err, db.ErrLastAdmin) {
		t.Fatalf("last admin deleted: %v", err)
	}

	mb, err := d.CreateMailbox(" Info@Contoh.COM ", "Info")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateMailbox("bukan-email", ""); err == nil {
		t.Fatal("invalid address accepted")
	}
	if got, err := d.MailboxByAddress("info@contoh.com"); err != nil || got.ID != mb {
		t.Fatalf("lookup normalized address: %v", err)
	}

	e := models.Email{MailboxID: mb, MessageID: "<x@y>", FromEmail: "a@b", Subject: "s", Folder: "inbox"}
	if err := d.SaveEmail(&e); err != nil {
		t.Fatal(err)
	}
	dup := e
	if err := d.SaveEmail(&dup); !errors.Is(err, db.ErrDuplicate) {
		t.Fatalf("duplicate Message-ID stored: %v", err)
	}
	if err := d.DeleteMailbox(mb); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetEmail(e.ID); err == nil {
		t.Fatal("emails not removed with mailbox")
	}
}

// Old builds stored datetimes as t.String(): a Date header with a numeric zone
// ("... +0700 +0700") became unscannable and broke every folder listing.
// Migration 3 rewrites such rows to canonical UTC.
func TestLegacyTimestampsRepaired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	d, err := db.InitDB(path)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := d.CreateMailbox("info@contoh.com", "")
	if err != nil {
		t.Fatal(err)
	}
	legacy := []models.Email{
		{MailboxID: mb, MessageID: "<broken@x>", FromEmail: "a@b", Subject: "broken", Folder: "inbox"},
		{MailboxID: mb, MessageID: "<mono@x>", FromEmail: "a@b", Subject: "mono", Folder: "inbox"},
	}
	for i := range legacy {
		if err := d.SaveEmail(&legacy[i]); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	const brokenDate = "2026-10-20 18:42:19 +0700 +0700"                         // mail.ParseDate("...+0700").String()
	const monoClock = "2026-10-20 19:42:02.147745055 +0800 CST m=+224.482093975" // time.Now().String()
	if _, err := raw.Exec(`UPDATE emails SET created_at = ? WHERE message_id = '<broken@x>'`, brokenDate); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE emails SET created_at = ?, updated_at = ? WHERE message_id = '<mono@x>'`, monoClock, monoClock); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 2`); err != nil { // pretend an older build wrote this
		t.Fatal(err)
	}
	raw.Close()

	d, err = db.InitDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// New inbound mail carries a numeric-zone Date header; it must round-trip too.
	dateHeader, err := mail.ParseDate("Mon, 20 Oct 2026 18:42:19 +0700")
	if err != nil {
		t.Fatal(err)
	}
	inbound := models.Email{MailboxID: mb, MessageID: "<inbound@x>", FromEmail: "a@b", Subject: "inbound", Folder: "inbox", CreatedAt: dateHeader}
	if err := d.SaveEmail(&inbound); err != nil {
		t.Fatal(err)
	}

	emails, _, err := d.ListEmails(mb, "inbox", "", 50, 0)
	if err != nil {
		t.Fatalf("list emails: %v", err)
	}
	got := map[string]time.Time{}
	for _, e := range emails {
		got[e.Subject] = e.CreatedAt
	}
	if len(emails) != 3 {
		t.Fatalf("got %d emails, want 3", len(emails))
	}
	wantBroken := time.Date(2026, 10, 20, 11, 42, 19, 0, time.UTC)
	if !got["broken"].UTC().Equal(wantBroken) {
		t.Fatalf("repaired created_at = %v, want %v", got["broken"], wantBroken)
	}
	if !got["mono"].UTC().Equal(time.Date(2026, 10, 20, 11, 42, 2, 147745055, time.UTC)) {
		t.Fatalf("repaired monotonic created_at = %v", got["mono"])
	}
	if !got["inbound"].UTC().Equal(dateHeader.UTC()) {
		t.Fatalf("inbound created_at = %v, want %v", got["inbound"], dateHeader.UTC())
	}
}
