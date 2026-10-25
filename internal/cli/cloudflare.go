package cli

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strings"
	"time"

	"wmail/internal/cf"
	"wmail/internal/service"
)

const (
	workerName = "wmail-inbound"
	kvTitle    = "wmail-inbox"
	tunnelName = "wmail"
)

const tokenHelp = `Buat API token di https://dash.cloudflare.com/profile/api-tokens -> Create Token -> Custom token
dengan permission berikut:
  Account | Workers Scripts       | Edit
  Account | Workers KV Storage    | Edit
  Account | Cloudflare Tunnel     | Edit
  Zone    | Zone                  | Read
  Zone    | DNS                   | Edit
  Zone    | Email Routing Rules   | Edit
  Zone    | Zone Settings         | Edit
Zone Resources: Include -> Specific zone -> setiap domain email Anda
(atau Include -> All zones from an account, agar domain baru langsung tercakup).`

func randomString(n int, enc func([]byte) string) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return enc(b)
}

func step(msg string)    { fmt.Printf("  • %s... ", msg) }
func done(detail string) { fmt.Println("✓ " + detail) }

type zoneInfo struct {
	ID, Status string
	Account    struct{ ID string }
}

func findZone(api *cf.Client, domain string) (zoneInfo, error) {
	var zones []zoneInfo
	if err := api.Do("GET", "/zones?name="+url.QueryEscape(domain), nil, &zones); err != nil {
		return zoneInfo{}, err
	}
	if len(zones) == 0 {
		return zoneInfo{}, fmt.Errorf("zone %s tidak terlihat oleh token ini. Pastikan domainnya sudah ditambahkan di Cloudflare, "+
			"lalu edit token di https://dash.cloudflare.com/profile/api-tokens dan tambahkan zone ini di Zone Resources "+
			"(nilai token tidak berubah, jadi tidak perlu disimpan ulang)", domain)
	}
	return zones[0], nil
}

func (c *ctx) cloudflareSetup() error {
	domains := c.db.Domains()
	publicURL := c.db.GetSetting("public_url")
	if len(domains) == 0 || publicURL == "" {
		return errors.New("set dulu domain dan public_url (wmail setup atau wmail config set)")
	}
	host := strings.TrimPrefix(publicURL, "https://")

	token := c.db.GetSetting("cf_api_token")
	if token == "" || !confirm("Pakai Cloudflare API token yang tersimpan "+maskTail(token)+"?", true) {
		fmt.Println(tokenHelp)
		if token = promptSecret("Cloudflare API token"); token == "" {
			return errors.New("token kosong")
		}
	}
	api := cf.New(token)

	// The tunnel hostname goes in the zone it belongs to (contoh.com for mail.contoh.com),
	// which need not be a mail domain: it may have been removed with `domain del`.
	step("Mencari zone " + host)
	z, err := findZone(api, host)
	for name := host; err != nil && strings.Count(name, ".") > 1; {
		_, name, _ = strings.Cut(name, ".")
		z, err = findZone(api, name)
	}
	if err != nil {
		return err
	}
	zone, account := z.ID, z.Account.ID
	done("zone " + z.Status)
	_ = c.db.SetSetting("cf_api_token", token)
	_ = c.db.SetSetting("cf_zone_id", zone)
	_ = c.db.SetSetting("cf_account_id", account)

	step("KV namespace " + kvTitle)
	var namespaces []struct{ ID, Title string }
	if err := api.Do("GET", "/accounts/"+account+"/storage/kv/namespaces?per_page=100", nil, &namespaces); err != nil {
		return err
	}
	nsID := ""
	for _, n := range namespaces {
		if n.Title == kvTitle {
			nsID = n.ID
		}
	}
	if nsID == "" {
		var created struct{ ID string }
		if err := api.Do("POST", "/accounts/"+account+"/storage/kv/namespaces", map[string]string{"title": kvTitle}, &created); err != nil {
			return err
		}
		nsID = created.ID
	}
	_ = c.db.SetSetting("cf_kv_namespace_id", nsID)
	done(nsID)

	step("Cloudflare Tunnel " + tunnelName)
	tunnelID, tunnelToken, err := c.ensureTunnel(api, account, zone, host)
	if err != nil {
		return err
	}
	_ = c.db.SetSetting("cf_tunnel_id", tunnelID)
	_ = c.db.SetSetting("cf_tunnel_token", tunnelToken)
	done(host + " -> localhost:" + c.cfg.Port)

	secret := c.db.GetSetting("inbound_secret")
	if secret == "" {
		secret = randomString(32, hex.EncodeToString)
		_ = c.db.SetSetting("inbound_secret", secret)
	}

	step("Upload Worker " + workerName)
	if err := uploadWorker(api, account, c.assets.WorkerJS, publicURL, secret, nsID); err != nil {
		return err
	}
	done("terpasang")

	if _, err := eachDomain(domains, func(d string) error { return routeDomain(api, account, d) }); err != nil {
		return err
	}

	fmt.Println("\n  Cloudflare siap. Alamat yang tidak dibuat sebagai mailbox akan ditolak (bounce).")
	fmt.Println("  Jalankan installer lagi agar cloudflared dipasang dengan token di file root-only")
	fmt.Println("  (bukan di command line):  sudo ./install.sh")
	return nil
}

