// Wmail inbound Worker. Deployed automatically by `wmail cloudflare setup`.
// Bindings: APP_URL (plain text), INBOUND_SECRET (secret), FALLBACK (KV namespace).
// MIME parsing happens on the Wmail server, so this stays well inside the free-tier CPU limit.
export default {
  async email(message, env) {
    const raw = await new Response(message.raw).arrayBuffer();
    let res = null;
    try {
      res = await fetch(env.APP_URL.replace(/\/$/, "") + "/api/inbound/raw", {
        method: "POST",
        headers: {
          "content-type": "message/rfc822",
          "x-inbound-secret": env.INBOUND_SECRET,
          "x-envelope-from": message.from,
          "x-envelope-to": message.to,
        },
        body: raw,
      });
    } catch (e) {
      console.log("push failed:", e);
    }
    if (res && res.status === 404) return message.setReject("No such mailbox");
    // 413/422: the server can never accept this message, so bounce it instead of parking it.
    if (res && (res.status === 413 || res.status === 422)) return message.setReject("Message rejected");
    if (res && res.ok) return;

    // Server unreachable (home server down, tunnel offline, 5xx): park the message.
    // Wmail drains this namespace through the Cloudflare API when it is back.
    // Parked mail expires after 14 days so a stuck record cannot live forever.
    const key = `${Date.now()}-${crypto.randomUUID()}`;
    await env.FALLBACK.put(key, raw, {
      expirationTtl: 14 * 24 * 3600,
      metadata: { to: message.to, from: message.from },
    });
    console.log("parked in KV:", key, res && res.status);
  },
};
