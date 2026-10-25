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
let domains = []; // mail domains, primary first; bare names typed in address fields get "@" + one of them
const fullAddress = (s, dom = domains[0]) => (s.includes("@") || !dom ? s : `${s}@${dom}`);
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

$("#sigForm").addEventListener("submit", (e) => {
  e.preventDefault();
  run(() => api("/api/me/signature", "PUT", { signature: $("#mySignature").value }), "Tanda tangan disimpan.");
});

window.wmailTheme.bind($("#themeSelect"));

$("#logoutBtn").addEventListener("click", async () => {
  await fetch("/api/auth/logout", { method: "POST" });
  location.href = "/login.html";
});

/* ---------- admin: users and their access ---------- */

const lc = (s) => String(s || "").toLowerCase();

// filterList hides the items of a long list that don't contain what was typed.
function filterList(input, items) {
  const q = lc(input.value.trim());
  for (const it of items) it.hidden = !!q && !it.dataset.filter.includes(q);
}

// accessHtml lists only the mailboxes u holds, and new ones are picked by typing the
// address, so it stays short however many mailboxes there are.
function accessHtml(u, held) {
  if (!data.mailboxes.length) return `<p class="muted">Belum ada mailbox.</p>`;
  return `${held.length ? `<div class="table-wrap"><table class="data-table"><tbody>${held.map((g) => `<tr>
      <td>${esc(g.address)}</td>
      <td><select class="settings-input" data-user="${u.id}" data-mailbox="${g.mailboxId}" aria-label="Akses ${esc(u.username)} ke ${esc(g.address)}">
        <option value="receive" ${g.canSend ? "" : "selected"}>Terima saja</option>
        <option value="send" ${g.canSend ? "selected" : ""}>Kirim &amp; terima</option>
      </select></td>
      <td><div class="row-btns"><button class="text-btn danger" data-revoke data-user="${u.id}" data-mailbox="${g.mailboxId}">Cabut</button></div></td>
    </tr>`).join("")}</tbody></table></div>` : `<p class="muted">Belum punya akses ke mailbox mana pun.</p>`}
    <form class="inline-form" data-grant="${u.id}">
      <div class="settings-field"><label for="grantAddr${u.id}">Tambah mailbox</label>
        <input class="settings-input" id="grantAddr${u.id}" name="address" list="mbOptions" placeholder="Ketik alamat…" autocomplete="off" required></div>
      <div class="settings-field"><label for="grantAccess${u.id}">Hak</label>
        <select class="settings-input" id="grantAccess${u.id}" name="access">
          <option value="receive">Terima saja</option>
          <option value="send">Kirim &amp; terima</option>
        </select></div>
      <button class="send-btn" type="submit">Beri akses</button>
    </form>`;
}

// One folded line per user; opening it shows their access and account actions.
function renderUsers() {
  const el = $("#usersList");
  const open = new Set([...el.querySelectorAll("details[open]")].map((d) => d.dataset.user));
  const byId = new Map(data.mailboxes.map((m) => [m.id, m]));
  $("#mbOptions").innerHTML = data.mailboxes.map((m) => `<option value="${esc(m.address)}">`).join("");
  el.innerHTML = data.users.map((u) => {
    const held = data.grants.filter((g) => g.userId === u.id && byId.has(g.mailboxId))
      .map((g) => ({ ...g, address: byId.get(g.mailboxId).address }))
      .sort((a, b) => a.address.localeCompare(b.address));
    const self = u.id === me.user.id;
    return `<details class="user-item" data-user="${u.id}" data-filter="${esc(lc(u.username))}" ${open.has(String(u.id)) ? "open" : ""}>
      <summary><strong>${esc(u.username)}</strong>${self ? ' <span class="muted">(Anda)</span>' : ""}
        <span class="muted">· ${u.isAdmin ? "Admin, semua mailbox" : `User, ${held.length} mailbox`}</span></summary>
      <div class="user-body">
        ${u.isAdmin ? `<p class="muted">Admin bisa membuka semua mailbox.</p>` : accessHtml(u, held)}
        <div class="row-btns">
          <button class="text-btn" data-act="user-pw" data-id="${u.id}">Reset password</button>
          ${self ? "" : `
            <button class="text-btn" data-act="user-role" data-id="${u.id}">${u.isAdmin ? "Jadikan user" : "Jadikan admin"}</button>
            <button class="text-btn danger" data-act="user-del" data-id="${u.id}">Hapus user</button>`}
        </div>
      </div>
    </details>`;
  }).join("");
  filterList($("#userFilter"), el.children);
}

$("#userFilter").addEventListener("input", () => filterList($("#userFilter"), $("#usersList").children));

function setGrant(userId, mailboxId, access, okMsg) {
  return run(async () => {
    await api("/api/admin/grants", "PUT", { userId: Number(userId), mailboxId: Number(mailboxId), access });
    data = await api("/api/admin/overview");
    renderUsers();
  }, okMsg);
}

