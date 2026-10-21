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
let expanded = new Set(); // ids of the open conversation's messages shown in full
let quoteText = ""; // sent after what was typed: the quoted reply or the forwarded message

const $ = (s) => document.querySelector(s);
const rowsEl = $("#mailrows");
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) =>
  ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
// One color per sender, so a conversation reads the same all the way down.
const avColor = (s) => AV_COLORS[[...String(s ?? "")].reduce((h, c) => h + c.charCodeAt(0), 0) % AV_COLORS.length];
const initial = (name, email) => (name || email || "?").trim().charAt(0).toUpperCase();
const currentMailbox = () => me.mailboxes.find((m) => m.id === mailboxId);
const sendable = () => me.mailboxes.filter((m) => m.canSend);
// Receive-only ("terima saja") grants are read-only: the server refuses moves and deletes.
const canManage = () => !!currentMailbox()?.canSend;
const lc = (s) => String(s || "").toLowerCase();
const isInbox = (x) => x.folder === "inbox" && !x.isArchived && !x.isDeleted;
// A conversation made only of our own outgoing mail has no inbox to go back to.
const outgoing = (x) => x.folder === "sent" || x.folder === "scheduled";
const movable = (x) => x.isDeleted || !outgoing(x) || x.threadCount > 1;
const threadOf = (m) => (m.thread?.length ? m.thread : [m]);

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
  { key: "scheduled", label: "Terjadwal", icon: "watch_later" },
  { key: "archive", label: "Arsip", icon: "archive" },
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
    // Like Gmail: the newest message and anything unread open; the rest fold to one line.
    const t = threadOf(currentMailDetail);
    expanded = new Set(t.filter((x, i) => x.isUnread || i === t.length - 1).map((x) => x.id));
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
  const isSent = outgoing(m);
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
    <span class="row-sender"><span class="row-name">${esc(fromLabel)}</span>${m.threadCount > 1 ? `<span class="count">${m.threadCount}</span>` : ""}</span>
    <span class="row-main">
      ${view !== "inbox" && m.inInbox ? `<span class="row-chip">Kotak masuk</span>` : ""}
      <span class="row-subject">${m.subject ? esc(m.subject) : '<span style="opacity:.55">(tanpa subjek)</span>'}</span>
      <span class="row-snippet">— ${esc(m.snippet || "")}</span>
    </span>
    ${m.hasAttachments ? `<span class="row-attach"><span class="ms">attach_file</span></span>` : ""}
    <span class="row-right">
      <span class="row-date">${esc(formatDate(m.createdAt))}</span>
      <span class="row-actions">
        ${canManage() ? `${m.inInbox ? `<button class="iconbtn" aria-label="Arsipkan" data-act="archive"><span class="ms">archive</span></button>`
          : movable(m) ? `<button class="iconbtn" aria-label="Pindahkan ke Kotak masuk" data-act="inbox"><span class="msf">inbox</span></button>` : ""}
        <button class="iconbtn" aria-label="${view === "trash" ? "Hapus permanen" : "Hapus"}" data-act="delete"><span class="ms">delete</span></button>` : ""}
        ${m.isUnread ? `<button class="iconbtn" aria-label="Tandai sudah dibaca" data-act="read"><span class="msf">drafts</span></button>`
          : `<button class="iconbtn" aria-label="Tandai belum dibaca" data-act="unread"><span class="ms">mark_email_unread</span></button>`}
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
  syncSelAll();
}

// Email HTML goes into a sandboxed iframe: scripts, forms and plugins are blocked,
// links open in a new tab. allow-same-origin (without allow-scripts) only lets us size it.
// Quoted history inside a message, as Gmail, Apple Mail and Thunderbird mark it up.
const QUOTES = ".gmail_quote, blockquote[type=cite], .moz-cite-prefix";

