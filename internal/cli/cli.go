// Package cli implements the wmail command line: server, account management,
// settings and the interactive setup wizard.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"wmail/internal/api"
	"wmail/internal/backup"
	"wmail/internal/cf"
	"wmail/internal/config"
	"wmail/internal/db"
	"wmail/internal/service"

	"golang.org/x/term"
)

type Assets struct {
	Web      fs.FS
	WorkerJS []byte
}

const usage = `Wmail - webmail untuk domain sendiri

Pemakaian: wmail <perintah> [argumen]

Server
  serve                                 jalankan web server (default)
  setup                                 wizard interaktif (domain, admin, Cloudflare, Resend, SES)

Domain
  domain list                           daftar domain email (yang pertama = utama)
  domain add <domain> [domain...]       tambah satu atau beberapa domain sekaligus,
                                        lalu pasang Email Routing (+ Resend bila ada key)

Akun
  user add <nama> [--admin]             buat akun (password ditanya)
  user list                             daftar akun beserta aksesnya
  user passwd <nama>                    ganti password
  user del <nama>                       hapus akun

Mailbox & akses
  mailbox add <alamat> [--name "Nama"]  buat alamat email
  mailbox list
  mailbox del <alamat>                  hapus alamat beserta semua emailnya
  grant <user> <alamat> [--send]        beri akses (default: hanya menerima)
  revoke <user> <alamat>                cabut akses

Konfigurasi
  config set <key> [nilai]              nilai kosong = ditanya (tersembunyi untuk secret)
  config get <key>
  config show                           semua setting (secret disamarkan)
  cloudflare setup                      buat KV, Tunnel, Worker, Email Routing otomatis
  resend setup                          daftarkan domain di Resend + pasang DNS di Cloudflare
  test-send <dari-alamat> <ke-alamat>   kirim email uji lewat provider aktif

Backup (terenkripsi dengan age; server hanya menyimpan kunci publik)
  backup key                            buat kunci backup; kunci rahasia tampil sekali, simpan di luar server
  backup > file.tar.gz.age              backup database + lampiran ke stdout
  restore < file.tar.gz.age             GANTI semua data dengan isi backup (kunci rahasia ditanya)

Database
  db copy-to [postgres://...]           salin semua data SQLite ke PostgreSQL kosong (URL kosong = ditanya tersembunyi)
`

func Run(args []string, assets Assets) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		fmt.Print(usage)
		return nil
	}

	cfg := config.LoadConfig()
	d, err := db.InitDB(cfg.DSN())
	if err != nil {
		return err
	}
	defer d.Close()
	c := &ctx{cfg: cfg, db: d, assets: assets}

	switch cmd {
	case "serve":
		return c.serve()
	case "setup":
		return c.setup()
	case "domain":
		return c.domain(args)
	case "user":
		return c.user(args)
	case "mailbox":
		return c.mailbox(args)
	case "grant", "revoke":
		return c.grant(cmd, args)
	case "config":
		return c.config(args)
	case "cloudflare":
		if len(args) == 1 && args[0] == "setup" {
			return c.cloudflareSetup()
		}
	case "resend":
		if len(args) == 1 && args[0] == "setup" {
			return c.resendSetup()
		}
	case "test-send":
		return c.testSend(args)
	case "backup":
		return c.backup(args)
	case "restore":
		return c.restore(args)
	case "db":
		if len(args) > 0 && args[0] == "copy-to" {
			return c.copyTo(args[1:])
		}
	}
	fmt.Print(usage)
	return fmt.Errorf("perintah tidak dikenal: %s %s", cmd, strings.Join(args, " "))
}

type ctx struct {
	cfg    *config.Config
	db     *db.DB
	assets Assets
}

/* ---------- serve ---------- */