// eachDomain runs fn for every domain. A failing domain is reported and skipped so it does
// not hold up the others; ok lists the ones that went through.
func eachDomain(domains []string, fn func(string) error) (ok []string, err error) {
	var errs []error
	for _, d := range domains {
		if err := fn(d); err != nil {
			fmt.Println("✗", err)
			errs = append(errs, fmt.Errorf("%s: %w", d, err))
		} else {
			ok = append(ok, d)
		}
	}
	return ok, errors.Join(errs...)
}

// routeDomain turns on Email Routing for the domain and sends its catch-all to the Worker.
func routeDomain(api *cf.Client, account, domain string) error {
	step("Zone " + domain)
	z, err := findZone(api, domain)
	if err != nil {
		return err
	}
	if z.Account.ID != account {
		return errors.New("zone ini ada di akun Cloudflare lain, sedangkan Worker hanya bisa dipakai zone di akun yang sama")
	}
	done(z.Status)
	if z.Status != "active" {
		fmt.Println("    ! Zone belum aktif: pastikan nameserver " + domain + " sudah diarahkan ke Cloudflare.")
	}

	step("Email Routing " + domain)
	enableErr := api.Do("POST", "/zones/"+z.ID+"/email/routing/dns", nil, nil)
	var routing struct {
		Enabled bool
		Status  string
	}
	_ = api.Do("GET", "/zones/"+z.ID+"/email/routing", nil, &routing)
	if !routing.Enabled {
		fmt.Println("✗")
		if enableErr != nil {
			fmt.Printf("    %v\n", enableErr)
		}
		fmt.Println("    Aktifkan manual: dashboard Cloudflare -> " + domain + " -> Email -> Email Routing -> Enable.")
		fmt.Println("    (Biasanya gagal karena masih ada record MX lama; hapus dulu.) Lalu jalankan ulang perintah ini.")
	} else {
		done("status " + routing.Status)
	}

	step("Catch-all *@" + domain + " -> Worker")
	rule := map[string]any{
		"name":     "Wmail",
		"enabled":  true,
		"matchers": []map[string]string{{"type": "all"}},
		"actions":  []map[string]any{{"type": "worker", "value": []string{workerName}}},
	}
	if err := api.Do("PUT", "/zones/"+z.ID+"/email/routing/rules/catch_all", rule, nil); err != nil {
		return err
	}
	done("aktif")
	return nil
}

// unrouteDomain turns off the zone's catch-all while it still goes to the Worker, so mail to
// the domain is bounced. A catch-all someone pointed elsewhere is left alone.
func unrouteDomain(api *cf.Client, domain string) error {
	step("Catch-all *@" + domain)
	z, err := findZone(api, domain)
	if err != nil {
		return err
	}
	path := "/zones/" + z.ID + "/email/routing/rules/catch_all"
	var rule struct {
		Name     string           `json:"name"`
		Enabled  bool             `json:"enabled"`
		Matchers []map[string]any `json:"matchers"`
		Actions  []struct {
			Type  string   `json:"type"`
			Value []string `json:"value,omitempty"`
		} `json:"actions"`
	}
	if err := api.Do("GET", path, nil, &rule); err != nil {
		return err
	}
	if !rule.Enabled || len(rule.Actions) != 1 || rule.Actions[0].Type != "worker" || !slices.Contains(rule.Actions[0].Value, workerName) {
		done("tidak mengarah ke Wmail, dibiarkan")
		return nil
	}
	rule.Enabled = false
	if err := api.Do("PUT", path, rule, nil); err != nil {
		return err
	}
	done("dimatikan")
	return nil
}

