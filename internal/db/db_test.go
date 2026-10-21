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

func TestConversations(t *testing.T) {
	d, err := db.InitDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	mb, _ := d.CreateMailbox("info@contoh.com", "")
	at := func(h int) time.Time { return time.Date(2026, 10, 20, h, 0, 0, 0, time.UTC) }
	save := func(m models.Email) int64 {
		t.Helper()
		m.MailboxID = mb
		if err := d.SaveEmail(&m); err != nil {
			t.Fatal(err)
		}
		return m.ID
	}
	first := save(models.Email{MessageID: "<a@luar>", FromEmail: "budi@luar", ToRecipients: []string{"info@contoh.com"}, Subject: "Rapat", Folder: "inbox", IsUnread: true, CreatedAt: at(1)})
	save(models.Email{MessageID: "<r@resend>", InReplyTo: "<a@luar>", FromEmail: "info@contoh.com", ToRecipients: []string{"budi@luar"}, Subject: "Re: Rapat", Folder: "sent", CreatedAt: at(2)})
	// Resend rewrote our Message-ID, so Budi's answer only matches on subject and sender.
	last := save(models.Email{MessageID: "<b@luar>", InReplyTo: "<x@amazonses>", FromEmail: "Budi@luar", ToRecipients: []string{"info@contoh.com"}, Subject: "RE: Re: Rapat", Folder: "inbox", IsUnread: true, CreatedAt: at(3)})
	other := save(models.Email{MessageID: "<c@luar>", FromEmail: "citra@luar", Subject: "Re: Rapat", Folder: "inbox", IsUnread: true, CreatedAt: at(4)})

	list, total, err := d.ListEmails(mb, "inbox", "", 50, 0)
	if err != nil || total != 2 || len(list) != 2 {
		t.Fatalf("inbox = %d rows (total %d), want 2 conversations: %v", len(list), total, err)
	}
	if list[1].ID != last || list[1].ThreadCount != 3 || !list[1].InInbox {
		t.Fatalf("conversation row = %+v, want newest message %d with 3 messages", list[1], last)
	}
	if s, _ := d.GetStats(mb); s.InboxUnread != 2 {
		t.Fatalf("unread conversations = %d, want 2", s.InboxUnread)
	}

	archived := true
	if err := d.UpdateEmailStatus(first, models.UpdateStatusRequest{IsArchived: &archived}); err != nil {
		t.Fatal(err)
	}
	if list, _, _ = d.ListEmails(mb, "inbox", "", 50, 0); len(list) != 1 || list[0].ID != other {
		t.Fatalf("after archiving, inbox = %+v, want only %d", list, other)
	}
	if list, _, _ = d.ListEmails(mb, "archive", "", 50, 0); len(list) != 1 || list[0].ID != last {
		t.Fatalf("Arsip = %+v, want the archived conversation", list)
	}
	if list, _, _ = d.ListEmails(mb, "all", "", 50, 0); len(list) != 2 || list[1].InInbox {
		t.Fatalf("archived conversation should stay in Semua email, outside the inbox: %+v", list)
	}
	spam := "spam"
	if err := d.UpdateEmailStatus(last, models.UpdateStatusRequest{Folder: &spam}); err != nil {
		t.Fatal(err)
	}
	if list, _, _ = d.ListEmails(mb, "archive", "", 50, 0); len(list) != 0 {
		t.Fatalf("a conversation moved to Spam must leave Arsip: %+v", list)
	}
	later := save(models.Email{InReplyTo: "<b@luar>", FromEmail: "info@contoh.com", Subject: "Re: Rapat", Folder: "scheduled", CreatedAt: at(9)})
	inbox := "inbox"
	_ = d.UpdateEmailStatus(last, models.UpdateStatusRequest{Folder: &inbox})
	if m, _ := d.GetEmail(later); m.Folder != "scheduled" {
		t.Fatalf("moving the conversation turned scheduled mail into %q", m.Folder)
	}
	_ = d.UpdateEmailStatus(last, models.UpdateStatusRequest{Folder: &spam})
	if list, _, _ = d.ListEmails(mb, "sent", "", 50, 0); len(list) != 1 {
		t.Fatalf("moving a conversation must leave its sent copy in Terkirim: %+v", list)
	}

	m, _ := d.GetEmail(last)
	thread, err := d.Thread(m)
	if err != nil || len(thread) != 4 || thread[0].ID != first || thread[2].ID != last {
		t.Fatalf("thread = %+v, %v", thread, err)
	}

	deleted := true
	_ = d.UpdateEmailStatus(first, models.UpdateStatusRequest{IsDeleted: &deleted})
	if err := d.DeleteEmail(first); err != nil {
		t.Fatal(err)
	}
	if list, _, _ = d.ListEmails(mb, "all", "", 50, 0); len(list) != 1 || list[0].ID != other {
		t.Fatalf("permanent delete should remove the whole conversation: %+v", list)
	}
}

func TestBareMailboxNameGetsDomain(t *testing.T) {
	d, err := db.InitDB(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_ = d.SetSetting("domain", "Contoh.com")
	id, err := d.CreateMailbox(" Sales ", "")
	if err != nil {
		t.Fatal(err)
	}
	if mb, err := d.MailboxByAddress("sales"); err != nil || mb.ID != id || mb.Address != "sales@contoh.com" {
		t.Fatalf("bare name: %+v, %v", mb, err)
	}
	if _, err := d.CreateMailbox("info@lain.org", ""); err != nil {
		t.Fatalf("a full address on another domain must stay as typed: %v", err)
	}
	if _, err := d.MailboxByAddress("info@lain.org"); err != nil {
		t.Fatal(err)
	}
}
