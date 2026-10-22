// Package dbtest gives each test a fresh database: SQLite in a temp dir, or a throwaway
// schema on the PostgreSQL server named by WMAIL_TEST_POSTGRES, so the same tests
// cover both, e.g. WMAIL_TEST_POSTGRES=postgres://wmail@localhost:5432/wmail go test ./...
package dbtest

import (
	"crypto/rand"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wmail/internal/db"
)

// DSN returns what db.InitDB should open for t; the database is removed after the test.
func DSN(t *testing.T) string {
	t.Helper()
	url := os.Getenv("WMAIL_TEST_POSTGRES")
	if url == "" {
		return filepath.Join(t.TempDir(), "test.db")
	}
	admin, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	schema := "test_" + strings.ToLower(rand.Text()[:12])
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		admin.Close()
	})
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	return url + sep + "search_path=" + schema
}

// Open returns a migrated database for t, closed when the test ends.
func Open(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.InitDB(DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}