func (c *ctx) serve() error {
	a := api.NewAPI(c.cfg, c.db)
	server := &http.Server{
		Addr:         net.JoinHostPort(c.cfg.BindAddr, c.cfg.Port),
		Handler:      api.SetupRouter(a, c.assets.Web),
		ReadTimeout:  120 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go a.Inbound().RunKVDrain(sigCtx, 5*time.Minute)
	go a.RunScheduled(sigCtx, 30*time.Second)

	where := c.cfg.DBPath
	if c.cfg.DBURL != "" {
		where = "PostgreSQL" // the URL may hold a password
	}
	log.Printf("Wmail berjalan di http://%s (db: %s)", server.Addr, where)
	if c.db.CountAdmins() == 0 {
		log.Printf("PERINGATAN: belum ada admin. Jalankan: wmail user add <nama> --admin")
	}

	errc := make(chan error, 1)
	go func() { errc <- server.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-sigCtx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Shutdown(shutdownCtx)
}

/* ---------- user ---------- */

func (c *ctx) user(args []string) error {
	args, isAdmin := popFlag(args, "--admin")
	if len(args) == 0 {
		return errors.New("pakai: wmail user add|list|passwd|del")
	}
	switch {
	case args[0] == "list":
		return c.userList()
	case len(args) != 2:
		return errors.New("nama user wajib diisi")
	case args[0] == "add":
		pw, err := newPassword()
		if err != nil {
			return err
		}
		if _, err := c.db.CreateUser(args[1], pw, isAdmin); err != nil {
			return err
		}
		fmt.Printf("✓ User %s dibuat%s\n", args[1], map[bool]string{true: " (admin)"}[isAdmin])
		return nil
	case args[0] == "passwd":
		u, err := c.db.GetUserByName(args[1])
		if err != nil {
			return fmt.Errorf("user %s tidak ada", args[1])
		}
		pw, err := newPassword()
		if err != nil {
			return err
		}
		if err := c.db.SetPassword(u.ID, pw); err != nil {
			return err
		}
		fmt.Println("✓ Password diganti, semua sesi login user ini dikeluarkan")
		return nil
	case args[0] == "del":
		u, err := c.db.GetUserByName(args[1])
		if err != nil {
			return fmt.Errorf("user %s tidak ada", args[1])
		}
		if err := c.db.DeleteUser(u.ID); err != nil {
			return err
		}
		fmt.Printf("✓ User %s dihapus\n", args[1])
		return nil
	}
	return errors.New("pakai: wmail user add|list|passwd|del")
}

func (c *ctx) userList() error {
	users, err := c.db.ListUsers()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "USER\tPERAN\tAKSES")
	for _, u := range users {
		role, access := "user", ""
		if u.IsAdmin {
			role, access = "admin", "semua mailbox"
		} else {
			mbs, _ := c.db.MailboxesForUser(&u)
			var parts []string
			for _, m := range mbs {
				parts = append(parts, m.Address+map[bool]string{true: " (kirim+terima)", false: " (terima)"}[m.CanSend])
			}
			access = strings.Join(parts, ", ")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", u.Username, role, access)
	}
	return tw.Flush()
}

/* ---------- domains ---------- */

// domain adds mail domains and sets up Email Routing, and Resend + DNS when a key is set.
// Domains already in the list are set up again, so a failed run can simply be repeated.
func (c *ctx) domain(args []string) error {
	if len(args) == 1 && args[0] == "list" {
		for i, d := range c.db.Domains() {
			fmt.Println(d + map[bool]string{true: "  (utama)"}[i == 0])
		}
		return nil
	}
	if len(args) < 2 || args[0] != "add" {
		return errors.New("pakai: wmail domain add <domain> [domain...] | list")
	}
	want := db.ParseDomains(strings.Join(args[1:], " "))
	if len(want) == 0 {
		return errors.New("pakai: wmail domain add <domain> [domain...]")
	}
	if err := api.ValidateSetting("domain", strings.Join(want, " ")); err != nil {
		return err
	}
	all := c.db.Domains()
	for _, d := range want {
		if !slices.Contains(all, d) {
			all = append(all, d)
		}
	}
	if err := c.db.SetSetting("domain", strings.Join(all, ", ")); err != nil {
		return err
	}
	fmt.Println("✓ Domain: " + strings.Join(all, ", "))

	token, account := c.db.GetSetting("cf_api_token"), c.db.GetSetting("cf_account_id")
	if token == "" || account == "" {
		fmt.Println("  Cloudflare belum disiapkan; lanjutkan dengan: sudo wmail cloudflare setup")
		return nil
	}
	routed, err := routeDomains(cf.New(token), account, want)
	if len(routed) > 0 && c.db.GetSetting("resend_api_key") != "" {
		err = errors.Join(err, c.resendDomains(routed))
	}
	if c.db.GetSetting("aws_access_key_id") != "" {
		fmt.Println("  Ingat: verifikasi juga " + strings.Join(want, ", ") + " di konsol SES (Identities).")
	}
	if err != nil {
		fmt.Println("\n  Setelah diperbaiki, ulangi: sudo wmail domain add " + strings.Join(want, " "))
	}
	return err
}

/* ---------- mailbox & grants ---------- */

func (c *ctx) mailbox(args []string) error {
	args, name := popValue(args, "--name")
	if len(args) == 0 {
		return errors.New("pakai: wmail mailbox add|list|del")
	}
	switch {
	case args[0] == "list":
		mbs, err := c.db.ListMailboxes()
		if err != nil {
			return err
		}
		for _, m := range mbs {
			fmt.Printf("%-40s %s\n", m.Address, m.DisplayName)
		}
		return nil
	case len(args) != 2:
		return errors.New("alamat wajib diisi")
	case args[0] == "add":
		if _, err := c.db.CreateMailbox(args[1], name); err != nil {
			return err
		}
		addr := c.db.FullAddress(args[1])
		fmt.Printf("✓ Mailbox %s dibuat\n", addr)
		if _, dom, _ := strings.Cut(addr, "@"); !slices.Contains(c.db.Domains(), dom) {
			fmt.Printf("  Catatan: %s belum terdaftar sebagai domain; tambahkan dengan: wmail domain add %s\n", dom, dom)
		}
		return nil
	case args[0] == "del":
		mb, err := c.db.MailboxByAddress(args[1])
		if err != nil {
			return fmt.Errorf("mailbox %s tidak ada", args[1])
		}
		if !confirm(fmt.Sprintf("Hapus %s beserta SEMUA emailnya?", mb.Address), false) {
			return nil
		}
		if err := c.db.DeleteMailbox(mb.ID); err != nil {
			return err
		}
		fmt.Printf("✓ Mailbox %s dihapus\n", mb.Address)
		return nil
	}
	return errors.New("pakai: wmail mailbox add|list|del")
}

func (c *ctx) grant(cmd string, args []string) error {
	args, canSend := popFlag(args, "--send")
	if len(args) != 2 {
		return fmt.Errorf("pakai: wmail %s <user> <alamat>", cmd)
	}
	u, err := c.db.GetUserByName(args[0])
	if err != nil {
		return fmt.Errorf("user %s tidak ada", args[0])
	}
	mb, err := c.db.MailboxByAddress(args[1])
	if err != nil {
		return fmt.Errorf("mailbox %s tidak ada", args[1])
	}
	if cmd == "revoke" {
		if err := c.db.DeleteGrant(u.ID, mb.ID); err != nil {
			return err
		}
		fmt.Printf("✓ Akses %s ke %s dicabut\n", u.Username, mb.Address)
		return nil
	}
	if err := c.db.SetGrant(u.ID, mb.ID, canSend); err != nil {
		return err
	}
	fmt.Printf("✓ %s sekarang bisa %s %s\n", u.Username, map[bool]string{true: "mengirim & menerima di", false: "menerima (baca saja) di"}[canSend], mb.Address)
	return nil
}

/* ---------- config ---------- */

func (c *ctx) config(args []string) error {
	if len(args) == 1 && args[0] == "show" {
		s, err := c.db.GetSettings()
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		for _, k := range db.SettingKeys {
			v := s[k.Key]
			if k.Secret {
				v = db.MaskSecret(v)
			}
			if v == "" {
				v = "-"
			}
			fmt.Fprintf(tw, "%s\t%s\t# %s\n", k.Key, v, k.Desc)
		}
		return tw.Flush()
	}
	if len(args) == 2 && args[0] == "get" {
		fmt.Println(c.db.GetSetting(args[1]))
		return nil
	}
	if len(args) >= 2 && args[0] == "set" {
		k, ok := db.LookupSettingKey(args[1])
		if !ok {
			return fmt.Errorf("key tidak dikenal: %s (lihat `wmail config show`)", args[1])
		}
		var v string
		if len(args) >= 3 {
			v = strings.Join(args[2:], " ")
		} else if k.Secret {
			v = promptSecret(k.Desc)
		} else {
			v = prompt(k.Desc, c.db.GetSetting(k.Key))
		}
		if err := api.ValidateSetting(k.Key, v); err != nil {
			return err
		}
		if err := c.db.SetSetting(k.Key, strings.TrimSpace(v)); err != nil {
			return err
		}
		fmt.Printf("✓ %s disimpan (langsung berlaku, tanpa restart)\n", k.Key)
		return nil
	}
	return errors.New("pakai: wmail config set <key> [nilai] | get <key> | show")
}

// copyTo moves everything from the SQLite file into an empty PostgreSQL database.
// backup streams an encrypted backup to stdout; `backup key` makes the key pair it needs.
func (c *ctx) backup(args []string) error {
	if len(args) == 1 && args[0] == "key" {
		return c.backupKey()
	}
	if len(args) > 0 || term.IsTerminal(int(os.Stdout.Fd())) {
		return errors.New("pakai: sudo wmail backup > wmail-$(date +%F).tar.gz.age")
	}
	key := c.db.GetSetting("backup_recipient")
	if key == "" {
		return errors.New("belum ada kunci backup, buat dulu: sudo wmail backup key")
	}
	if err := backup.Write(os.Stdout, c.db, key, filepath.Dir(c.cfg.DBPath)); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "✓ Backup selesai, terenkripsi untuk "+key)
	return nil
}

func (c *ctx) backupKey() error {
	if old := c.db.GetSetting("backup_recipient"); old != "" &&
		!confirm("Sudah ada kunci backup "+old+". Ganti? (backup lama tetap butuh kunci rahasia lamanya)", false) {
		return nil
	}
	secret, public, err := backup.NewKey()
	if err != nil {
		return err
	}
	if err := c.db.SetSetting("backup_recipient", public); err != nil {
		return err
	}
	fmt.Println("Kunci rahasia backup. Simpan di luar server, mis. di password manager: server tidak menyimpannya,")
	fmt.Println("dan tanpa kunci ini backup tidak bisa dibuka.")
	fmt.Println("\n" + secret + "\n")
	fmt.Println("Kunci publik " + public + " disimpan sebagai backup_recipient.")
	return nil
}

// restore reads a backup from stdin and asks for the secret key on the terminal.
func (c *ctx) restore(args []string) error {
	if len(args) > 0 || term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("pakai: sudo wmail restore < wmail-2026-10-22.tar.gz.age")
	}
	host := c.cfg.BindAddr
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	if conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, c.cfg.Port), time.Second); err == nil {
		conn.Close()
		return errors.New("Wmail masih berjalan, hentikan dulu: sudo systemctl stop wmail")
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return errors.New("restore perlu terminal untuk menanyakan kunci rahasia")
	}
	defer tty.Close()
	fmt.Fprint(tty, "Restore MENGGANTI semua data Wmail (akun, mailbox, email, setting) dengan isi backup.\nKetik 'ganti' untuk lanjut: ")
	if answer, _ := bufio.NewReader(tty).ReadString('\n'); strings.TrimSpace(answer) != "ganti" {
		return errors.New("dibatalkan")
	}
	fmt.Fprint(tty, "Kunci rahasia backup (AGE-SECRET-KEY-1...): ")
	secret, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(tty)
	if err != nil {
		return err
	}
	if err := backup.Restore(os.Stdin, strings.TrimSpace(string(secret)), c.db, c.cfg.DataDir); err != nil {
		return err
	}
	fmt.Fprintln(tty, "✓ Data dipulihkan. Jalankan sudo ./install.sh untuk menyalakan Wmail lagi (dan cloudflared dengan token dari backup).")
	return nil
}

