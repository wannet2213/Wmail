#!/usr/bin/env bash
# Wmail installer: build, systemd service, setup wizard, cloudflared.
# Run from the project folder:  sudo ./install.sh
# Re-running upgrades the binary and restarts the service (the wizard can be skipped).
set -euo pipefail

SRC="$(cd "$(dirname "$0")" && pwd)"
PREFIX=/opt/wmail
DATA=/var/lib/wmail
ETC=/etc/wmail
ENVFILE=$ETC/wmail.env
GO_MIN=$(awk '/^go /{print $2}' "$SRC/go.mod")

bold() { printf '\n\033[1m%s\033[0m\n' "$*"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; }
die()  { printf '\033[31m✗ %s\033[0m\n' "$*" >&2; exit 1; }
ask()  { local a; read -r -p "$1 [$2]: " a; echo "${a:-$2}"; }
# confirm "question" [Y/n|y/N]: the capital letter is the default.
confirm() { local hint=${2:-Y/n} a; read -r -p "$1 [$hint]: " a; [[ -z $a && $hint == Y/n ]] && return 0; [[ ${a,,} == y* ]]; }

[[ $EUID -eq 0 ]] || die "Jalankan dengan sudo: sudo ./install.sh"
command -v systemctl >/dev/null || die "systemd dibutuhkan."

case "$(uname -m)" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  armv7l|armv6l) ARCH=arm ;;
  *) die "Arsitektur $(uname -m) belum didukung." ;;
esac

bold "1. Build Wmail ($ARCH)"
GO=$(command -v go || true)
[[ -z $GO && -x /usr/local/go/bin/go ]] && GO=/usr/local/go/bin/go
if [[ -z $GO && -x $SRC/wmail ]] && confirm "Go tidak ditemukan. Pakai binary siap pakai $SRC/wmail?"; then
  install -D -m 0755 "$SRC/wmail" "$PREFIX/wmail"
else
  if [[ -z $GO ]]; then
    LATEST=$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -1)
    confirm "Go belum terpasang. Unduh $LATEST resmi dari go.dev ke /usr/local/go?" || die "Pasang Go >= $GO_MIN lalu ulangi."
    curl -fsSL "https://go.dev/dl/$LATEST.linux-$ARCH.tar.gz" -o /tmp/go.tgz
    rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz
    GO=/usr/local/go/bin/go
  fi
  ok "$($GO version)"
  (cd "$SRC" && GOTOOLCHAIN=auto CGO_ENABLED=0 "$GO" build -trimpath -ldflags='-s -w' -o "$PREFIX/wmail.new" .)
  mv "$PREFIX/wmail.new" "$PREFIX/wmail"
fi
ok "$PREFIX/wmail"

bold "2. User sistem, folder data, konfigurasi"
id wmail >/dev/null 2>&1 || useradd --system --home-dir "$DATA" --shell /usr/sbin/nologin wmail
install -d -o wmail -g wmail -m 0750 "$DATA"
install -d -m 0755 "$ETC"
if [[ ! -f $ENVFILE ]]; then
  PORT=$(ask "Port lokal web server" 8080)
  cat > "$ENVFILE" <<EOF
PORT=$PORT
# 127.0.0.1: hanya lewat Cloudflare Tunnel. 0.0.0.0: juga bisa dibuka dari LAN (http://ip-lan:PORT, tanpa HTTPS).
BIND_ADDR=127.0.0.1
DB_PATH=$DATA/mail.db
DATA_DIR=$DATA/attachments
EOF
fi
chown root:wmail "$ENVFILE" && chmod 0640 "$ENVFILE"
ok "$ENVFILE"

# `sudo wmail ...` from anywhere: loads the env file and runs as the wmail user so file ownership stays right.
cat > /usr/local/bin/wmail <<EOF
#!/bin/sh
set -a; . $ENVFILE; set +a
cd $DATA 2>/dev/null || true
if [ "\$(id -u)" = 0 ]; then exec runuser -u wmail -- $PREFIX/wmail "\$@"; fi
exec $PREFIX/wmail "\$@"
EOF
chmod 0755 /usr/local/bin/wmail
ok "perintah: sudo wmail help"