// foldQuotes hides that history behind a "•••" button, as Gmail does in a conversation.
function emailFrame(html, foldQuotes = false) {
  const frame = document.createElement("iframe");
  frame.className = "reader-frame";
  frame.setAttribute("sandbox", "allow-same-origin allow-popups allow-popups-to-escape-sandbox");
  frame.setAttribute("title", "Isi email");
  frame.srcdoc = `<!doctype html><html><head><meta charset="utf-8"><base target="_blank">
    <style>body{margin:0;font:14px/1.6 Roboto,Arial,sans-serif;color:#1f1f1f;overflow-wrap:anywhere}img{max-width:100%;height:auto}
    .wm-fold :is(${QUOTES}){display:none}</style>
    </head><body class="${foldQuotes ? "wm-fold" : ""}">${html}</body></html>`;
  frame.addEventListener("load", () => {
    const doc = frame.contentDocument;
    if (!doc) return;
    // The box, not scrollHeight: that never drops below the frame's own 150px default.
    const fit = () => { frame.style.height = Math.ceil(doc.documentElement.getBoundingClientRect().height) + "px"; };
    fit();
    new ResizeObserver(fit).observe(doc.body);
    if (foldQuotes && doc.querySelector(QUOTES)) {
      const btn = document.createElement("button");
      btn.type = "button";
      btn.className = "quote-toggle";
      btn.setAttribute("aria-label", "Tampilkan teks yang dikutip");
      btn.setAttribute("aria-expanded", "false");
      btn.innerHTML = `<span aria-hidden="true">•••</span>`;
      btn.onclick = () => btn.setAttribute("aria-expanded", String(!doc.body.classList.toggle("wm-fold")));
      frame.after(btn);
    }
  });
  return frame;
}

// textHtml renders plain-text mail; "> " runs become blockquotes instead of showing the marks.
function textHtml(text) {
  const lines = (text || "").replace(/\r/g, "").split("\n");
  let html = "";
  let para = [];
  const flush = () => { if (para.length) html += `<p>${para.join("<br>")}</p>`; para = []; };
  for (let i = 0; i < lines.length;) {
    if (!lines[i].startsWith(">")) {
      if (lines[i].trim()) para.push(esc(lines[i]));
      else flush();
      i++;
      continue;
    }
    const quoted = [];
    for (; i < lines.length && lines[i].startsWith(">"); i++) quoted.push(lines[i].slice(1).replace(/^ /, ""));
    flush();
    html += `<blockquote class="gmail_quote">${textHtml(quoted.join("\n"))}</blockquote>`;
  }
  flush();
  return html;
}

function attachmentsHtml(x) {
  if (!x.attachments?.length) return "";
  return `<div class="reader-content"><div class="reader-attachments-section">
      <div class="reader-attachments-title"><span class="ms">attach_file</span><span>${x.attachments.length} lampiran</span></div>
      <div class="reader-attachments-grid">
        ${x.attachments.map((att) => `
          <a href="/api/attachments/${encodeURIComponent(att.id)}" target="_blank" rel="noopener" class="reader-att-card" title="Unduh ${esc(att.filename)}">
            <span class="ms">attach_file</span>
            <div class="reader-att-info">
              <span class="reader-att-name">${esc(att.filename)}</span>
              <span class="reader-att-size">${formatSize(att.sizeBytes)}</span>
            </div>
          </a>`).join("")}
      </div>
    </div></div>`;
}