// The URL is asked when not given, so its password stays out of ps and shell history.
func (c *ctx) copyTo(args []string) error {
	var url string
	if len(args) > 0 {
		url = args[0]
	} else {
		url = promptSecret("URL PostgreSQL tujuan (postgres://user:password@host:5432/nama_db)")
	}
	if c.cfg.DBURL != "" {
		return errors.New("DATABASE_URL sudah terisi: sumber salinan harus database SQLite")
	}
	if !db.IsPostgres(url) {
		return errors.New("tujuan harus URL postgres://user:password@host:5432/nama_db")
	}
	dst, err := db.InitDB(url)
	if err != nil {
		return err
	}
	defer dst.Close()
	fmt.Println("Menyalin " + c.cfg.DBPath + " ke PostgreSQL ...")
	if err := c.db.CopyTo(dst, false); err != nil {
		return err
	}
	fmt.Println("✓ Selesai. Tambahkan DATABASE_URL=<url tadi> ke file env (server: /etc/wmail/wmail.env),")
	fmt.Println("  lalu jalankan lagi wmail. File SQLite tidak diubah dan bisa dipakai lagi dengan menghapus DATABASE_URL.")
	return nil
}

func (c *ctx) testSend(args []string) error {
	if len(args) != 2 {
		return errors.New("pakai: wmail test-send <dari-alamat> <ke-alamat>")
	}
	mb, err := c.db.MailboxByAddress(args[0])
	if err != nil {
		return fmt.Errorf("mailbox %s tidak ada", args[0])
	}
	res, err := service.NewMailer(c.db).Send(service.SendOptions{
		FromAddress: mb.Address,
		FromName:    mb.DisplayName,
		To:          []string{args[1]},
		Subject:     "Tes Wmail",
		Text:        "Email uji dari Wmail. Kalau ini sampai, pengiriman sudah berfungsi.",
		HTML:        "<p>Email uji dari Wmail. Kalau ini sampai, pengiriman sudah berfungsi.</p>",
	})
	if err != nil {
		return err
	}
	fmt.Printf("✓ Terkirim lewat %s (id %s)\n", res.Provider, res.MessageID)
	return nil
}

