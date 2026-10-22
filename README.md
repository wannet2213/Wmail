# Wmail

Webmail untuk domain sendiri yang berjalan di home server tanpa IP publik.

- **Terima:** Cloudflare Email Routing → Worker → Cloudflare Tunnel → server rumah.
  Kalau server sedang mati, Worker menitipkan email di Workers KV, lalu Wmail mengambilnya saat hidup lagi (dicek tiap 5 menit).
- **Kirim:** Resend, dengan Amazon SES (SMTP) sebagai cadangan otomatis saat Resend gagal atau kuota gratisnya habis.
- **Akun:** admin bisa membuka semua mailbox. User hanya mailbox yang diberikan admin, dengan izin *terima saja* atau *kirim & terima*.
  Akun hanya bisa dibuat lewat CLI di server atau halaman Pengaturan admin; tidak ada pendaftaran publik.

```
Pengirim → Cloudflare Email Routing (catch-all) → Worker wmail-inbound
             ├─ POST raw MIME → https://mail.domain/api/inbound/raw (tunnel) → Wmail
             ├─ 404 (alamat tidak terdaftar) → email ditolak/bounce
             └─ server tidak terjangkau → disimpan di KV → diambil Wmail nanti
Browser → https://mail.domain (tunnel) → Wmail (Go + SQLite, listen 127.0.0.1; BIND_ADDR=0.0.0.0 untuk akses LAN)
Wmail → Resend API  ─(gagal / kuota habis)→  Amazon SES SMTP :587
```

## Instalasi

Kebutuhan: Linux dengan systemd (Debian/Ubuntu/Raspberry Pi OS paling mudah), domain yang DNS-nya di Cloudflare.

```bash
git clone <repo> wmail && cd wmail
sudo ./install.sh
```

Installer akan:
1. Build binary (Go dipasang otomatis dari go.dev bila belum ada).
2. Membuat user sistem `wmail`, data di `/var/lib/wmail`, dan service `wmail.service`.
3. Menjalankan wizard `wmail setup`:
   - domain dan hostname web (default `mail.<domain>`)
   - akun admin dan mailbox pertama
   - Cloudflare: buat KV, Tunnel + DNS, upload Worker, aktifkan Email Routing, catch-all → Worker
   - Resend: daftarkan domain dan pasang record DKIM/SPF/DMARC otomatis di Cloudflare
   - SES (opsional): uji login SMTP
4. Memasang `cloudflared` dengan token tunnel, lalu menyalakan Wmail.

Jalankan ulang `sudo ./install.sh` untuk upgrade.

### Cloudflare API token

Buat di <https://dash.cloudflare.com/profile/api-tokens> → *Custom token*, batasi ke zone domain Anda:

| Scope   | Permission          | Level |
|---------|---------------------|-------|
| Account | Workers Scripts     | Edit  |
| Account | Workers KV Storage  | Edit  |
| Account | Cloudflare Tunnel   | Edit  |
| Zone    | Zone                | Read  |
| Zone    | DNS                 | Edit  |
| Zone    | Email Routing Rules | Edit  |
| Zone    | Zone Settings       | Edit  |

Token disimpan di database server karena dipakai untuk menarik antrean KV.

### Resend dan SES

- **Resend:** setup otomatis butuh API key *Full access*. Key *Sending access* tetap bisa dipakai kirim, tapi domain harus diverifikasi manual di dashboard Resend.
- **SES:** buat IAM user dengan izin `ses:SendRawEmail`, verifikasi domain di konsol SES (Identities), lalu minta keluar dari sandbox.
  Password SMTP diturunkan otomatis dari secret key, jadi cukup isi region, access key, dan secret.
- **Mode `auto` (default):** Resend dipakai sampai 95 email/hari atau 2800/bulan (batas aman paket gratis), lalu beralih ke SES.
  Ubah batasnya dengan `resend_daily_limit` / `resend_monthly_limit` (`0` = tanpa batas).

## Perintah

Di server, semua perintah lewat `sudo wmail …`. Perubahan langsung berlaku tanpa restart.

```
wmail user add <nama> [--admin]          wmail mailbox add <alamat> [--name "Nama"]
wmail user list | passwd <nama> | del    wmail mailbox list | del <alamat>
wmail grant <user> <alamat> [--send]     wmail revoke <user> <alamat>
wmail config set <key> [nilai]           wmail config show | get <key>
wmail cloudflare setup                   wmail resend setup
wmail test-send <dari> <ke>              wmail setup
wmail backup key | backup > file.age     wmail restore < file.age
```

