/* Wmail web client */

const AV_COLORS = [
  "#0B57D0", "#7B1FA2", "#1E8E3E", "#C5221F", "#F9AB00",
  "#00838F", "#5F6368", "#A142F4", "#3367D6", "#E37400"
];

let me = { user: null, mailboxes: [] };
let mailboxId = null;
let mails = [];
let totalMails = 0;
let currentMailDetail = null;
let stats = { inboxUnread: 0, starredCount: 0, sentCount: 0, draftsCount: 0, trashCount: 0 };

let view = "inbox";
let query = "";
let openId = null;
let selected = new Set();
let moreOpen = false;
let pendingAttachments = [];

const $ = (s) => document.querySelector(s);
const rowsEl = $("#mailrows");
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) =>
  ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const avColor = (id) => AV_COLORS[Math.abs(Number(id || 0)) % AV_COLORS.length];
const initial = (name, email) => (name || email || "?").trim().charAt(0).toUpperCase();
const currentMailbox = () => me.mailboxes.find((m) => m.id === mailboxId);
const sendable = () => me.mailboxes.filter((m) => m.canSend);
// Receive-only ("terima saja") grants are read-only: the server refuses moves and deletes.
const canManage = () => !!currentMailbox()?.canSend;

function formatSize(bytes) {
  if (!bytes || bytes <= 0) return "0 B";
  const k = 1024;
  const sizes = ["B", "KB", "MB", "GB"];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + " " + sizes[i];
}

function formatDate(isoStr) {
  if (!isoStr) return "";
  const d = new Date(isoStr);
  if (isNaN(d.getTime())) return isoStr;
  const now = new Date();
  if (d.toDateString() === now.toDateString()) {
    return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  }
  return d.toLocaleDateString([], { month: "short", day: "numeric" });
}

function formatFullDate(isoStr) {
  if (!isoStr) return "";
  const d = new Date(isoStr);
  if (isNaN(d.getTime())) return isoStr;
  return d.toLocaleString([], {
    weekday: "short", month: "short", day: "numeric",
    year: "numeric", hour: "2-digit", minute: "2-digit"
  });
}

const VIEWS = [
  { key: "inbox", label: "Kotak masuk", icon: "inbox", fill: true, cls: "icon-blue" },
  { key: "starred", label: "Berbintang", icon: "star", fill: true },
  { key: "sent", label: "Terkirim", icon: "send", fill: true },
  { key: "trash", label: "Sampah", icon: "delete" },
];

const MORE_VIEWS = [
  { key: "snoozed", label: "Ditunda", icon: "schedule", fill: true },
  { key: "spam", label: "Spam", icon: "report" },
  { key: "all", label: "Semua email", icon: "label" },
];

/* ---------- API Calls ---------- */

// api wraps fetch; an expired session sends the user back to the login page.
async function api(path, opts) {
  const res = await fetch(path, opts);
  if (res.status === 401) {
    location.href = "/login.html";
    throw new Error("unauthorized");
  }
  return res;
}

const mbq = () => `mailbox=${mailboxId}`;

async function apiGetMe() {
  const res = await api("/api/me");
  me = await res.json();
  const name = me.user.username;
  $("#accountBtn").textContent = initial(name);
  $("#accountBtn").setAttribute("aria-label", `Akun: ${name}`);
  $("#accountName").textContent = name + (me.user.isAdmin ? " · admin" : "");

  const saved = Number(localStorage.getItem("wmail.mailbox"));
  mailboxId = (me.mailboxes.find((m) => m.id === saved) || me.mailboxes[0] || {}).id ?? null;
  renderMailboxPicker();
}

async function apiGetStats() {
  if (!mailboxId) return;
  try {
    const res = await api(`/api/stats?${mbq()}`);
    if (res.ok) {
      stats = await res.json();
      renderNav();
    }
  } catch (err) {
    console.warn("apiGetStats error:", err);
  }
}