/* ---------- setup wizard ---------- */

func (c *ctx) setup() error {
	fmt.Println("\n=== Wmail setup ===  (Enter = pakai nilai di [kurung]; setiap langkah bisa diulang)")

	fmt.Println("\n[1/6] Domain")
	in := prompt("Domain email; beberapa dipisah koma, yang pertama domain utama (mis. contoh.com, toko.id)", strings.Join(c.db.Domains(), ", "))
	domains := db.ParseDomains(in)
	if len(domains) == 0 {
		return errors.New("domain wajib diisi")
	}
	if err := api.ValidateSetting("domain", in); err != nil {
		return err
	}
	_ = c.db.SetSetting("domain", strings.Join(domains, ", "))
	domain := domains[0]
	host := strings.TrimPrefix(c.db.GetSetting("public_url"), "https://")
	if host == "" {
		host = "mail." + domain
	}
	host = prompt("Hostname web (lewat Cloudflare Tunnel)", host)
	_ = c.db.SetSetting("public_url", "https://"+strings.TrimPrefix(host, "https://"))

	fmt.Println("\n[2/6] Akun admin")
	if c.db.CountAdmins() > 0 {
		fmt.Println("Admin sudah ada, dilewati. (Tambah lagi: wmail user add <nama> --admin)")
	} else {
		name := prompt("Username admin", "admin")
		pw, err := newPassword()
		if err != nil {
			return err
		}
		if _, err := c.db.CreateUser(name, pw, true); err != nil {
			return err
		}
		fmt.Printf("✓ Admin %s dibuat\n", name)
	}

	fmt.Println("\n[3/6] Mailbox")
	if mbs, _ := c.db.ListMailboxes(); len(mbs) > 0 {
		fmt.Printf("Sudah ada %d mailbox, dilewati. (Tambah: wmail mailbox add <alamat>)\n", len(mbs))
	} else if addr := prompt("Alamat email pertama (kosongkan untuk lewati)", "admin@"+domain); addr != "" {
		name := prompt("Nama tampilan pengirim", "")
		if _, err := c.db.CreateMailbox(addr, name); err != nil {
			return err
		}
		fmt.Printf("✓ Mailbox %s dibuat\n", addr)
	}

	fmt.Println("\n[4/6] Cloudflare (Tunnel + Worker + Email Routing)")
	if confirm("Hubungkan akun Cloudflare sekarang?", true) {
		if err := c.cloudflareSetup(); err != nil {
			fmt.Printf("✗ Cloudflare setup gagal: %v\n  Perbaiki lalu jalankan ulang: wmail cloudflare setup\n", err)
		}
	}

	fmt.Println("\n[5/6] Resend (pengiriman utama)")
	if key := promptSecret("Resend API key (Enter = " + keepOrSkip(c.db.GetSetting("resend_api_key")) + ")"); key != "" {
		_ = c.db.SetSetting("resend_api_key", key)
	}
	if c.db.GetSetting("resend_api_key") != "" && c.db.GetSetting("cf_zone_id") != "" &&
		confirm("Daftarkan "+strings.Join(domains, ", ")+" di Resend dan pasang DNS (DKIM/SPF) otomatis?", true) {
		if err := c.resendSetup(); err != nil {
			fmt.Printf("✗ Resend setup gagal: %v\n  Jalankan ulang nanti: wmail resend setup\n", err)
		}
	}

	fmt.Println("\n[6/6] Amazon SES (cadangan, opsional)")
	if confirm("Konfigurasi Amazon SES?", c.db.GetSetting("aws_access_key_id") != "") {
		region := prompt("Region SES", orDefault(c.db.GetSetting("aws_region"), "ap-southeast-1"))
		keyID := prompt("IAM Access Key ID (izin ses:SendRawEmail)", c.db.GetSetting("aws_access_key_id"))
		secret := promptSecret("IAM Secret Access Key (Enter = " + keepOrSkip(c.db.GetSetting("aws_secret_access_key")) + ")")
		if secret == "" {
			secret = c.db.GetSetting("aws_secret_access_key")
		}
		fmt.Print("Menguji login SMTP SES... ")
		if err := service.CheckSES(region, keyID, secret); err != nil {
			fmt.Printf("gagal: %v\n", err)
		} else {
			fmt.Println("berhasil")
		}
		_ = c.db.SetSetting("aws_region", region)
		_ = c.db.SetSetting("aws_access_key_id", keyID)
		_ = c.db.SetSetting("aws_secret_access_key", secret)
		fmt.Println("  Ingat: verifikasi domain " + strings.Join(domains, ", ") + " di konsol SES (Identities) dan keluar dari sandbox SES.")
	}

	fmt.Println("\n=== Selesai ===")
	fmt.Printf("Web: %s (lokal: http://%s, ubah lewat BIND_ADDR)\n", c.db.GetSetting("public_url"), net.JoinHostPort(c.cfg.BindAddr, c.cfg.Port))
	fmt.Println("Cek konfigurasi: wmail config show · Uji kirim: wmail test-send <dari> <ke>")
	return nil
}