function messageHtml(x, open, canReply, foldable) {
  const avatar = `<span class="avatar" style="background:${avColor(x.fromEmail)}" aria-hidden="true">${esc(initial(x.fromName, x.fromEmail))}</span>`;
  const toggle = foldable ? `data-toggle role="button" tabindex="0" aria-expanded="${open}"` : "";
  if (!open) {
    return `<div class="msg is-folded" data-msg="${x.id}">
      <div class="reader-msg-head" ${toggle}>
        ${avatar}
        <div class="reader-who">
          <div class="reader-from">${esc(x.fromName || x.fromEmail)}</div>
          <div class="reader-meta msg-snippet">${esc(x.snippet || "")}</div>
        </div>
        <span class="reader-meta">${esc(formatDate(x.createdAt))}</span>
      </div>
    </div>`;
  }
  const cc = (x.ccRecipients?.length ? `, cc ${x.ccRecipients.join(", ")}` : "") +
    (x.bccRecipients?.length ? `, bcc ${x.bccRecipients.join(", ")}` : "");
  const scheduled = x.folder === "scheduled" ? `<div class="sched-banner">
      <span class="ms">schedule</span><span>Akan dikirim ${esc(formatFullDate(x.createdAt))}</span>
      ${canReply ? `<button class="text-btn" data-unschedule>Batalkan pengiriman</button>` : ""}
    </div>` : "";
  return `<div class="msg" data-msg="${x.id}">
    <div class="reader-msg-head" ${toggle}>
      ${avatar}
      <div class="reader-who">
        <div class="reader-from">${esc(x.fromName || x.fromEmail)} <span class="addr">&lt;${esc(x.fromEmail || "unknown")}&gt;</span></div>
        <div class="reader-meta">kepada ${esc((x.toRecipients || []).join(", ") + cc)} — ${esc(formatFullDate(x.createdAt))}</div>
      </div>
      <span class="reader-msg-actions">
        <button class="iconbtn" data-star aria-label="${x.isStarred ? "Hapus bintang" : "Beri bintang"}">
          <span class="${x.isStarred ? "msf star-on" : "ms"}">star</span>
        </button>
        ${canReply ? `<button class="iconbtn" data-reply="reply" aria-label="Balas"><span class="ms">reply</span></button>
        <button class="iconbtn" data-reply="forward" aria-label="Teruskan"><span class="ms">forward</span></button>` : ""}
      </span>
    </div>
    ${scheduled}
    <div class="reader-content" data-body>${x.bodyHtml?.trim() ? "" : textHtml(x.bodyText)}</div>
    ${attachmentsHtml(x)}
  </div>`;
}

// Reply-all only makes sense when someone besides the sender and us is on the message.
function othersOn(x) {
  const self = lc(currentMailbox()?.address);
  return new Set([x.fromEmail, ...(x.toRecipients || []), ...(x.ccRecipients || [])].map(lc).filter((a) => a && a !== self)).size;
}

function renderReader() {
  const r = $("#reader");
  if (!r) return;
  const comp = $("#compose");
  // An inline reply belongs to the conversation it was started in.
  if (comp.classList.contains("is-inline") && !comp.hidden && Number(comp.dataset.for) !== openId) closeCompose();
  if (!openId || !currentMailDetail) {
    r.hidden = true;
    rowsEl.hidden = false;
    $(".list-tools").hidden = false;
    return;
  }

  r.hidden = false;
  rowsEl.hidden = true;
  $(".list-tools").hidden = true;

  const m = currentMailDetail;
  const thread = threadOf(m);
  const last = thread[thread.length - 1];
  const inbox = thread.some(isInbox);
  for (const id of ["#btnRSpam", "#btnRDelete", "#btnRSnooze"]) $(id).hidden = !canManage();
  $("#btnRArchive").hidden = !canManage() || !inbox;
  $("#btnRInbox").hidden = !canManage() || inbox || !thread.some((x) => x.isDeleted || !outgoing(x));
  const pos = Math.max(1, mails.findIndex((x) => x.id === m.id) + 1);
  $("#readerPos").textContent = `${pos} dari ${mails.length || 1}`;

  const canReply = currentMailbox()?.canSend;
  const subject = thread[0].subject || m.subject;
  $("#readerThread").innerHTML = `
    <h1 class="reader-subject">${subject ? esc(subject) : "(tanpa subjek)"}</h1>
    ${thread.map((x) => messageHtml(x, expanded.has(x.id), canReply, thread.length > 1)).join("")}
    ${canReply ? `<div class="reader-quick">
      <button class="quick-btn" data-reply="reply"><span class="ms">reply</span>Balas</button>
      ${othersOn(last) > 1 ? `<button class="quick-btn" data-reply="replyAll"><span class="ms">reply_all</span>Balas ke semua</button>` : ""}
      <button class="quick-btn" data-reply="forward"><span class="ms">forward</span>Teruskan</button>
    </div>` : `<p class="reader-note">Mailbox ini hanya bisa dibaca (hak terima saja).</p>`}`;

  for (const x of thread) {
    if (expanded.has(x.id) && x.bodyHtml?.trim()) $(`[data-msg="${x.id}"] [data-body]`).appendChild(emailFrame(x.bodyHtml, thread.length > 1));
  }
}

