/* Wmail settings: own password for everyone; users, mailboxes, access and providers for admins. */

const $ = (s) => document.querySelector(s);
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) =>
  ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

async function api(path, method = "GET", body) {
  const res = await fetch(path, {
    method,
    headers: body ? { "Content-Type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401) {
    location.href = "/login.html";
    throw new Error("unauthorized");
  }
  if (!res.ok) throw new Error((await res.text()).trim() || `Gagal (${res.status})`);
  return res.json();
}

function toast(msg) {
  $("#snackText").textContent = msg;
  $("#snackbar").hidden = false;
  clearTimeout(toast._t);
  toast._t = setTimeout(() => { $("#snackbar").hidden = true; }, 4000);
}

// run wraps an action: shows its result, or the server's error message.
async function run(fn, okMsg) {
  try {
    await fn();
    if (okMsg) toast(okMsg);
  } catch (err) {
    toast(err.message);
  }
}

let me = null;
let data = { users: [], mailboxes: [], grants: [] };

/* ---------- my account ---------- */

$("#pwForm").addEventListener("submit", (e) => {
  e.preventDefault();
  const current = $("#pwCurrent").value, next = $("#pwNew").value;
  if (next !== $("#pwRepeat").value) return toast("Password baru tidak sama.");
  run(async () => {
    await api("/api/me/password", "POST", { current, new: next });
    e.target.reset();
  }, "Password diganti. Sesi di perangkat lain sudah dikeluarkan.");
});

$("#logoutBtn").addEventListener("click", async () => {
  await fetch("/api/auth/logout", { method: "POST" });
  location.href = "/login.html";
});

/* ---------- admin: users ---------- */

function renderUsers() {
  $("#usersBody").innerHTML = data.users.map((u) => `
    <tr>
      <td>${esc(u.username)}${u.id === me.user.id ? ' <span class="muted">(Anda)</span>' : ""}</td>
      <td>${u.isAdmin ? "Admin" : "User"}</td>
      <td><div class="row-btns">
        <button class="text-btn" data-act="user-pw" data-id="${u.id}">Reset password</button>
        ${u.id === me.user.id ? "" : `
          <button class="text-btn" data-act="user-role" data-id="${u.id}">${u.isAdmin ? "Jadikan user" : "Jadikan admin"}</button>
          <button class="text-btn danger" data-act="user-del" data-id="${u.id}">Hapus</button>`}
      </div></td>
    </tr>`).join("");
}

$("#usersBody").addEventListener("click", (e) => {
  const b = e.target.closest("[data-act]");
  if (!b) return;
  const u = data.users.find((x) => x.id === Number(b.dataset.id));
  if (b.dataset.act === "user-pw") {
    const pw = prompt(`Password baru untuk ${u.username} (min. 8 karakter):`);
    if (pw) run(() => api(`/api/admin/users/${u.id}`, "PATCH", { password: pw }), `Password ${u.username} diganti.`);
  } else if (b.dataset.act === "user-role") {
    run(async () => { await api(`/api/admin/users/${u.id}`, "PATCH", { isAdmin: !u.isAdmin }); await loadAdmin(); }, "Peran diubah.");
  } else if (b.dataset.act === "user-del" && confirm(`Hapus user ${u.username}?`)) {
    run(async () => { await api(`/api/admin/users/${u.id}`, "DELETE"); await loadAdmin(); }, "User dihapus.");
  }
});

$("#userForm").addEventListener("submit", (e) => {
  e.preventDefault();
  run(async () => {
    await api("/api/admin/users", "POST", {
      username: $("#newUsername").value.trim(),
      password: $("#newPassword").value,
      isAdmin: $("#newIsAdmin").checked,
    });
    e.target.reset();
    await loadAdmin();
  }, "User dibuat. Atur aksesnya di bagian Akses.");
});

/* ---------- admin: mailboxes ---------- */

function renderMailboxes() {
  $("#mbBody").innerHTML = data.mailboxes.length ? data.mailboxes.map((m) => `
    <tr>
      <td>${esc(m.address)}</td>
      <td><input class="settings-input" data-name-for="${m.id}" value="${esc(m.displayName)}" aria-label="Nama pengirim ${esc(m.address)}"></td>
      <td><div class="row-btns">
        <button class="text-btn" data-act="mb-save" data-id="${m.id}">Simpan</button>
        <button class="text-btn danger" data-act="mb-del" data-id="${m.id}">Hapus</button>
      </div></td>
    </tr>`).join("")
    : `<tr><td colspan="3" class="muted">Belum ada mailbox. Tambahkan alamat pertama di bawah.</td></tr>`;
}

$("#mbBody").addEventListener("click", (e) => {
  const b = e.target.closest("[data-act]");
  if (!b) return;
  const m = data.mailboxes.find((x) => x.id === Number(b.dataset.id));
  if (b.dataset.act === "mb-save") {
    const displayName = document.querySelector(`[data-name-for="${m.id}"]`).value;
    run(() => api(`/api/admin/mailboxes/${m.id}`, "PATCH", { displayName }), "Nama pengirim disimpan.");
  } else if (b.dataset.act === "mb-del" && confirm(`Hapus ${m.address} beserta SEMUA emailnya? Tidak bisa dibatalkan.`)) {
    run(async () => { await api(`/api/admin/mailboxes/${m.id}`, "DELETE"); await loadAdmin(); }, "Mailbox dihapus.");
  }
});

$("#mbForm").addEventListener("submit", (e) => {
  e.preventDefault();
  run(async () => {
    await api("/api/admin/mailboxes", "POST", { address: $("#newAddress").value, displayName: $("#newDisplay").value });
    e.target.reset();
    await loadAdmin();
  }, "Mailbox dibuat.");
});

/* ---------- admin: access matrix ---------- */

function renderAccess() {
  const users = data.users.filter((u) => !u.isAdmin);
  const table = $("#accessTable");
  if (!users.length || !data.mailboxes.length) {
    table.innerHTML = `<tbody><tr><td class="muted">${!users.length
      ? "Belum ada user non-admin. Admin otomatis punya akses ke semua mailbox."
      : "Belum ada mailbox."}</td></tr></tbody>`;
    return;
  }
  const access = (u, m) => {
    const g = data.grants.find((x) => x.userId === u.id && x.mailboxId === m.id);
    return !g ? "none" : g.canSend ? "send" : "receive";
  };
  table.innerHTML = `
    <thead><tr><th>User</th>${data.mailboxes.map((m) => `<th>${esc(m.address)}</th>`).join("")}</tr></thead>
    <tbody>${users.map((u) => `<tr><td>${esc(u.username)}</td>${data.mailboxes.map((m) => {
      const a = access(u, m);
      return `<td><select class="settings-input" data-user="${u.id}" data-mailbox="${m.id}" aria-label="Akses ${esc(u.username)} ke ${esc(m.address)}">
        <option value="none" ${a === "none" ? "selected" : ""}>—</option>
        <option value="receive" ${a === "receive" ? "selected" : ""}>Terima saja</option>
        <option value="send" ${a === "send" ? "selected" : ""}>Kirim &amp; terima</option>
      </select></td>`;
    }).join("")}</tr>`).join("")}</tbody>`;
}

$("#accessTable").addEventListener("change", (e) => {
  const s = e.target.closest("select[data-user]");
  if (!s) return;
  run(async () => {
    await api("/api/admin/grants", "PUT", { userId: Number(s.dataset.user), mailboxId: Number(s.dataset.mailbox), access: s.value });
    data = await api("/api/admin/overview");
  }, "Akses disimpan.");
});

/* ---------- admin: providers & Cloudflare info ---------- */

const PROVIDER_KEYS = ["active_provider", "resend_api_key", "resend_daily_limit", "resend_monthly_limit", "aws_region", "aws_access_key_id", "aws_secret_access_key"];

async function loadSettings() {
  const s = await api("/api/admin/settings");
  for (const k of PROVIDER_KEYS) $("#" + k).value = s[k] || (k === "active_provider" ? "auto" : "");
  const info = [
    ["Domain", s.domain], ["URL publik", s.public_url], ["Account", s.cf_account_id],
    ["Zone", s.cf_zone_id], ["KV cadangan", s.cf_kv_namespace_id], ["Tunnel", s.cf_tunnel_id],
  ];
  $("#cfInfo").innerHTML = info.map(([k, v]) => `<dt>${k}</dt><dd>${v ? esc(v) : '<span class="muted">belum diatur</span>'}</dd>`).join("");
}

$("#providerForm").addEventListener("submit", (e) => {
  e.preventDefault();
  const body = Object.fromEntries(PROVIDER_KEYS.map((k) => [k, $("#" + k).value]));
  run(async () => { await api("/api/admin/settings", "POST", body); await loadSettings(); }, "Pengaturan pengiriman disimpan.");
});

/* ---------- boot ---------- */

async function loadAdmin() {
  data = await api("/api/admin/overview");
  renderUsers();
  renderMailboxes();
  renderAccess();
}

async function boot() {
  me = await api("/api/me");
  $("#whoami").textContent = `Masuk sebagai ${me.user.username}${me.user.isAdmin ? " (admin)" : ""}.`;
  if (!me.user.isAdmin) return;
  for (const id of ["#secUsers", "#secMailboxes", "#secAccess", "#secProvider", "#secCloudflare"]) $(id).hidden = false;
  await Promise.all([loadAdmin(), loadSettings()]);
}

boot().catch((err) => toast(err.message));