/* ---------- prompts ---------- */

var stdin = bufio.NewReader(os.Stdin)

func prompt(label, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, _ := stdin.ReadString('\n')
	if line = strings.TrimSpace(line); line == "" {
		return def
	}
	return line
}

func promptSecret(label string) string {
	fmt.Printf("%s: ", label)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, _ := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		return strings.TrimSpace(string(b))
	}
	line, _ := stdin.ReadString('\n')
	return strings.TrimSpace(line)
}

func confirm(label string, def bool) bool {
	hint := map[bool]string{true: "Y/n", false: "y/N"}[def]
	switch strings.ToLower(prompt(label+" ["+hint+"]", "")) {
	case "y", "ya", "yes":
		return true
	case "n", "no", "tidak":
		return false
	}
	return def
}

func newPassword() (string, error) {
	pw := promptSecret("Password (min. 8 karakter)")
	if pw != promptSecret("Ulangi password") {
		return "", errors.New("password tidak sama")
	}
	return pw, nil
}

func keepOrSkip(current string) string {
	if current != "" {
		return "pakai yang tersimpan " + db.MaskSecret(current)
	}
	return "lewati"
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// popFlag removes a boolean flag from anywhere in args.
func popFlag(args []string, flag string) ([]string, bool) {
	var rest []string
	found := false
	for _, a := range args {
		if a == flag {
			found = true
		} else {
			rest = append(rest, a)
		}
	}
	return rest, found
}

// popValue removes "--flag value" from anywhere in args.
func popValue(args []string, flag string) ([]string, string) {
	var rest []string
	val := ""
	for i := 0; i < len(args); i++ {
		if args[i] == flag && i+1 < len(args) {
			val = args[i+1]
			i++
		} else {
			rest = append(rest, args[i])
		}
	}
	return rest, val
}