$("#readerThread").addEventListener("click", async (e) => {
  const m = currentMailDetail;
  if (!m) return;
  const thread = threadOf(m);
  const msgEl = e.target.closest("[data-msg]");
  // Buttons inside a message act on it; the ones under the conversation act on the newest.
  const x = msgEl ? thread.find((t) => t.id === Number(msgEl.dataset.msg)) : thread[thread.length - 1];
  const btn = e.target.closest("button");
  if (btn?.dataset.reply) return startReply(x, btn.dataset.reply);
  if (btn?.hasAttribute("data-unschedule")) return unschedule(x);
  if (btn?.hasAttribute("data-star")) {
    x.isStarred = !x.isStarred;
    await apiUpdateEmail(x.id, { isStarred: x.isStarred });
    return renderReader();
  }
  if (!btn && e.target.closest("[data-toggle]")) {
    expanded.has(x.id) ? expanded.delete(x.id) : expanded.add(x.id);
    renderReader();
  }
});
$("#readerThread").addEventListener("keydown", (e) => {
  if ((e.key === "Enter" || e.key === " ") && e.target.matches("[data-toggle]")) {
    e.preventDefault();
    e.target.click();
  }
});

// unschedule cancels a scheduled message and reopens it in the composer, as Gmail does.
async function unschedule(x) {
  const res = await api(`/api/emails/${x.id}/unschedule`, { method: "POST" });
  if (!res.ok) return showSnackbar((await res.text()) || "Gagal membatalkan.");
  const m = await res.json();
  openId = null;
  currentMailDetail = null;
  renderReader();
  apiGetEmails();
  resetCompose();
  openCompose();
  $("#composeFrom").value = String(m.mailboxId);
  $("#composeTo").value = (m.toRecipients || []).join(", ");
  for (const [field, list] of [["Cc", m.ccRecipients], ["Bcc", m.bccRecipients]]) {
    $(`#compose${field}`).value = (list || []).join(", ");
    $(`#compose${field}Row`).hidden = !list?.length;
  }
  $("#composeSubject").value = m.subject === "(no subject)" ? "" : m.subject;
  $("#composeInReplyTo").value = m.inReplyTo || "";
  editor.innerHTML = m.bodyHtml || textHtml(m.bodyText);
  showSnackbar(m.attachments?.length ? "Pengiriman dibatalkan. Lampirannya perlu ditambahkan lagi." : "Pengiriman dibatalkan.");
}

/* ---------- Reply & Forward (inline, under the conversation) ---------- */

function quoteHeader(m) {
  const author = m.fromName ? `${m.fromName} <${m.fromEmail}>` : m.fromEmail;
  return { date: formatFullDate(m.createdAt), author };
}

// setQuote keeps the text to send after the reply and shows the original message,
// formatted, behind "•••". forward lays it out as a forwarded message, not a quote.
function setQuote(text, head, m, forward) {
  quoteText = text;
  const q = $("#composeQuote");
  q.innerHTML = m ? `<div class="gmail_attr">${esc(head).replace(/\n/g, "<br>")}</div>
    <${forward ? "div" : "blockquote"} data-body></${forward ? "div" : "blockquote"}>` : "";
  if (m) {
    const body = q.querySelector("[data-body]");
    if (m.bodyHtml?.trim()) body.appendChild(emailFrame(m.bodyHtml));
    else body.innerHTML = textHtml(m.bodyText);
  }
  q.hidden = !forward;
  $("#btnQuote").hidden = !m;
  $("#btnQuote").setAttribute("aria-expanded", String(!!forward));
}