async function apiGetEmails() {
  if (!mailboxId) {
    rowsEl.innerHTML = `<div class="empty-state">${me.user?.isAdmin
      ? 'Belum ada mailbox. Buat alamat email pertama di <a href="/settings.html">Pengaturan</a>.'
      : "Akun Anda belum diberi akses ke mailbox mana pun. Hubungi admin."}</div>`;
    return;
  }
  try {
    const url = `/api/emails?${mbq()}&folder=${encodeURIComponent(view)}&q=${encodeURIComponent(query)}`;
    const res = await api(url);
    if (!res.ok) throw new Error(await res.text());
    const data = await res.json();
    mails = data.emails || [];
    totalMails = data.total || 0;
    renderRows();
  } catch (err) {
    console.error("apiGetEmails error:", err);
    rowsEl.innerHTML = `<div class="empty-state">Gagal memuat email. Periksa koneksi ke server lalu tekan muat ulang.</div>`;
  }
}

async function apiGetEmailDetail(id) {
  try {
    const res = await api(`/api/emails/${id}`);
    if (!res.ok) throw new Error("Gagal mengambil detail email");
    currentMailDetail = await res.json();
    const m = mails.find((x) => x.id === id);
    if (m) m.isUnread = false;
    renderReader();
    apiGetStats();
  } catch (err) {
    console.error("apiGetEmailDetail error:", err);
  }
}

