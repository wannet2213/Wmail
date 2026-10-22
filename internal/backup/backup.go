// Package backup writes and restores Wmail backups: a SQLite snapshot of the database plus
// the attachment files, as a gzip-compressed tar encrypted with age to a public key. The
// server keeps only that public key, so neither a leaked backup nor a broken-into server
// opens old backups; the secret key stays with the operator. Without Wmail:
//
//	age -d -i key.txt wmail.tar.gz.age | tar xz
package backup

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"

	"wmail/internal/db"
)

// NewKey returns a fresh key pair: the secret key opens backups, the public key makes them.
func NewKey() (secret, public string, err error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", "", err
	}
	return id.String(), id.Recipient().String(), nil
}

// Write streams a backup of d, encrypted to the public key recipient, to w. The database
// snapshot is staged in a temporary directory under tmpDir, which needs room for a copy.
func Write(w io.Writer, d *db.DB, recipient, tmpDir string) error {
	rcpt, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return fmt.Errorf("kunci publik backup tidak valid: %w", err)
	}
	dir, err := os.MkdirTemp(tmpDir, ".backup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	snapPath := filepath.Join(dir, "mail.db")
	if err := d.Snapshot(snapPath); err != nil {
		return fmt.Errorf("snapshot database: %w", err)
	}
	snap, err := db.InitDB(snapPath)
	if err != nil {
		return err
	}
	files, err := snap.AttachmentPaths() // the files the snapshot's rows point at, no more
	snap.Close()
	if err != nil {
		return err
	}

	enc, err := age.Encrypt(w, rcpt)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(enc)
	tw := tar.NewWriter(gz)
	if err := addFile(tw, "mail.db", snapPath); err != nil {
		return err
	}
	for id, path := range files {
		if err := addFile(tw, "attachments/"+id, path); errors.Is(err, fs.ErrNotExist) {
			log.Printf("lampiran %s tidak ada di disk, dilewati: %s", id, path)
		} else if err != nil {
			return err
		}
	}
	for _, c := range []io.Closer{tw, gz, enc} {
		if err := c.Close(); err != nil {
			return err
		}
	}
	return nil
}

func addFile(tw *tar.Writer, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0640, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

// Restore replaces everything dst holds with the backup read from r, opened with the secret
// key. Attachment files go to dataDir, and the files of the replaced attachments are removed.
// The whole backup is decrypted and checked before dst is touched.
func Restore(r io.Reader, secret string, dst *db.DB, dataDir string) error {
	id, err := age.ParseX25519Identity(secret)
	if err != nil {
		return fmt.Errorf("kunci rahasia tidak valid: %w", err)
	}
	plain, err := age.Decrypt(r, id)
	if err != nil {
		return fmt.Errorf("backup tidak bisa dibuka dengan kunci ini: %w", err)
	}
	gz, err := gzip.NewReader(plain)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0750); err != nil {
		return err
	}
	// Staged inside dataDir so the files move in by rename, on the same filesystem.
	stage, err := os.MkdirTemp(dataDir, ".restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	// Anyone holding the public key can make a backup, so names are checked, never used as paths.
	restored := map[string]bool{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		out := filepath.Join(stage, "mail.db")
		if id, ok := strings.CutPrefix(hdr.Name, "attachments/"); ok && safeID(id) {
			out, restored[id] = filepath.Join(stage, "att-"+id), true
		} else if hdr.Name != "mail.db" {
			return fmt.Errorf("isi backup tidak dikenal: %q", hdr.Name)
		}
		if err := writeNew(out, tr); err != nil {
			return err
		}
	}
	if _, err := io.Copy(io.Discard, gz); err != nil { // authenticates the rest of the stream
		return err
	}

	snapPath := filepath.Join(stage, "mail.db")
	if _, err := os.Stat(snapPath); err != nil {
		return errors.New("bukan backup Wmail: mail.db tidak ada di dalamnya")
	}
	snap, err := db.InitDB(snapPath) // a backup from an older version is migrated here
	if err != nil {
		return err
	}
	defer snap.Close()
	ids, err := snap.AttachmentPaths()
	if err != nil {
		return err
	}
	for id := range ids {
		if !safeID(id) {
			return fmt.Errorf("id lampiran tidak valid di backup: %q", id)
		}
	}
	if err := snap.MoveAttachments(dataDir); err != nil {
		return err
	}
	old, err := dst.AttachmentPaths()
	if err != nil {
		return err
	}
	if err := snap.CopyTo(dst, true); err != nil {
		return err
	}
	for id := range restored {
		if err := os.Rename(filepath.Join(stage, "att-"+id), filepath.Join(dataDir, id)); err != nil {
			return err
		}
	}
	for id, path := range old {
		if !restored[id] {
			os.Remove(path)
		}
	}
	return nil
}

// safeID accepts the uuid names SaveAttachmentFile gives files; anything else could be a path.
func safeID(id string) bool {
	return id != "" && strings.Trim(id, "0123456789abcdef-") == ""
}

func writeNew(path string, r io.Reader) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