// startReply opens the reply box under the conversation. mode: reply | replyAll | forward.
function startReply(m, mode) {
  resetCompose();
  openCompose(true);
  const sub = m.subject || "";
  const { date, author } = quoteHeader(m);
  $("#composeMode").textContent = mode === "replyAll" ? "reply_all" : mode;
  // Gmail keeps forwards in the conversation too, so both carry In-Reply-To.
  $("#composeInReplyTo").value = m.messageId || "";
  if (mode === "forward") {
    $("#composeSubject").value = /^fwd?:/i.test(sub) ? sub : `Fwd: ${sub}`;
    const head = `---------- Pesan yang diteruskan ---------\nDari: ${author}\nTanggal: ${date}\nSubjek: ${sub}\nKepada: ${(m.toRecipients || []).join(", ")}`;
    setQuote(`${head}\n\n${m.bodyText || ""}`, head, m, true);
    $("#composeTo").focus();
  } else {
    const self = lc(currentMailbox()?.address);
    // Replying to our own message goes to the people we sent it to.
    const mine = lc(m.fromEmail) === self;
    const to = mine ? m.toRecipients || [] : [m.fromEmail];
    const seen = new Set([self, ...to.map(lc)]);
    const cc = mode !== "replyAll" ? [] : [...(mine ? [] : m.toRecipients || []), ...(m.ccRecipients || [])]
      .filter((a) => !seen.has(lc(a)) && seen.add(lc(a)));
    $("#composeTo").value = to.join(", ");
    $("#composeCc").value = cc.join(", ");
    $("#composeCcRow").hidden = !cc.length;
    $("#composeSubject").value = /^re:/i.test(sub) ? sub : `Re: ${sub}`;
    // The text part quotes with "> " as mail always has; the server turns it into a
    // blockquote for the HTML part, so nobody sees the marks.
    const head = `Pada ${date}, ${author} menulis:`;
    const lines = (m.bodyText || "").replace(/\r/g, "").trimEnd().split("\n");
    setQuote(`${head}\n${lines.map((l) => (l ? `> ${l}` : ">")).join("\n")}`, head, m, false);
    editor.focus();
  }
  $("#compose").scrollIntoView({ block: "nearest" });
}

/* ---------- Snackbar ---------- */

function showSnackbar(text, undo) {
  const sn = $("#snackbar");
  $("#snackText").textContent = text;
  $("#snackUndo").hidden = !undo;
  $("#snackUndo").onclick = () => { sn.hidden = true; undo(); };
  sn.hidden = false;
  clearTimeout(showSnackbar._t);
  showSnackbar._t = setTimeout(() => { sn.hidden = true; }, undo ? 8000 : 4000);
}

/* ---------- Row Click & Actions ---------- */

// moveOut applies patch to each conversation (patch null = delete for good) and,
// given undo, offers to revert it from the snackbar.
async function moveOut(ids, patch, message, undo) {
  await Promise.all(ids.map((id) => (patch ? apiUpdateEmail(id, patch) : apiDeleteEmail(id))));
  showSnackbar(message, undo && (async () => {
    await Promise.all(ids.map((id) => apiUpdateEmail(id, undo)));
    apiGetEmails();
  }));
  selected.clear();
  if (ids.includes(openId)) {
    openId = null;
    currentMailDetail = null;
    renderReader();
  }
  apiGetEmails();
}

