package db_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

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
