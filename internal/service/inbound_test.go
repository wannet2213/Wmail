package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"wmail/internal/cf"
	"wmail/internal/db"
)

// A parked message that keeps failing is quarantined after maxKVAttempts drains instead of
// being retried forever, and KV listing follows the cursor past the first page.
func TestDrainKVQuarantinesAndPages(t *testing.T) {
	dir := t.TempDir()
	d, err := db.InitDB(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.CreateMailbox("a@x.test", "")
	for k, v := range map[string]string{"cf_account_id": "acc", "cf_kv_namespace_id": "ns", "cf_api_token": "tok"} {
		d.SetSetting(k, v)
	}
	// DataDir is a regular file, so every ingest with an attachment fails with a non-permanent error.
	blocker := filepath.Join(dir, "att")
	os.WriteFile(blocker, nil, 0600)

	raw := "From: x@example.com\r\nTo: a@x.test\r\nSubject: s\r\nMessage-ID: <m@x>\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=B\r\n\r\n--B\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=\"f.txt\"\r\n\r\nx\r\n--B--\r\n"
	var mu sync.Mutex
	deleted := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/keys"):
			page, cursor := []string{"k1"}, "c2"
			if r.URL.Query().Get("cursor") == "c2" {
				page, cursor = []string{"k2"}, ""
			}
			var keys []map[string]any
			for _, k := range page {
				if !deleted[k] {
					keys = append(keys, map[string]any{"name": k, "metadata": map[string]string{"to": "a@x.test"}})
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": keys, "result_info": map[string]string{"cursor": cursor}})
		case r.Method == "GET":
			w.Write([]byte(raw))
		case r.Method == "DELETE":
			deleted[filepath.Base(r.URL.Path)] = true
			w.Write([]byte(`{"success":true,"result":null}`))
		}
	}))
	defer srv.Close()
	old := cf.BaseURL
	cf.BaseURL = srv.URL
	defer func() { cf.BaseURL = old }()

	s := NewInboundService(blocker, d)
	for i := 1; i < maxKVAttempts; i++ {
		s.DrainKV()
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted before the retry bound: %v", deleted)
	}
	s.DrainKV()
	if !deleted["k1"] || !deleted["k2"] {
		t.Fatalf("both pages should be quarantined and deleted after %d drains: %v", maxKVAttempts, deleted)
	}
	if files, _ := os.ReadDir(filepath.Join(dir, "kv-failed")); len(files) != 2 {
		t.Fatalf("want 2 quarantined .eml files, got %d", len(files))
	}
}