cat > /etc/systemd/system/wmail.service <<EOF
[Unit]
Description=Wmail webmail
After=network-online.target
Wants=network-online.target

[Service]
User=wmail
Group=wmail
EnvironmentFile=$ENVFILE
WorkingDirectory=$DATA
ExecStart=$PREFIX/wmail serve
Restart=always
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$DATA

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
ok "wmail.service"

bold "3. Setup (domain, admin, Cloudflare, Resend, SES)"
FIRST=1
[[ -f $DATA/mail.db ]] && FIRST=0
if [[ $FIRST == 1 ]] || confirm "Jalankan wizard setup lagi?" "y/N"; then
  /usr/local/bin/wmail setup
fi

bold "4. Cloudflare Tunnel (cloudflared)"
TOKEN=$(/usr/local/bin/wmail config get cf_tunnel_token)
if [[ -z $TOKEN ]]; then
  echo "  Tunnel belum dibuat; lewati. Nanti: sudo wmail cloudflare setup, lalu jalankan installer ini lagi."
else
  if ! command -v cloudflared >/dev/null; then
    if command -v apt-get >/dev/null; then
      mkdir -p --mode=0755 /usr/share/keyrings
      curl -fsSL https://pkg.cloudflare.com/cloudflare-main.gpg -o /usr/share/keyrings/cloudflare-main.gpg
      echo "deb [signed-by=/usr/share/keyrings/cloudflare-main.gpg] https://pkg.cloudflare.com/cloudflared any main" > /etc/apt/sources.list.d/cloudflared.list
      apt-get update -qq && apt-get install -y -qq cloudflared
    else
      curl -fsSL "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-$ARCH" -o /usr/local/bin/cloudflared
      chmod 0755 /usr/local/bin/cloudflared
    fi
  fi
  ok "$(cloudflared --version 2>&1 | head -1)"
  # Keep the token off every command line and out of world-readable unit files:
  # printf is a bash builtin (never in argv), the env file is root-only, and systemd reads it
  # before starting cloudflared as an unprivileged dynamic user.
  TOKFILE=$ETC/cloudflared.env
  ( umask 077; printf 'TUNNEL_TOKEN=%s\n' "$TOKEN" > "$TOKFILE" )
  chown root:root "$TOKFILE" && chmod 0600 "$TOKFILE"
  OLDUNIT=/etc/systemd/system/cloudflared.service
  if [[ -f $OLDUNIT && $(< "$OLDUNIT") == *"$TOKEN"* ]]; then
    echo "  ! $OLDUNIT (dari versi installer lama) menyimpan token tunnel Wmail di ExecStart, bisa dibaca semua user lokal."
    if confirm "Hapus service lama itu dan pakai wmail-cloudflared?" "Y/n"; then
      cloudflared service uninstall || true
      echo "  Token lama sempat terbaca: rotasi token tunnel 'wmail' di dashboard Cloudflare, lalu jalankan"
      echo "  sudo wmail cloudflare setup && sudo ./install.sh"
    fi
  fi
  cat > /etc/systemd/system/wmail-cloudflared.service <<EOF
[Unit]
Description=Cloudflare Tunnel for Wmail
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=$TOKFILE
ExecStart=$(command -v cloudflared) --no-autoupdate tunnel run
DynamicUser=yes
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable wmail-cloudflared >/dev/null 2>&1
  systemctl restart wmail-cloudflared
  ok "cloudflared berjalan (wmail-cloudflared.service, user tanpa hak root)"
fi

bold "5. Menjalankan Wmail"
systemctl enable wmail >/dev/null 2>&1
systemctl restart wmail
sleep 1
systemctl is-active --quiet wmail && ok "wmail aktif" || die "wmail gagal start: journalctl -u wmail -n 50"

. "$ENVFILE"
URL=$(/usr/local/bin/wmail config get public_url)
bold "Selesai"
echo "  Web lokal : http://${BIND_ADDR:-127.0.0.1}:$PORT  (BIND_ADDR di $ENVFILE)"
[[ -n $URL ]] && echo "  Web publik: $URL"
echo "  Log       : journalctl -u wmail -f"
echo "  Perintah  : sudo wmail help   (user, mailbox, grant, config, test-send)"
