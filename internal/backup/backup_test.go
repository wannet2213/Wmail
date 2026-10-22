package backup_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"

	"wmail/internal/backup"
	"wmail/internal/dbtest"
	"wmail/internal/models"
	"wmail/internal/service"
)

func TestBackupRestore(t *testing.T) {
	src, srcDir := dbtest.Open(t), t.TempDir()
	uid, _ := src.CreateUser("ana", "password-1", true)
	mb, _ := src.CreateMailbox("info@contoh.com", "Info")
	_ = src.SetSetting("domain", "contoh.com")
	att, _ := service.SaveAttachmentFile(srcDir, "kontrak.pdf", "application/pdf", []byte("isi pdf"))
	e := models.Email{MailboxID: mb, MessageID: "<k@x>", FromEmail: "budi@luar.test", Subject: "Kontrak baru",
		Folder: "inbox", Attachments: []models.Attachment{att}}
	if err := src.SaveEmail(&e); err != nil {
		t.Fatal(err)
	}

	secret, public, _ := backup.NewKey()
	var file bytes.Buffer
	if err := backup.Write(&file, src, public, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(file.Bytes(), []byte("budi@luar.test")) || bytes.Contains(file.Bytes(), []byte("isi pdf")) {
		t.Fatal("backup is readable without the key")
	}

	// A new server, with what its setup wizard made and one attachment of its own.
	dst, dstDir := dbtest.Open(t), t.TempDir()
	_, _ = dst.CreateUser("admin", "password-2", true)
	dmb, _ := dst.CreateMailbox("lama@contoh.com", "")
	oldAtt, _ := service.SaveAttachmentFile(dstDir, "lama.txt", "text/plain", []byte("lama"))
	_ = dst.SaveEmail(&models.Email{MailboxID: dmb, MessageID: "<l@x>", FromEmail: "a@b", Subject: "lama", Folder: "inbox",
		Attachments: []models.Attachment{oldAtt}})

	wrong, _, _ := backup.NewKey()
	if err := backup.Restore(bytes.NewReader(file.Bytes()), wrong, dst, dstDir); err == nil {
		t.Fatal("restored with the wrong key")
	}
	if _, err := dst.Authenticate("admin", "password-2"); err != nil {
		t.Fatal("a failed restore changed the database")
	}

	if err := backup.Restore(bytes.NewReader(file.Bytes()), secret, dst, dstDir); err != nil {
		t.Fatal(err)
	}
	if u, err := dst.Authenticate("ana", "password-1"); err != nil || u.ID != uid {
		t.Fatalf("login after restore: %v", err)
	}
	if _, err := dst.Authenticate("admin", "password-2"); err == nil {
		t.Fatal("data from before the restore survived it")
	}
	if dst.GetSetting("domain") != "contoh.com" {
		t.Fatal("settings not restored")
	}
	if list, _, err := dst.ListEmails(mb, "inbox", "kontrak", 50, 0); err != nil || len(list) != 1 {
		t.Fatalf("restored mail not found by search: %v %v", list, err)
	}
	got, err := dst.GetEmail(e.ID)
	if err != nil || len(got.Attachments) != 1 {
		t.Fatalf("restored email: %+v %v", got, err)
	}
	if b, _ := os.ReadFile(got.Attachments[0].FilePath); string(b) != "isi pdf" || filepath.Dir(got.Attachments[0].FilePath) != dstDir {
		t.Fatalf("attachment at %s holds %q", got.Attachments[0].FilePath, b)
	}
	if entries, _ := os.ReadDir(dstDir); len(entries) != 1 {
		t.Fatalf("data dir holds %v, want only the restored attachment", entries)
	}
}

func TestRestoreRefusesPathsInBackup(t *testing.T) {
	secret, public, _ := backup.NewKey()
	rcpt, _ := age.ParseX25519Recipient(public)
	var file bytes.Buffer
	enc, _ := age.Encrypt(&file, rcpt)
	gz := gzip.NewWriter(enc)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "attachments/../../evil", Mode: 0600, Size: 1})
	_, _ = tw.Write([]byte("x"))
	tw.Close()
	gz.Close()
	enc.Close()

	dir := t.TempDir()
	if err := backup.Restore(&file, secret, dbtest.Open(t), filepath.Join(dir, "a", "att")); err == nil {
		t.Fatal("accepted a path in the backup")
	}
	if _, err := os.Stat(filepath.Join(dir, "evil")); err == nil {
		t.Fatal("wrote outside the data dir")
	}
}
