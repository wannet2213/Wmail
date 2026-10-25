package cli

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wmail/internal/cf"
	"wmail/internal/dbtest"
)

type catchAll struct {
	Enabled bool `json:"enabled"`
	Actions []struct {
		Type  string   `json:"type"`
		Value []string `json:"value"`
	} `json:"actions"`
}

// fakeCloudflare serves the zones the token can see (name -> account) and keeps each
// zone's catch-all rule; zone ids are the zone names.
func fakeCloudflare(t *testing.T, zones map[string]string) map[string]*catchAll {
	rules := map[string]*catchAll{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var result any = map[string]any{"enabled": true, "status": "ready"}
		parts := strings.Split(r.URL.Path, "/")
		switch {
		case r.URL.Path == "/zones":
			list := []any{}
			if acct, ok := zones[r.URL.Query().Get("name")]; ok {
				list = append(list, map[string]any{"id": r.URL.Query().Get("name"), "status": "active", "account": map[string]string{"id": acct}})
			}
			result = list
		case strings.HasSuffix(r.URL.Path, "/catch_all") && r.Method == "PUT":
			var rule catchAll
			json.NewDecoder(r.Body).Decode(&rule)
			rules[parts[2]] = &rule
		case strings.HasSuffix(r.URL.Path, "/catch_all"):
			result = rules[parts[2]]
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result})
	}))
	old := cf.BaseURL
	cf.BaseURL = srv.URL
	t.Cleanup(func() { srv.Close(); cf.BaseURL = old })
	return rules
}

// A token that cannot see a new zone fails only that domain; once the token is fixed,
// repeating the same `domain add` finishes it.
func TestDomainAddRetriesAfterTokenFix(t *testing.T) {
	zones := map[string]string{"a.com": "acct1", "other.com": "acct2"}
	rules := fakeCloudflare(t, zones)
	c := &ctx{db: dbtest.Open(t)}
	_ = c.db.SetSetting("domain", "a.com")
	_ = c.db.SetSetting("cf_api_token", "t")
	_ = c.db.SetSetting("cf_account_id", "acct1")

	err := c.domain([]string{"add", "b.com", "other.com"})
	if err == nil || !strings.Contains(err.Error(), "b.com") || !strings.Contains(err.Error(), "akun Cloudflare lain") {
		t.Fatalf("want errors for b.com (no access) and other.com (other account): %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("no zone should be routed yet: %v", rules)
	}
	if got := strings.Join(c.db.Domains(), " "); got != "a.com b.com other.com" {
		t.Fatalf("domains: %q", got)
	}

	zones["b.com"] = "acct1" // token edited to include b.com
	if err := c.domain([]string{"add", "b.com"}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if r := rules["b.com"]; r == nil || !r.Enabled || r.Actions[0].Value[0] != workerName {
		t.Fatalf("b.com catch-all: %+v", r)
	}
}

// del drops the domains from the list and turns off a catch-all that goes to the Worker,
// but leaves one that was pointed elsewhere, and keeps the mailboxes.
func TestDomainDel(t *testing.T) {
	rules := fakeCloudflare(t, map[string]string{"a.com": "acct1", "b.com": "acct1", "c.com": "acct1"})
	c := &ctx{db: dbtest.Open(t)}
	_ = c.db.SetSetting("cf_api_token", "t")
	_ = c.db.SetSetting("cf_account_id", "acct1")
	if err := c.domain([]string{"add", "a.com", "b.com", "c.com"}); err != nil {
		t.Fatal(err)
	}
	rules["c.com"].Actions[0].Type = "forward" // changed by hand in the dashboard
	if _, err := c.db.CreateMailbox("info@a.com", ""); err != nil {
		t.Fatal(err)
	}

	stdin = bufio.NewReader(strings.NewReader("y\n"))
	if err := c.domain([]string{"del", "a.com", "c.com"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.db.Domains(), " "); got != "b.com" {
		t.Fatalf("domains: %q", got)
	}
	if rules["a.com"].Enabled || !rules["b.com"].Enabled || !rules["c.com"].Enabled {
		t.Fatalf("catch-all a=%v b=%v c=%v, want off/on/untouched", rules["a.com"].Enabled, rules["b.com"].Enabled, rules["c.com"].Enabled)
	}
	if _, err := c.db.MailboxByAddress("info@a.com"); err != nil {
		t.Fatalf("mailbox must stay: %v", err)
	}
}