func maskTail(s string) string {
	if len(s) < 4 {
		return "••••"
	}
	return "••••" + s[len(s)-4:]
}

func (c *ctx) ensureTunnel(api *cf.Client, account, zone, host string) (id, token string, err error) {
	var tunnels []struct{ ID string }
	if err = api.Do("GET", "/accounts/"+account+"/cfd_tunnel?is_deleted=false&name="+tunnelName, nil, &tunnels); err != nil {
		return
	}
	if len(tunnels) > 0 {
		id = tunnels[0].ID
	} else {
		var created struct{ ID string }
		body := map[string]string{"name": tunnelName, "config_src": "cloudflare", "tunnel_secret": randomString(32, base64.StdEncoding.EncodeToString)}
		if err = api.Do("POST", "/accounts/"+account+"/cfd_tunnel", body, &created); err != nil {
			return
		}
		id = created.ID
	}

	ingress := map[string]any{"config": map[string]any{"ingress": []map[string]string{
		{"hostname": host, "service": "http://localhost:" + c.cfg.Port},
		{"service": "http_status:404"},
	}}}
	if err = api.Do("PUT", "/accounts/"+account+"/cfd_tunnel/"+id+"/configurations", ingress, nil); err != nil {
		return
	}

	target := id + ".cfargotunnel.com"
	if err = c.ensureDNS(api, zone, dnsRecord{Type: "CNAME", Name: host, Content: target, Proxied: true}, true); err != nil {
		return
	}
	err = api.Do("GET", "/accounts/"+account+"/cfd_tunnel/"+id+"/token", nil, &token)
	return
}

type dnsRecord struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Content  string `json:"content"`
	Proxied  bool   `json:"proxied"`
	TTL      int    `json:"ttl"`
	Priority *int   `json:"priority,omitempty"`
	Comment  string `json:"comment,omitempty"`
}

// ensureDNS creates rec unless an identical one exists. With exclusive, other records
// on the same name block it (and may be replaced after confirmation).
func (c *ctx) ensureDNS(api *cf.Client, zone string, rec dnsRecord, exclusive bool) error {
	var existing []dnsRecord
	if err := api.Do("GET", "/zones/"+zone+"/dns_records?name="+url.QueryEscape(rec.Name), nil, &existing); err != nil {
		return err
	}
	for _, e := range existing {
		if e.Type == rec.Type && strings.Trim(e.Content, `"`) == strings.Trim(rec.Content, `"`) {
			return nil
		}
	}
	if exclusive {
		for _, e := range existing {
			if !confirm(fmt.Sprintf("\n    Record %s %s -> %s sudah ada. Hapus dan ganti?", e.Type, e.Name, e.Content), false) {
				return fmt.Errorf("record %s sudah dipakai; pilih hostname lain", rec.Name)
			}
			if err := api.Do("DELETE", "/zones/"+zone+"/dns_records/"+e.ID, nil, nil); err != nil {
				return err
			}
		}
	}
	rec.TTL = 1
	rec.Comment = "wmail"
	return api.Do("POST", "/zones/"+zone+"/dns_records", rec, nil)
}