$("#usersList").addEventListener("change", (e) => {
  const s = e.target.closest("select[data-mailbox]");
  if (s) setGrant(s.dataset.user, s.dataset.mailbox, s.value, "Akses disimpan.");
});

$("#usersList").addEventListener("submit", (e) => {
  const f = e.target.closest("form[data-grant]");
  if (!f) return;
  e.preventDefault();
  const address = fullAddress(lc(f.address.value.trim()));
  const m = data.mailboxes.find((x) => lc(x.address) === address);
  if (!m) return toast("Mailbox tidak ditemukan. Pilih dari daftar alamat.");
  setGrant(f.dataset.grant, m.id, f.access.value, "Akses diberikan.");
});

$("#usersList").addEventListener("click", (e) => {
  const r = e.target.closest("[data-revoke]");
  if (r) return setGrant(r.dataset.user, r.dataset.mailbox, "none", "Akses dicabut.");
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

// One folded line per mailbox; opening it edits the sender name.
function renderMailboxes() {
  const el = $("#mbList");
  const open = new Set([...el.querySelectorAll("details[open]")].map((d) => d.dataset.mb));
  el.innerHTML = data.mailboxes.length ? data.mailboxes.map((m) => `
    <details class="user-item" data-mb="${m.id}" data-filter="${esc(lc(m.address + " " + m.displayName))}" ${open.has(String(m.id)) ? "open" : ""}>
      <summary><strong>${esc(m.address)}</strong>${m.displayName ? ` <span class="muted">· ${esc(m.displayName)}</span>` : ""}</summary>
      <div class="user-body">
        <div class="settings-field"><label for="mbName${m.id}">Nama pengirim</label>
          <input class="settings-input" id="mbName${m.id}" value="${esc(m.displayName)}" placeholder="Nama yang tampil di penerima"></div>
        <div class="row-btns">
          <button class="text-btn" data-act="mb-save" data-id="${m.id}">Simpan</button>
          <button class="text-btn danger" data-act="mb-del" data-id="${m.id}">Hapus mailbox</button>
        </div>
      </div>
    </details>`).join("")
    : `<p class="muted" data-filter="">Belum ada mailbox. Tambahkan alamat pertama di bawah.</p>`;
  filterList($("#mbFilter"), el.children);
}

$("#mbFilter").addEventListener("input", () => filterList($("#mbFilter"), $("#mbList").children));

$("#mbList").addEventListener("click", (e) => {
  const b = e.target.closest("[data-act]");
  if (!b) return;
  const m = data.mailboxes.find((x) => x.id === Number(b.dataset.id));
  if (b.dataset.act === "mb-save") {
    const body = { displayName: $(`#mbName${m.id}`).value };
    run(async () => { await api(`/api/admin/mailboxes/${m.id}`, "PATCH", body); await loadAdmin(); }, "Nama pengirim disimpan.");
  } else if (b.dataset.act === "mb-del" && confirm(`Hapus ${m.address} beserta SEMUA emailnya? Tidak bisa dibatalkan.`)) {
    run(async () => { await api(`/api/admin/mailboxes/${m.id}`, "DELETE"); await loadAdmin(); }, "Mailbox dihapus.");
  }
});

// The "@domain" picker shows while a bare name is typed; a full address hides it.
function syncDomainSuffix() {
  $("#newDomain").hidden = !domains.length || $("#newAddress").value.includes("@");
}
$("#newAddress").addEventListener("input", syncDomainSuffix);

$("#mbForm").addEventListener("submit", (e) => {
  e.preventDefault();
  run(async () => {
    const address = fullAddress($("#newAddress").value.trim(), $("#newDomain").value);
    await api("/api/admin/mailboxes", "POST", { address, displayName: $("#newDisplay").value });
    e.target.reset();
    syncDomainSuffix();
    await loadAdmin();
  }, "Mailbox dibuat.");
});

/* ---------- admin: providers & Cloudflare info ---------- */

const PROVIDER_KEYS = ["active_provider", "resend_api_key", "resend_daily_limit", "resend_monthly_limit", "aws_region", "aws_access_key_id", "aws_secret_access_key"];

async function loadSettings() {
  const s = await api("/api/admin/settings");
  domains = lc(s.domain).split(/[\s,]+/).filter(Boolean);
  $("#newDomain").innerHTML = domains.map((d) => `<option value="${esc(d)}">@${esc(d)}</option>`).join("");
  $("#newAddress").placeholder = domains.length ? "nama" : "nama@domain-anda.com";
  syncDomainSuffix();
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
}

async function boot() {
  me = await api("/api/me");
  $("#whoami").textContent = `Masuk sebagai ${me.user.username}${me.user.isAdmin ? " (admin)" : ""}.`;
  $("#mySignature").value = me.user.signature || "";
  if (!me.user.isAdmin) return;
  for (const id of ["#secUsers", "#secMailboxes", "#secProvider", "#secCloudflare"]) $(id).hidden = false;
  await Promise.all([loadAdmin(), loadSettings()]);
}

boot().catch((err) => toast(err.message));
