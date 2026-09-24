/**
 * TorBot Cloudflare Worker — hardened download proxy.
 *
 * Token v2: base64url( 0x02 || iv(12) || AES-GCM(ciphertext+tag) )
 * Key = SHA-256(PROXY_SECRET)
 *
 * Security:
 *  - Encrypted tokens (no cleartext CDN URLs)
 *  - Required exp; optional once+jti single-use (Cache API / KV)
 *  - Per-IP rate limit
 *  - SSRF host blocks for CDN fetch
 *
 * Secrets: PROXY_SECRET (16+), TORBOX_API_KEY (webdav/api)
 * Optional binding: TOKEN_KV (KV namespace) for durable single-use
 */

const enc = new TextEncoder();
const dec = new TextDecoder();
const TOKEN_VERSION = 2;
const IV_LEN = 12;
const RL_PER_MINUTE = 30;

function b64urlToBytes(s) {
  s = s.replace(/-/g, "+").replace(/_/g, "/");
  const pad = s.length % 4 === 0 ? "" : "=".repeat(4 - (s.length % 4));
  const bin = atob(s + pad);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

async function aesKeyFromSecret(secret) {
  const hash = await crypto.subtle.digest("SHA-256", enc.encode(secret));
  return crypto.subtle.importKey("raw", hash, { name: "AES-GCM" }, false, [
    "decrypt",
  ]);
}

async function decryptToken(token, secret) {
  if (!token || !secret || secret.length < 16) return null;
  let blob;
  try {
    blob = b64urlToBytes(token);
  } catch {
    return null;
  }
  if (blob.length < 1 + IV_LEN + 16) return null;
  if (blob[0] !== TOKEN_VERSION) return null;
  const iv = blob.slice(1, 1 + IV_LEN);
  const ct = blob.slice(1 + IV_LEN);
  try {
    const key = await aesKeyFromSecret(secret);
    const plain = await crypto.subtle.decrypt({ name: "AES-GCM", iv }, key, ct);
    const payload = JSON.parse(dec.decode(plain));
    if (!payload || typeof payload !== "object") return null;
    if (payload.exp == null) return null;
    if (Number(payload.exp) < Math.floor(Date.now() / 1000)) return null;
    return payload;
  } catch {
    return null;
  }
}

function basicAuth(user, pass) {
  return "Basic " + btoa(`${user}:${pass}`);
}

function err(status, msg) {
  return new Response(msg, {
    status,
    headers: {
      "content-type": "text/plain; charset=utf-8",
      "cache-control": "no-store",
      "x-content-type-options": "nosniff",
    },
  });
}

function htmlPage(title, body) {
  return new Response(
    `<!DOCTYPE html><html><head><meta charset="utf-8"/><meta name="viewport" content="width=device-width,initial-scale=1"/>
<title>${title}</title>
<style>
body{font-family:system-ui,sans-serif;max-width:40rem;margin:2rem auto;padding:0 1rem;background:#0f1115;color:#e8eaed}
code{background:#1e2230;padding:.15rem .4rem;border-radius:4px}
.card{background:#1a1d27;border-radius:12px;padding:1.25rem;line-height:1.5}
</style></head><body><div class="card"><h1>${title}</h1>${body}</div></body></html>`,
    {
      headers: {
        "content-type": "text/html; charset=utf-8",
        "x-content-type-options": "nosniff",
        "cache-control": "no-store",
      },
    }
  );
}

function clientIp(request) {
  return (
    request.headers.get("cf-connecting-ip") ||
    request.headers.get("x-forwarded-for") ||
    "unknown"
  );
}

async function rateLimit(request, env) {
  const ip = clientIp(request);
  const minute = Math.floor(Date.now() / 60000);
  const keyUrl = `https://torbot-rl.internal/${ip}/${minute}`;
  const cache = caches.default;
  const req = new Request(keyUrl);
  const hit = await cache.match(req);
  let count = 0;
  if (hit) {
    count = parseInt(await hit.text(), 10) || 0;
  }
  count += 1;
  if (count > RL_PER_MINUTE) {
    return err(429, "Rate limited. Try again in a minute.");
  }
  const ttl = 90;
  await cache.put(
    req,
    new Response(String(count), {
      headers: { "cache-control": `max-age=${ttl}`, "content-type": "text/plain" },
    })
  );
  return null;
}

/** Single-use: mark jti consumed. Returns error Response if already used. */
async function consumeOnce(payload, env) {
  if (!payload.once && payload.once !== 1) return null;
  const jti = payload.jti;
  if (!jti || typeof jti !== "string") {
    return err(400, "Single-use token missing jti");
  }
  const exp = Number(payload.exp) || Math.floor(Date.now() / 1000) + 3600;
  const ttl = Math.max(60, Math.min(86400, exp - Math.floor(Date.now() / 1000) + 60));

  // Prefer KV if bound (durable across isolates)
  if (env.TOKEN_KV) {
    const used = await env.TOKEN_KV.get(`jti:${jti}`);
    if (used) return err(403, "This link was already used");
    await env.TOKEN_KV.put(`jti:${jti}`, "1", { expirationTtl: ttl });
    return null;
  }

  // Fallback: Cache API (best-effort single-use on free Workers)
  const cache = caches.default;
  const key = new Request(`https://torbot-jti.internal/${jti}`);
  const hit = await cache.match(key);
  if (hit) return err(403, "This link was already used");
  await cache.put(
    key,
    new Response("1", {
      headers: { "cache-control": `max-age=${ttl}`, "content-type": "text/plain" },
    })
  );
  return null;
}

function isPrivateOrLocalHost(host) {
  if (
    !host ||
    host === "localhost" ||
    host === "127.0.0.1" ||
    host === "[::1]" ||
    host === "0.0.0.0"
  ) {
    return true;
  }
  if (host.endsWith(".local") || host.endsWith(".internal")) return true;
  if (/^(10\.|192\.168\.|172\.(1[6-9]|2\d|3[0-1])\.|169\.254\.|127\.)/.test(host)) {
    return true;
  }
  return false;
}

function isAllowedCdnUrl(urlStr) {
  let u;
  try {
    u = new URL(urlStr);
  } catch {
    return false;
  }
  if (u.protocol !== "https:") return false;
  return !isPrivateOrLocalHost((u.hostname || "").toLowerCase());
}

async function streamFetch(url, init = {}) {
  const resp = await fetch(url, init);
  if (!resp.ok) {
    const t = await resp.text().catch(() => "");
    return err(resp.status, `Upstream ${resp.status}: ${t.slice(0, 200)}`);
  }
  const headers = new Headers();
  for (const h of [
    "content-type",
    "content-length",
    "content-disposition",
    "accept-ranges",
    "content-range",
  ]) {
    const v = resp.headers.get(h);
    if (v) headers.set(h, v);
  }
  headers.set("cache-control", "private, no-store");
  headers.set("x-proxied-by", "torbot-worker");
  headers.set("x-content-type-options", "nosniff");
  headers.set("referrer-policy", "no-referrer");
  return new Response(resp.body, { status: resp.status, headers });
}

function webdavUrl(base, path) {
  const segs = String(path || "")
    .split("/")
    .filter(Boolean)
    .filter((s) => s !== "." && s !== "..")
    .map(encodeURIComponent);
  return `${base}/${segs.join("/")}`;
}

function safeFilename(name) {
  return String(name || "download")
    .replace(/[\r\n";\\]/g, "")
    .slice(0, 120);
}

async function handleWebdav(payload, env) {
  const key = env.TORBOX_API_KEY;
  if (!key) {
    return err(500, "Worker missing TORBOX_API_KEY secret (needed for WebDAV).");
  }
  const base = (env.WEBDAV_BASE || "https://webdav.torbox.app").replace(/\/$/, "");
  const paths = [];
  if (payload.p) paths.push(payload.p);
  if (Array.isArray(payload.alt)) paths.push(...payload.alt);

  let last = null;
  for (const p of paths) {
    if (typeof p !== "string" || p.includes("..")) continue;
    const path = p.startsWith("/") ? p : `/${p}`;
    const finalUrl = webdavUrl(base, path);
    if (!finalUrl.startsWith(base + "/")) continue;
    const resp = await fetch(finalUrl, {
      headers: {
        Authorization: basicAuth("torbox", key),
        "User-Agent": "TorBot-Worker/1.0",
      },
    });
    if (resp.ok) {
      const headers = new Headers();
      const ct = resp.headers.get("content-type");
      if (ct) headers.set("content-type", ct);
      const cd = resp.headers.get("content-disposition");
      if (cd) headers.set("content-disposition", cd);
      else if (payload.n) {
        headers.set(
          "content-disposition",
          `attachment; filename="${safeFilename(payload.n)}"`
        );
      }
      const cl = resp.headers.get("content-length");
      if (cl) headers.set("content-length", cl);
      headers.set("cache-control", "private, no-store");
      headers.set("x-proxied-by", "torbot-worker");
      headers.set("x-content-type-options", "nosniff");
      headers.set("referrer-policy", "no-referrer");
      return new Response(resp.body, { status: 200, headers });
    }
    last = resp.status;
  }
  return err(last || 404, "WebDAV file not found.");
}

function kindToApi(kind) {
  const k = String(kind || "torrent").toLowerCase();
  if (k === "usenet" || k === "u" || k === "n" || k === "nzb") {
    return { path: "/api/usenet/requestdl", idParam: "usenet_id" };
  }
  if (k === "webdl" || k === "web" || k === "w") {
    return { path: "/api/webdl/requestdl", idParam: "web_id" };
  }
  return { path: "/api/torrents/requestdl", idParam: "torrent_id" };
}

async function handleApi(payload, env) {
  const key = env.TORBOX_API_KEY;
  if (!key) {
    return err(500, "Worker missing TORBOX_API_KEY secret (needed for api mode).");
  }
  const id = Number(payload.id);
  if (!Number.isFinite(id) || id < 0 || id > 1e12) {
    return err(400, "Invalid item id");
  }
  const apiBase = (env.TORBOX_API_BASE || "https://api.torbox.app/v1").replace(
    /\/$/,
    ""
  );
  const { path, idParam } = kindToApi(payload.k);
  const params = new URLSearchParams();
  params.set("token", key);
  params.set(idParam, String(Math.trunc(id)));
  if (payload.z) params.set("zip_link", "true");
  else if (payload.f != null) params.set("file_id", String(payload.f));
  else params.set("zip_link", "true");

  const r = await fetch(`${apiBase}${path}?${params}`, {
    headers: {
      Authorization: `Bearer ${key}`,
      "User-Agent": "TorBot-Worker/1.0",
    },
  });
  let data;
  try {
    data = await r.json();
  } catch {
    return err(502, "TorBox API returned non-JSON");
  }
  if (!r.ok || data.success === false) {
    const detail = data.detail || data.error || r.statusText;
    return err(r.status || 400, `TorBox API: ${String(detail).slice(0, 200)}`);
  }
  let url = data.data;
  if (url && typeof url === "object") {
    url = url.url || url.download_url || url.link;
  }
  if (!url || typeof url !== "string" || !isAllowedCdnUrl(url)) {
    return err(502, "TorBox returned no safe download URL");
  }
  return streamFetch(url, { headers: { "User-Agent": "TorBot-Worker/1.0" } });
}

async function handleCdn(payload) {
  const url = payload.u;
  if (!url || typeof url !== "string") return err(400, "Token missing CDN url");
  if (!isAllowedCdnUrl(url)) return err(400, "CDN host not allowed");
  return streamFetch(url, { headers: { "User-Agent": "TorBot-Worker/1.0" } });
}

export default {
  async fetch(request, env) {
    if (request.method !== "GET" && request.method !== "HEAD") {
      return err(405, "GET only");
    }

    const url = new URL(request.url);

    if (url.pathname === "/" || url.pathname === "") {
      return htmlPage(
        "TorBot proxy",
        `<p>Hardened encrypted download proxy.</p>
         <p><code>${url.origin}/d/&lt;token&gt;</code></p>
         <p>Links are short-lived and single-use. Do not share.</p>`
      );
    }

    if (url.pathname === "/health") {
      return new Response("ok", {
        headers: { "content-type": "text/plain", "cache-control": "no-store" },
      });
    }

    const m = url.pathname.match(/^\/d\/([^/]+)$/);
    if (!m) return err(404, "Not found. Use /d/<token>");

    const limited = await rateLimit(request, env);
    if (limited) return limited;

    const token = decodeURIComponent(m[1]);
    const secret = env.PROXY_SECRET;
    if (!secret || secret.length < 16) {
      return err(500, "Worker PROXY_SECRET missing or too short (min 16)");
    }

    const payload = await decryptToken(token, secret);
    if (!payload) return err(403, "Invalid or expired link");

    // Consume single-use BEFORE streaming so double-click fails closed
    const onceErr = await consumeOnce(payload, env);
    if (onceErr) return onceErr;

    const mode = String(payload.m || "cdn").toLowerCase();
    try {
      if (mode === "webdav") return await handleWebdav(payload, env);
      if (mode === "api") return await handleApi(payload, env);
      if (mode === "cdn") return await handleCdn(payload);
      return err(400, `Unknown mode: ${mode}`);
    } catch (e) {
      return err(502, `Proxy error: ${e && e.message ? e.message : e}`);
    }
  },
};
