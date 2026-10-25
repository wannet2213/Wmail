package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wmail/internal/cf"
	"wmail/internal/dbtest"
)

// A token that cannot see a new zone fails only that domain; once the token is fixed,
// repeating the same `domain add` finishes it.
func TestDomainAddRetriesAfterTokenFix(t *testing.T) {
	zones := map[string]string{"a.com": "acct1", "other.com": "acct2"} // what the token can see
	var catchAll []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var result any = map[string]any{"enabled": true, "status": "ready"}
		if r.URL.Path == "/zones" {
			list := []any{}
			if acct, ok := zones[r.URL.Query().Get("name")]; ok {
				list = append(list, map[string]any{"id": r.URL.Query().Get("name"), "status": "active", "account": map[string]string{"id": acct}})
			}
			result = list
		}
		if strings.HasSuffix(r.URL.Path, "/catch_all") {
			catchAll = append(catchAll, strings.Split(r.URL.Path, "/")[2])
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result})
	}))
	defer srv.Close()
	old := cf.BaseURL
	cf.BaseURL = srv.URL
	t.Cleanup(func() { cf.BaseURL = old })

	c := &ctx{db: dbtest.Open(t)}
	_ = c.db.SetSetting("domain", "a.com")
	_ = c.db.SetSetting("cf_api_token", "t")
	_ = c.db.SetSetting("cf_account_id", "acct1")

	err := c.domain([]string{"add", "b.com", "other.com"})
	if err == nil || !strings.Contains(err.Error(), "b.com") || !strings.Contains(err.Error(), "akun Cloudflare lain") {
		t.Fatalf("want errors for b.com (no access) and other.com (other account): %v", err)
	}
	if len(catchAll) != 0 {
		t.Fatalf("no zone should be routed yet: %v", catchAll)
	}
	if got := strings.Join(c.db.Domains(), " "); got != "a.com b.com other.com" {
		t.Fatalf("domains: %q", got)
	}

	zones["b.com"] = "acct1" // token edited to include b.com
	if err := c.domain([]string{"add", "b.com"}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if strings.Join(catchAll, " ") != "b.com" {
		t.Fatalf("catch-all set for %v, want b.com", catchAll)
	}
}