// Every action takes conversation ids; the server applies it to the whole conversation.
const ACTIONS = {
  archive: (ids) => moveOut(ids, { isArchived: true }, "Percakapan diarsipkan.", { isArchived: false }),
  inbox: (ids) => moveOut(ids, { isArchived: false, isDeleted: false, folder: "inbox" }, "Dipindahkan ke Kotak masuk."),
  delete: (ids) => view === "trash"
    ? confirm("Hapus permanen percakapan yang dipilih?") && moveOut(ids, null, "Dihapus permanen.")
    : moveOut(ids, { isDeleted: true }, "Percakapan dipindahkan ke Sampah.", { isDeleted: false }),
  snooze: (ids) => moveOut(ids, { folder: "snoozed" }, "Percakapan ditunda.", { folder: "inbox" }),
  spam: (ids) => moveOut(ids, { folder: "spam" }, "Ditandai sebagai spam.", { folder: "inbox" }),
  unread: (ids) => moveOut(ids, { isUnread: true }, "Ditandai belum dibaca."),
  read: (ids) => moveOut(ids, { isUnread: false }, "Ditandai sudah dibaca."),
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
      ACTIONS[act]?.([id]);
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
  const sel = mails.filter((m) => selected.has(m.id));
  const all = mails.length && sel.length === mails.length;
  $("#selAllIcon").textContent = all ? "check_box" : (sel.length ? "indeterminate_check_box" : "check_box_outline_blank");
  // Gmail swaps refresh for actions on the selection.
  $("#bulkTools").hidden = !sel.length;
  $("#btn-refresh").hidden = !!sel.length;
  const show = {
    archive: canManage() && sel.some((m) => m.inInbox),
    inbox: canManage() && sel.some((m) => !m.inInbox && movable(m)),
    delete: canManage(),
    read: sel.some((m) => m.isUnread),
    unread: !sel.some((m) => m.isUnread),
  };
  document.querySelectorAll("[data-bulk]").forEach((b) => { b.hidden = !show[b.dataset.bulk]; });
}

$("#bulkTools").addEventListener("click", (e) => {
  const b = e.target.closest("[data-bulk]");
  if (b) ACTIONS[b.dataset.bulk](mails.filter((m) => selected.has(m.id)).map((m) => m.id));
});

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

for (const [id, act] of [["#btnRArchive", "archive"], ["#btnRInbox", "inbox"], ["#btnRSpam", "spam"],
  ["#btnRSnooze", "snooze"], ["#btnRUnread", "unread"], ["#btnRDelete", "delete"]]) {
  $(id).onclick = () => openId && ACTIONS[act]([openId]);
}

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

// openCompose shows the composer as Gmail's floating window, or inline under the
// open conversation for replies and forwards.
function openCompose(inline = false) {
  const list = sendable();
  if (!list.length) return;
  const cur = currentMailbox();
  const from = cur?.canSend ? cur.id : list[0].id;
  $("#composeFrom").innerHTML = list.map((m) =>
    `<option value="${m.id}" ${m.id === from ? "selected" : ""}>${esc(m.displayName ? `${m.displayName} <${m.address}>` : m.address)}</option>`
  ).join("");
  syncSignature();
  const comp = $("#compose");
  comp.classList.toggle("is-inline", inline);
  comp.classList.remove("is-min", "is-full");
  comp.dataset.for = inline ? String(openId) : "";
  comp.setAttribute("aria-label", inline ? "Balas" : "Pesan baru");
  (inline ? $("#readerBody") : document.body).appendChild(comp);
  comp.hidden = false;
  $("#composeTo").focus();
}

function closeCompose() {
  $("#compose").hidden = true;
  resetCompose();
}