async function apiUpdateEmail(id, patch) {
  try {
    const res = await api(`/api/emails/${id}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(patch),
    });
    if (res.ok) apiGetStats();
  } catch (err) {
    console.error("apiUpdateEmail error:", err);
  }
}

async function apiDeleteEmail(id) {
  try {
    await api(`/api/emails/${id}`, { method: "DELETE" });
    apiGetStats();
  } catch (err) {
    console.error("apiDeleteEmail error:", err);
  }
}

async function apiEmptyTrash() {
  if (!confirm("Hapus permanen semua email di Sampah?")) return;
  await api(`/api/emails/empty-trash?${mbq()}`, { method: "POST" });
  showSnackbar("Sampah dikosongkan.");
  openId = null;
  apiGetEmails();
  apiGetStats();
}

/* ---------- Rendering UI ---------- */

function renderMailboxPicker() {
  const picker = $("#mailboxPicker");
  const sel = $("#mailboxSelect");
  picker.hidden = me.mailboxes.length === 0;
  sel.innerHTML = me.mailboxes.map((m) =>
    `<option value="${m.id}" ${m.id === mailboxId ? "selected" : ""}>${esc(m.address)}${m.canSend ? "" : " (terima saja)"}</option>`
  ).join("");
  const composeBtn = $("#composeBtn");
  composeBtn.disabled = sendable().length === 0;
  composeBtn.title = composeBtn.disabled ? "Tidak ada mailbox yang diizinkan mengirim" : "";
}

function switchMailbox(id) {
  mailboxId = id;
  try { localStorage.setItem("wmail.mailbox", String(id)); } catch {}
  view = "inbox";
  openId = null;
  currentMailDetail = null;
  selected.clear();
  renderReader();
  apiGetStats();
  apiGetEmails();
  renderNav();
}

function renderNav() {
  const el = $("#navlist");
  if (!el) return;
  const items = [...VIEWS];
  if (moreOpen) items.push(...MORE_VIEWS);

  let html = items.map((v) => {
    let count = 0;
    if (v.key === "inbox") count = stats.inboxUnread;
    else if (v.key === "trash") count = stats.trashCount;

    return `<button class="nav-item ${v.cls || ""} ${view === v.key ? "is-active" : ""}" data-view="${v.key}" ${view === v.key ? 'aria-current="page"' : ""}>
      <span class="${v.fill ? "msf" : "ms"}">${v.icon}</span>
      <span class="nav-label">${v.label}</span>
      ${count > 0 ? `<span class="nav-count">${count}</span>` : ""}
    </button>`;
  }).join("");

  html += `<button class="nav-item" id="moreBtn" aria-expanded="${moreOpen}">
      <span class="ms" style="${moreOpen ? "transform:rotate(180deg)" : ""}">expand_more</span>
      <span class="nav-label">${moreOpen ? "Lebih sedikit" : "Lainnya"}</span>
    </button>`;

  if (view === "trash" && stats.trashCount > 0 && canManage()) {
    html += `<button class="nav-item" id="emptyTrashBtn"><span class="ms">delete</span><span class="nav-label">Kosongkan sampah</span></button>`;
  }

  el.innerHTML = html;

  el.querySelectorAll("[data-view]").forEach((b) =>
    b.addEventListener("click", () => {
      view = b.dataset.view;
      openId = null;
      currentMailDetail = null;
      selected.clear();
      syncSelAll();
      renderReader();
      apiGetEmails();
      renderNav();
      document.body.classList.remove("panel-open");
    })
  );

  $("#moreBtn")?.addEventListener("click", () => { moreOpen = !moreOpen; renderNav(); });
  $("#emptyTrashBtn")?.addEventListener("click", apiEmptyTrash);
}

function rowHtml(m) {
  const isSent = m.folder === "sent";
  const fromLabel = isSent ? `Kepada: ${(m.toRecipients || []).join(", ") || "(tidak diketahui)"}` : (m.fromName || m.fromEmail);
  const sel = selected.has(m.id);

  return `<div class="mailrow ${m.isUnread ? "is-unread" : ""} ${sel ? "is-selected" : ""}" role="listitem"
      id="row-${m.id}" data-mail="${m.id}" tabindex="0">
    <button class="iconbtn row-check ${sel ? "is-checked" : ""}" aria-label="Pilih" aria-pressed="${sel}" data-act="select">
      <span class="ms">${sel ? "check_box" : "check_box_outline_blank"}</span>
    </button>
    <button class="iconbtn row-star star-btn ${m.isStarred ? "is-starred" : ""}" aria-label="${m.isStarred ? "Hapus bintang" : "Beri bintang"}" data-act="star">
      <span class="${m.isStarred ? "msf" : "ms"}">star</span>
    </button>
    <span class="row-sender">${esc(fromLabel)}</span>
    <span class="row-main">
      <span class="row-subject">${m.subject ? esc(m.subject) : '<span style="opacity:.55">(tanpa subjek)</span>'}</span>
      <span class="row-snippet">— ${esc(m.snippet || "")}</span>
    </span>
    ${m.hasAttachments ? `<span class="row-attach"><span class="ms">attach_file</span></span>` : ""}
    <span class="row-right">
      <span class="row-date">${esc(formatDate(m.createdAt))}</span>
      <span class="row-actions">
        ${canManage() ? `<button class="iconbtn" aria-label="Arsipkan" data-act="archive"><span class="ms">archive</span></button>
        <button class="iconbtn" aria-label="Hapus" data-act="delete"><span class="ms">delete</span></button>` : ""}
        <button class="iconbtn" aria-label="Tandai belum dibaca" data-act="unread"><span class="ms">mark_email_unread</span></button>
        ${canManage() ? `<button class="iconbtn" aria-label="Tunda" data-act="snooze"><span class="ms">schedule</span></button>` : ""}
      </span>
    </span>
  </div>`;
}

function renderRows() {
  if (openId !== null) {
    rowsEl.hidden = true;
    return;
  }
  rowsEl.hidden = false;

  if (!mails.length) {
    const label = [...VIEWS, ...MORE_VIEWS].find((v) => v.key === view)?.label || view;
    rowsEl.innerHTML = `<div class="empty-state">${query ? `Tidak ada email yang cocok dengan "${esc(query)}".` : `Tidak ada email di ${esc(label)}.`}</div>`;
  } else {
    rowsEl.innerHTML = mails.map(rowHtml).join("");
  }

  const mb = currentMailbox();
  document.title = `${stats.inboxUnread ? `(${stats.inboxUnread}) ` : ""}${mb ? mb.address : "Wmail"} - Wmail`;
  $("#pageCount").textContent = totalMails > 0 ? `1–${mails.length} dari ${totalMails}` : "";
}

// Email HTML goes into a sandboxed iframe: scripts, forms and plugins are blocked,
// links open in a new tab. allow-same-origin (without allow-scripts) only lets us size it.
function emailFrame(html) {
  const frame = document.createElement("iframe");
  frame.className = "reader-frame";
  frame.setAttribute("sandbox", "allow-same-origin allow-popups allow-popups-to-escape-sandbox");
  frame.setAttribute("title", "Isi email");
  frame.srcdoc = `<!doctype html><html><head><meta charset="utf-8"><base target="_blank">
    <style>body{margin:0;font:14px/1.6 Roboto,Arial,sans-serif;color:#1f1f1f;overflow-wrap:anywhere}img{max-width:100%;height:auto}</style>
    </head><body>${html}</body></html>`;
  frame.addEventListener("load", () => {
    const doc = frame.contentDocument;
    if (!doc) return;
    const fit = () => { frame.style.height = doc.documentElement.scrollHeight + "px"; };
    fit();
    new ResizeObserver(fit).observe(doc.body);
  });
  return frame;
}

function renderReader() {
  const r = $("#reader");
  if (!r) return;
  if (!openId || !currentMailDetail) {
    r.hidden = true;
    rowsEl.hidden = false;
    return;
  }

  r.hidden = false;
  rowsEl.hidden = true;
  for (const id of ["#btnRArchive", "#btnRSpam", "#btnRDelete", "#btnRSnooze"]) $(id).hidden = !canManage();

  const m = currentMailDetail;
  const pos = Math.max(1, mails.findIndex((x) => x.id === m.id) + 1);
  $("#readerPos").textContent = `${pos} dari ${mails.length || 1}`;

  let attachmentsHtml = "";
  if (m.attachments && m.attachments.length > 0) {
    attachmentsHtml = `
      <div class="reader-attachments-section">
        <div class="reader-attachments-title">
          <span class="ms">attach_file</span>
          <span>${m.attachments.length} lampiran</span>
        </div>
        <div class="reader-attachments-grid">
          ${m.attachments.map((att) => `
            <a href="/api/attachments/${encodeURIComponent(att.id)}" target="_blank" rel="noopener" class="reader-att-card" title="Unduh ${esc(att.filename)}">
              <span class="ms">attach_file</span>
              <div class="reader-att-info">
                <span class="reader-att-name">${esc(att.filename)}</span>
                <span class="reader-att-size">${formatSize(att.sizeBytes)}</span>
              </div>
            </a>
          `).join("")}
        </div>
      </div>`;
  }

  const hasHtml = m.bodyHtml && m.bodyHtml.trim();
  const textHtml = (m.bodyText || "").split(/\r?\n\r?\n/).map((p) => `<p>${esc(p).replace(/\r?\n/g, "<br>")}</p>`).join("");
  const canReply = currentMailbox()?.canSend;

  $("#readerBody").innerHTML = `
    <h1 class="reader-subject">${m.subject ? esc(m.subject) : "(tanpa subjek)"}</h1>
    <div class="reader-msg-head">
      <span class="avatar" style="background:${avColor(m.id)}" aria-hidden="true">${esc(initial(m.fromName, m.fromEmail))}</span>
      <div class="reader-who">
        <div class="reader-from">${esc(m.fromName || m.fromEmail)} <span class="addr">&lt;${esc(m.fromEmail || "unknown")}&gt;</span></div>
        <div class="reader-meta">kepada ${esc((m.toRecipients || []).join(", "))} — ${esc(formatFullDate(m.createdAt))}</div>
      </div>
      <span class="reader-msg-actions">
        <button class="iconbtn" aria-label="${m.isStarred ? "Hapus bintang" : "Beri bintang"}" id="readerStarBtn">
          <span class="${m.isStarred ? "msf" : "ms"}">star</span>
        </button>
        ${canReply ? `<button class="iconbtn" aria-label="Balas" id="readerReplyBtn"><span class="ms">reply</span></button>
        <button class="iconbtn" aria-label="Teruskan" id="readerForwardBtn"><span class="ms">forward</span></button>` : ""}
      </span>
    </div>
    <div class="reader-content" id="readerContent">${hasHtml ? "" : textHtml}</div>
    ${attachmentsHtml ? `<div class="reader-content">${attachmentsHtml}</div>` : ""}
    ${canReply ? `<div class="reader-quick">
      <button class="quick-btn" id="btnQuickReply"><span class="ms">reply</span>Balas</button>
      <button class="quick-btn" id="btnQuickForward"><span class="ms">forward</span>Teruskan</button>
    </div>` : `<p class="reader-note">Mailbox ini hanya bisa dibaca (hak terima saja).</p>`}`;

  if (hasHtml) $("#readerContent").appendChild(emailFrame(m.bodyHtml));

  $("#readerStarBtn").addEventListener("click", async () => {
    m.isStarred = !m.isStarred;
    await apiUpdateEmail(m.id, { isStarred: m.isStarred });
    renderReader();
    apiGetEmails();
  });
  for (const id of ["#readerReplyBtn", "#btnQuickReply"]) $(id)?.addEventListener("click", () => replyEmail(m));
  for (const id of ["#readerForwardBtn", "#btnQuickForward"]) $(id)?.addEventListener("click", () => forwardEmail(m));
}

/* ---------- Reply & Forward ---------- */

function quoteHeader(m) {
  const author = m.fromName ? `${m.fromName} <${m.fromEmail}>` : m.fromEmail;
  return { date: formatFullDate(m.createdAt), author };
}

function replyEmail(m) {
  openCompose();
  $("#composeTo").value = m.folder === "sent" ? (m.toRecipients || []).join(", ") : (m.fromEmail || "");
  const sub = m.subject || "";
  $("#composeSubject").value = /^re:/i.test(sub) ? sub : `Re: ${sub}`;
  $("#composeInReplyTo").value = m.messageId || "";
  const { date, author } = quoteHeader(m);
  const quoteBody = (m.bodyText || "").split("\n").map((l) => `> ${l}`).join("\n");
  $("#composeBody").value = `\n\nPada ${date}, ${author} menulis:\n${quoteBody}`;
  $("#composeBody").focus();
  $("#composeBody").setSelectionRange(0, 0);
}

function forwardEmail(m) {
  openCompose();
  $("#composeTo").value = "";
  const sub = m.subject || "";
  $("#composeSubject").value = /^fwd:/i.test(sub) ? sub : `Fwd: ${sub}`;
  $("#composeInReplyTo").value = "";
  const { date, author } = quoteHeader(m);
  $("#composeBody").value = `\n\n---------- Pesan yang diteruskan ---------\nDari: ${author}\nTanggal: ${date}\nSubjek: ${m.subject}\nKepada: ${(m.toRecipients || []).join(", ")}\n\n${m.bodyText || ""}`;
  $("#composeTo").focus();
}

/* ---------- Snackbar ---------- */

function showSnackbar(text) {
  const sn = $("#snackbar");
  $("#snackText").textContent = text;
  sn.hidden = false;
  clearTimeout(showSnackbar._t);
  showSnackbar._t = setTimeout(() => { sn.hidden = true; }, 4000);
}

/* ---------- Row Click & Actions ---------- */

async function moveOut(id, patch, message) {
  await apiUpdateEmail(id, patch);
  showSnackbar(message);
  if (openId === id) {
    openId = null;
    currentMailDetail = null;
    renderReader();
  }
  apiGetEmails();
}

const ACTIONS = {
  archive: (id) => moveOut(id, { isArchived: true }, "Email diarsipkan."),
  delete: (id) => moveOut(id, { isDeleted: true }, "Email dipindahkan ke Sampah."),
  snooze: (id) => moveOut(id, { folder: "snoozed" }, "Email ditunda."),
  spam: (id) => moveOut(id, { folder: "spam" }, "Email ditandai sebagai spam."),
  unread: (id) => moveOut(id, { isUnread: true }, "Ditandai belum dibaca."),
};

rowsEl.addEventListener("click", async (e) => {
  const actBtn = e.target.closest("[data-act]");
  const row = e.target.closest(".mailrow");
  if (!row) return;
  const id = Number(row.dataset.mail);
  const m = mails.find((x) => x.id === id);
  if (!m) return;

  if (actBtn) {
    const act = actBtn.dataset.act;
    if (act === "star") {
      m.isStarred = !m.isStarred;
      await apiUpdateEmail(id, { isStarred: m.isStarred });
      renderRows();
    } else if (act === "select") {
      selected.has(id) ? selected.delete(id) : selected.add(id);
      syncSelAll();
      renderRows();
    } else {
      ACTIONS[act]?.(id);
    }
    return;
  }

  openId = id;
  await apiGetEmailDetail(id);
  renderRows();
});

rowsEl.addEventListener("keydown", (e) => {
  if (e.key === "Enter" && e.target.classList.contains("mailrow")) e.target.click();
});

function syncSelAll() {
  const all = mails.length && mails.every((m) => selected.has(m.id));
  const some = mails.some((m) => selected.has(m.id));
  $("#selAllIcon").textContent = all ? "check_box" : (some ? "indeterminate_check_box" : "check_box_outline_blank");
}

$(".sel-all").addEventListener("click", () => {
  const all = mails.length && mails.every((m) => selected.has(m.id));
  mails.forEach((m) => (all ? selected.delete(m.id) : selected.add(m.id)));
  syncSelAll();
  renderRows();
});

$("#readerBack").addEventListener("click", () => {
  openId = null;
  currentMailDetail = null;
  renderReader();
  apiGetEmails();
});

$("#btnRArchive").onclick = () => openId && ACTIONS.archive(openId);
$("#btnRSpam").onclick = () => openId && ACTIONS.spam(openId);
$("#btnRSnooze").onclick = () => openId && ACTIONS.snooze(openId);
$("#btnRUnread").onclick = () => openId && ACTIONS.unread(openId);
$("#btnRDelete").onclick = async () => {
  if (!openId) return;
  if (view === "trash") {
    if (!confirm("Hapus permanen email ini?")) return;
    await apiDeleteEmail(openId);
    showSnackbar("Email dihapus permanen.");
    openId = null;
    renderReader();
    apiGetEmails();
  } else {
    ACTIONS.delete(openId);
  }
};

let searchTimer = null;
$("#searchInput").addEventListener("input", (e) => {
  query = e.target.value.trim();
  clearTimeout(searchTimer);
  searchTimer = setTimeout(() => {
    openId = null;
    renderReader();
    apiGetEmails();
  }, 300);
});

$("#btn-refresh").addEventListener("click", () => {
  apiGetStats();
  apiGetEmails();
});

$("#mailboxSelect").addEventListener("change", (e) => switchMailbox(Number(e.target.value)));

$("#menuBtn").addEventListener("click", () => {
  // Wide screens collapse the panel; narrow screens open it as an overlay.
  const narrow = matchMedia("(max-width: 768px)").matches;
  document.body.classList.toggle(narrow ? "panel-open" : "panel-hidden");
});

$("#logoutBtn").addEventListener("click", async () => {
  await fetch("/api/auth/logout", { method: "POST" });
  location.href = "/login.html";
});

/* ---------- Compose & Attachments ---------- */

function openCompose() {
  const list = sendable();
  if (!list.length) return;
  const cur = currentMailbox();
  const from = cur?.canSend ? cur.id : list[0].id;
  $("#composeFrom").innerHTML = list.map((m) =>
    `<option value="${m.id}" ${m.id === from ? "selected" : ""}>${esc(m.displayName ? `${m.displayName} <${m.address}>` : m.address)}</option>`
  ).join("");
  const comp = $("#compose");
  comp.hidden = false;
  comp.classList.remove("is-min");
  $("#composeTo").focus();
}

function resetCompose() {
  $("#composeTo").value = "";
  $("#composeCc").value = "";
  $("#composeCcRow").hidden = true;
  $("#composeSubject").value = "";
  $("#composeBody").value = "";
  $("#composeInReplyTo").value = "";
  pendingAttachments = [];
  renderComposeAttachments();
}

function renderComposeAttachments() {
  const container = $("#composeAttachments");
  if (pendingAttachments.length === 0) {
    container.hidden = true;
    container.innerHTML = "";
    return;
  }
  container.hidden = false;
  container.innerHTML = pendingAttachments.map((f, i) => `
    <span class="compose-att-chip">
      <span class="ms" style="font-size:16px;">attach_file</span>
      <span>${esc(f.name)} (${formatSize(f.size)})</span>
      <button type="button" class="remove-att" data-att-idx="${i}" aria-label="Hapus lampiran ${esc(f.name)}">
        <span class="ms" style="font-size:16px;">close</span>
      </button>
    </span>`).join("");
  container.querySelectorAll(".remove-att").forEach((b) => {
    b.onclick = () => {
      pendingAttachments.splice(Number(b.dataset.attIdx), 1);
      renderComposeAttachments();
    };
  });
}

$("#composeBtn").addEventListener("click", () => { resetCompose(); openCompose(); });
$("#composeClose").addEventListener("click", () => { $("#compose").hidden = true; });
$("#composeMin").addEventListener("click", () => $("#compose").classList.toggle("is-min"));
$("#composeFull").addEventListener("click", () => $("#compose").classList.toggle("is-full"));
$("#btnCcToggle").addEventListener("click", () => {
  const row = $("#composeCcRow");
  row.hidden = !row.hidden;
  if (!row.hidden) $("#composeCc").focus();
});

const fileInput = $("#composeFileInput");
$("#btnAttach").addEventListener("click", () => fileInput.click());
fileInput.addEventListener("change", (e) => {
  for (const f of e.target.files || []) pendingAttachments.push(f);
  renderComposeAttachments();
  fileInput.value = "";
});

const sendBtn = $("#sendBtn");
sendBtn.addEventListener("click", async () => {
  const to = $("#composeTo").value.trim();
  if (!to) {
    showSnackbar("Isi alamat penerima (Kepada).");
    $("#composeTo").focus();
    return;
  }

  const fd = new FormData();
  fd.append("from", $("#composeFrom").value);
  fd.append("to", to);
  fd.append("cc", $("#composeCc").value.trim());
  fd.append("subject", $("#composeSubject").value.trim());
  fd.append("body", $("#composeBody").value.trim());
  fd.append("inReplyTo", $("#composeInReplyTo").value.trim());
  for (const file of pendingAttachments) fd.append("attachments", file);

  sendBtn.disabled = true;
  sendBtn.innerHTML = `<span class="msf">schedule</span><span>Mengirim...</span>`;
  try {
    const res = await api("/api/emails/send", { method: "POST", body: fd });
    if (!res.ok) throw new Error((await res.text()) || "Gagal mengirim email");

    $("#compose").hidden = true;
    resetCompose();
    showSnackbar("Pesan terkirim.");
    const fromId = Number(fd.get("from"));
    if (fromId !== mailboxId) {
      $("#mailboxSelect").value = String(fromId);
      switchMailbox(fromId);
    }
    view = "sent";
    openId = null;
    renderNav();
    apiGetStats();
    apiGetEmails();
  } catch (err) {
    alert(err.message);
  } finally {
    sendBtn.disabled = false;
    sendBtn.innerHTML = `<span class="msf">send</span><span>Kirim</span>`;
  }
});

/* ---------- Boot & Auto-Refresh ---------- */

async function boot() {
  await apiGetMe();
  renderNav();
  await apiGetStats();
  await apiGetEmails();

  setInterval(() => {
    apiGetStats();
    if (!openId) apiGetEmails();
  }, 15000);
}

// A 401 already redirected to the login page; nothing else to report.
boot().catch((err) => { if (err.message !== "unauthorized") console.error(err); });
