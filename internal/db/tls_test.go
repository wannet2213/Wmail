package db

import "testing"

func TestRemotePostgresNeedsTLS(t *testing.T) {
	for dsn, plaintext := range map[string]bool{
		"postgres://u:p@db.example.com/wmail":                          true, // default sslmode=prefer
		"postgres://u:p@db.example.com/wmail?sslmode=prefer":           true,
		"postgres://u:p@db.example.com/wmail?sslmode=allow":            true,
		"postgres://u:p@db.example.com/wmail?sslmode=disable":          true,
		"postgres://u:p@localhost,db.example.com/wmail?sslmode=prefer": true, // the second host
		"postgres://u:p@db.example.com/wmail?sslmode=require":          false,
		"postgres://u:p@db.example.com/wmail?sslmode=verify-full":      false,
		"postgres://u:p@localhost:5432/wmail":                          false,
		"postgres://u:p@127.0.0.1/wmail?sslmode=disable":               false,
		"postgres://u:p@[::1]/wmail":                                   false,
		"postgres:///wmail?host=/var/run/postgresql&port=5432":         false, // installer's local socket
	} {
		host, err := plaintextRemote(dsn)
		if err != nil {
			t.Fatalf("%s: %v", dsn, err)
		}
		if (host != "") != plaintext {
			t.Errorf("%s: plaintext host = %q, want plaintext %v", dsn, host, plaintext)
		}
	}
}