function resetCompose() {
  $("#composeTo").value = "";
  for (const f of ["Cc", "Bcc"]) {
    $(`#compose${f}`).value = "";
    $(`#compose${f}Row`).hidden = true;
  }
  $("#composeSubject").value = "";
  editor.innerHTML = "";
  lastRange = null;
  $("#composeInReplyTo").value = "";
  setQuote("", "", null, false);
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
$("#composeClose").addEventListener("click", closeCompose);
$("#composeDiscard").addEventListener("click", closeCompose);
$("#btnQuote").addEventListener("click", () => {
  const q = $("#composeQuote");
  q.hidden = !q.hidden;
  $("#btnQuote").setAttribute("aria-expanded", String(!q.hidden));
});
$("#composeMin").addEventListener("click", () => $("#compose").classList.toggle("is-min"));
$("#composeFull").addEventListener("click", () => $("#compose").classList.toggle("is-full"));
for (const b of document.querySelectorAll(".text-toggle")) {
  b.addEventListener("click", () => {
    const row = $(`#${b.dataset.show}`);
    row.hidden = false;
    row.querySelector("input").focus();
  });
}

/* ---------- Editor: formatting, links, emoji, signature ---------- */

const editor = $("#composeBody");
let lastRange = null; // the caret in the editor, put back before inserting from a toolbar

document.addEventListener("selectionchange", () => {
  const sel = getSelection();
  if (sel.rangeCount && editor.contains(sel.anchorNode)) lastRange = sel.getRangeAt(0).cloneRange();
});

function restoreCaret() {
  editor.focus();
  if (!lastRange) return;
  const sel = getSelection();
  sel.removeAllRanges();
  sel.addRange(lastRange);
}

// execCommand is deprecated but still the one native way to edit rich text; no library needed.
const exec = (cmd, arg) => { restoreCaret(); document.execCommand(cmd, false, arg); };

$("#btnFormat").addEventListener("click", () => {
  const bar = $("#formatBar");
  bar.hidden = !bar.hidden;
  $("#btnFormat").setAttribute("aria-pressed", String(!bar.hidden));
});
// mousedown would move focus out of the editor and lose the selection being formatted.
$("#formatBar").addEventListener("mousedown", (e) => e.preventDefault());
$("#formatBar").addEventListener("click", (e) => {
  const b = e.target.closest("[data-cmd]");
  if (b) exec(b.dataset.cmd, b.dataset.arg);
});

$("#btnLink").addEventListener("mousedown", (e) => e.preventDefault());
$("#btnLink").addEventListener("click", () => {
  const text = lastRange?.toString() || "";
  let url = prompt("Alamat link (URL atau email):", "")?.trim();
  if (!url) return;
  if (!/^(https?:|mailto:)/i.test(url)) url = (/^[^\s/]+@[^\s/]+$/.test(url) ? "mailto:" : "https://") + url;
  if (text) exec("createLink", url);
  else exec("insertHTML", `<a href="${esc(url)}">${esc(url.replace(/^mailto:/i, ""))}</a>`);
});

const EMOJI = "😀 😁 😂 🤣 😊 😍 😘 😉 😎 🤩 🤔 😅 😢 😭 😡 😴 🙏 👍 👎 👏 🙌 💪 👋 🤝 ❤️ 💔 🔥 ✨ 🎉 🎁 ✅ ❌ ⭐ 📌 📎 📅 ⏰ 💡 📞 ✉️ 🚀 ☕ 🍰 🌹 😇 🥳 🤗 😬".split(" ");
$("#emojiPanel").innerHTML = EMOJI.map((e) => `<button type="button" data-emoji="${e}" aria-label="${e}">${e}</button>`).join("");
$("#emojiPanel").addEventListener("mousedown", (e) => e.preventDefault());
$("#emojiPanel").addEventListener("click", (e) => {
  const b = e.target.closest("[data-emoji]");
  if (!b) return;
  exec("insertText", b.dataset.emoji);
  $("#emojiPanel").hidePopover();
});

// Popovers open next to the button that opened them, above it like Gmail's.
for (const [pop, btn] of [["#sendMenu", "#sendMore"], ["#emojiPanel", "#btnEmoji"]]) {
  $(pop).addEventListener("toggle", (e) => {
    if (e.newState !== "open") return; // measured once open: a closed popover has no width
    const r = $(btn).getBoundingClientRect();
    $(pop).style.left = `${Math.max(8, Math.min(r.left, innerWidth - $(pop).offsetWidth - 8))}px`;
    $(pop).style.bottom = `${innerHeight - r.top + 6}px`;
  });
}

// The sending mailbox's signature lives in its own block, so changing From swaps it.
function syncSignature() {
  const mb = me.mailboxes.find((m) => m.id === Number($("#composeFrom").value));
  let block = editor.querySelector(".wm-signature");
  if (!mb?.signature) return block?.remove();
  if (!editor.textContent.trim() && !block) editor.innerHTML = "<div><br></div>";
  if (!block) {
    block = document.createElement("div");
    block.className = "wm-signature";
    editor.append(document.createElement("br"), block);
  }
  block.innerHTML = `-- <br>${esc(mb.signature).replace(/\n/g, "<br>")}`;
}
$("#composeFrom").addEventListener("change", syncSignature);
$("#btnSignature").addEventListener("mousedown", (e) => e.preventDefault());
$("#btnSignature").addEventListener("click", () => {
  const block = editor.querySelector(".wm-signature");
  if (block) return block.remove();
  if (!me.mailboxes.find((m) => m.id === Number($("#composeFrom").value))?.signature) {
    return showSnackbar("Mailbox ini belum punya tanda tangan. Admin bisa mengaturnya di Pengaturan → Mailbox.");
  }
  syncSignature();
});

/* ---------- Schedule send ---------- */

function schedulePresets() {
  const at = (days, hour) => {
    const t = new Date();
    t.setDate(t.getDate() + days);
    t.setHours(hour, 0, 0, 0);
    return t;
  };
  const toMonday = (8 - new Date().getDay()) % 7 || 7;
  return [["Besok pagi", at(1, 8)], ["Besok siang", at(1, 13)], ["Senin pagi", at(toMonday, 8)]];
}

const scheduleDialog = $("#scheduleDialog");
$("#btnSchedule").addEventListener("click", () => {
  $("#sendMenu").hidePopover();
  $("#schedulePresets").innerHTML = schedulePresets().map(([label, t]) =>
    `<button type="button" class="preset" data-at="${t.toISOString()}"><span>${label}</span><span class="muted">${esc(formatFullDate(t))}</span></button>`).join("");
  const now = new Date(Date.now() - new Date().getTimezoneOffset() * 60000);
  $("#scheduleAt").min = now.toISOString().slice(0, 16);
  $("#scheduleAt").value = "";
  scheduleDialog.showModal();
});
$("#schedulePresets").addEventListener("click", (e) => {
  const b = e.target.closest("[data-at]");
  if (!b) return;
  scheduleDialog.close();
  send(b.dataset.at);
});
$("#scheduleCustom").addEventListener("click", () => {
  const t = new Date($("#scheduleAt").value);
  if (isNaN(t) || t <= new Date()) return showSnackbar("Pilih tanggal dan waktu yang akan datang.");
  scheduleDialog.close();
  send(t.toISOString());
});
$("#scheduleCancel").addEventListener("click", () => scheduleDialog.close());

const fileInput = $("#composeFileInput");
$("#btnAttach").addEventListener("click", () => fileInput.click());
fileInput.addEventListener("change", (e) => {
  for (const f of e.target.files || []) pendingAttachments.push(f);
  renderComposeAttachments();
  fileInput.value = "";
});

const sendBtn = $("#sendBtn");
sendBtn.addEventListener("click", () => send());

// send delivers the composed message now, or at sendAt (ISO time) when scheduling.
async function send(sendAt) {
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
  fd.append("bcc", $("#composeBcc").value.trim());
  fd.append("subject", $("#composeSubject").value.trim());
  const text = editor.innerText.trim();
  fd.append("body", text);
  fd.append("html", text ? editor.innerHTML.trim() : "");
  fd.append("quote", quoteText.trim());
  fd.append("inReplyTo", $("#composeInReplyTo").value.trim());
  if (sendAt) fd.append("sendAt", sendAt);
  for (const file of pendingAttachments) fd.append("attachments", file);

  sendBtn.disabled = true;
  sendBtn.innerHTML = `<span class="msf">schedule</span><span>Mengirim...</span>`;
  try {
    const res = await api("/api/emails/send", { method: "POST", body: fd });
    if (!res.ok) throw new Error((await res.text()) || "Gagal mengirim email");

    // Like Gmail, stay put: a reply shows up at the bottom of its conversation.
    const inline = $("#compose").classList.contains("is-inline");
    closeCompose();
    showSnackbar(sendAt ? `Dijadwalkan: ${formatFullDate(sendAt)}. Lihat di Terjadwal.` : "Pesan terkirim.");
    apiGetStats();
    if (inline && openId) apiGetEmailDetail(openId);
    else apiGetEmails();
  } catch (err) {
    alert(err.message);
  } finally {
    sendBtn.disabled = false;
    sendBtn.innerHTML = `<span class="msf">send</span><span>Kirim</span>`;
  }
}

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