func uploadWorker(api *cf.Client, account string, script []byte, publicURL, secret, nsID string) error {
	meta, _ := json.Marshal(map[string]any{
		"main_module":        "worker.js",
		"compatibility_date": "2025-01-01",
		"bindings": []map[string]string{
			{"type": "plain_text", "name": "APP_URL", "text": publicURL},
			{"type": "secret_text", "name": "INBOUND_SECRET", "text": secret},
			{"type": "kv_namespace", "name": "FALLBACK", "namespace_id": nsID},
		},
	})
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("metadata", string(meta))
	part, err := mw.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="worker.js"; filename="worker.js"`},
		"Content-Type":        {"application/javascript+module"},
	})
	if err != nil {
		return err
	}
	part.Write(script)
	mw.Close()
	return api.DoRaw("PUT", "/accounts/"+account+"/workers/scripts/"+workerName, mw.FormDataContentType(), &buf, nil)
}

/* ---------- Resend domain + DNS ---------- */

type resendRecord struct {
	Record, Name, Type, Value, Status string
	Priority                          *int
}

func (c *ctx) resendSetup() error { return c.resendDomains(c.db.Domains()) }

// resendDomains registers each domain at Resend and puts its DKIM/SPF/DMARC records in its zone.
func (c *ctx) resendDomains(domains []string) error {
	key, token := c.db.GetSetting("resend_api_key"), c.db.GetSetting("cf_api_token")
	if key == "" || len(domains) == 0 {
		return errors.New("set dulu resend_api_key dan domain")
	}
	if token == "" {
		return errors.New("jalankan `wmail cloudflare setup` dulu (dibutuhkan untuk memasang DNS)")
	}
	hc := &http.Client{Timeout: 30 * time.Second}
	api := cf.New(token)

	var list struct {
		Data []struct{ ID, Name string }
	}
	if err := service.ResendDo(hc, key, "GET", "/domains", nil, &list); err != nil {
		if strings.Contains(err.Error(), "restricted_api_key") || strings.Contains(err.Error(), "(401)") {
			return fmt.Errorf("API key ini hanya untuk kirim. Buat key \"Full access\" di resend.com untuk setup otomatis, atau verifikasi domain manual di dashboard Resend")
		}
		return err
	}
	var errs []error
	for _, domain := range domains {
		id := ""
		for _, d := range list.Data {
			if d.Name == domain {
				id = d.ID
			}
		}
		if err := c.resendDomain(hc, api, key, id, domain); err != nil {
			fmt.Printf("  ✗ %s: %v\n", domain, err)
			errs = append(errs, fmt.Errorf("%s: %w", domain, err))
		}
	}
	fmt.Println("  Verifikasi biasanya selesai dalam beberapa menit; cek di https://resend.com/domains atau jalankan ulang `wmail resend setup`.")
	return errors.Join(errs...)
}

func (c *ctx) resendDomain(hc *http.Client, api *cf.Client, key, id, domain string) error {
	z, err := findZone(api, domain)
	if err != nil {
		return err
	}
	zone := z.ID
	if id == "" {
		var created struct{ ID string }
		if err := service.ResendDo(hc, key, "POST", "/domains", map[string]string{"name": domain}, &created); err != nil {
			return err
		}
		id = created.ID
		fmt.Printf("  • Domain %s didaftarkan di Resend\n", domain)
	}

	var dom struct {
		Status  string
		Records []resendRecord
	}
	if err := service.ResendDo(hc, key, "GET", "/domains/"+id, nil, &dom); err != nil {
		return err
	}
	for _, r := range dom.Records {
		name := r.Name
		if name == "" || name == "@" {
			name = domain
		} else if !strings.HasSuffix(name, domain) {
			name += "." + domain
		}
		fmt.Printf("  • DNS %s %s (%s)... ", r.Type, name, r.Record)
		if err := c.ensureDNS(api, zone, dnsRecord{Type: r.Type, Name: name, Content: r.Value, Priority: r.Priority}, false); err != nil {
			fmt.Println("✗", err)
			continue
		}
		fmt.Println("✓")
	}
	fmt.Print("  • DNS TXT _dmarc." + domain + "... ")
	var dmarc []dnsRecord
	_ = api.Do("GET", "/zones/"+zone+"/dns_records?type=TXT&name=_dmarc."+domain, nil, &dmarc)
	if len(dmarc) == 0 {
		if err := api.Do("POST", "/zones/"+zone+"/dns_records", dnsRecord{Type: "TXT", Name: "_dmarc." + domain, Content: "v=DMARC1; p=none;", TTL: 1, Comment: "wmail"}, nil); err != nil {
			fmt.Println("✗", err)
		} else {
			fmt.Println("✓")
		}
	} else {
		fmt.Println("✓ sudah ada")
	}

	if err := service.ResendDo(hc, key, "POST", "/domains/"+id+"/verify", nil, nil); err != nil {
		return err
	}
	fmt.Printf("  • Verifikasi %s dimulai (status saat ini: %s)\n", domain, dom.Status)
	return nil
}