Contoh: `sudo wmail config set resend_api_key` meminta key secara tersembunyi.

## Database

Default-nya SQLite (`DB_PATH`), tanpa server tambahan. PostgreSQL 12+ dipakai bila
`DATABASE_URL=postgres://user:password@host:5432/nama_db` diisi di `/etc/wmail/wmail.env`.
Server PostgreSQL di mesin lain wajib lewat TLS: tambahkan `?sslmode=verify-full` (atau
`sslmode=require`, terenkripsi tanpa memeriksa sertifikat). Tanpa itu Wmail menolak start,
karena bawaan driver-nya diam-diam beralih ke koneksi tanpa enkripsi bila TLS gagal.

Pindah dari SQLite ke PostgreSQL: jalankan `sudo ./install.sh` dan jawab `y` di langkah
*Database*. Installer bisa memasang PostgreSQL di server ini (apt untuk Debian/Ubuntu, dnf untuk
Fedora/RHEL; database `wmail`, login lewat socket lokal tanpa password) atau memakai server PostgreSQL yang sudah ada (URL).
Lalu installer menghentikan Wmail, menyalin semua data ke database yang masih kosong,
menulis `DATABASE_URL` ke file env, dan menyalakan Wmail lagi. Email yang
masuk selama berhenti ditahan di KV Cloudflare. Bila penyalinan gagal, Wmail tetap memakai
SQLite. Langkah manualnya: hentikan Wmail, `sudo wmail db copy-to` (URL ditanya tersembunyi),
tambahkan `DATABASE_URL='postgres://…'` ke `/etc/wmail/wmail.env`, nyalakan lagi.

Lampiran tetap di `DATA_DIR`. File SQLite tidak diubah: hapus `DATABASE_URL` untuk kembali
ke SQLite (data yang masuk selama memakai PostgreSQL tidak ikut). Bedanya di PostgreSQL:
pencarian tidak mengabaikan aksen ("kafe" tidak menemukan "café") dan hanya 200.000
karakter pertama tiap email yang terindeks.

## Backup

Backup berisi database (selalu sebagai file SQLite, juga bila memakai PostgreSQL) dan semua
lampiran, dienkripsi dengan [age](https://age-encryption.org) ke sebuah kunci publik. Server
hanya menyimpan kunci publik itu, jadi backup yang bocor, atau server yang dibobol, tidak
membuka backup lama. Kunci rahasianya Anda yang simpan.

```bash
sudo wmail backup key                                   # sekali; simpan kunci rahasia yang tampil
sudo wmail backup > wmail-$(date +%F).tar.gz.age        # bisa dijadwalkan lewat cron
```

Memulihkan (di server lama atau baru; semua data saat ini DIGANTI isi backup):

```bash
sudo systemctl stop wmail
sudo wmail restore < wmail-2026-10-22.tar.gz.age        # kunci rahasia ditanya tersembunyi
sudo ./install.sh                                       # nyalakan lagi, cloudflared memakai token dari backup
```

Tanpa Wmail: `age -d -i kunci.txt wmail-….tar.gz.age | tar xz` mengeluarkan `mail.db`
dan folder `attachments/`.

## Development

```bash
cp .env.example .env
go run . user add admin --admin
go run . mailbox add saya@contoh.test
go run . config set inbound_secret dev
go run . serve      # http://localhost:8080
go test ./...
```

Simulasikan email masuk tanpa Cloudflare:

```bash
curl --data-binary @contoh.eml -H 'x-inbound-secret: dev' -H 'x-envelope-to: saya@contoh.test' \
  http://localhost:8080/api/inbound/raw
```

Struktur kode: `internal/api` (HTTP + auth), `internal/db` (SQLite), `internal/service` (kirim, terima, antrean KV),
`internal/cli` (perintah + wizard + provisioning Cloudflare/Resend), `worker/worker.js` (di-embed ke binary).
File web (`index.html`, `login.html`, `settings.html`, `assets/`) juga di-embed. Hanya file tersebut yang disajikan lewat HTTP.

Lisensi pihak ketiga: ikon Material Symbols di `assets/fonts` © Google, [Apache License 2.0](https://github.com/google/material-design-icons/blob/master/LICENSE). Teks UI memakai font sistem.
