/**
 * TorBot Cloudflare Worker — hardened download proxy.
 *
 * Token v2: base64url( 0x02 || iv(12) || AES-GCM(ciphertext+tag) )
 * Key = SHA-256(PROXY_SECRET)
 *
 * Modes: list shows a download's files on a page, each linked with a
 * short-lived api token minted here; api streams one file or the zip.
 *
 * Security:
 *  - Encrypted tokens (no cleartext CDN URLs)
 *  - Required exp
 *  - Per-IP rate limit
 *  - SSRF host blocks for CDN fetch
 *
 * Secrets: PROXY_SECRET (16+), TORBOX_API_KEY
 */

import { FONTS } from "./fonts.js";

const enc = new TextEncoder();
const dec = new TextDecoder();
const TOKEN_VERSION = 2;
const IV_LEN = 12;
const RL_PER_MINUTE = 30;
// File links minted on a list page. Long enough for a download manager to
// resume; the page itself can be reloaded for fresh ones.
const FILE_LINK_TTL = 6 * 3600;

function b64urlToBytes(s) {
  s = s.replace(/-/g, "+").replace(/_/g, "/");
  const pad = s.length % 4 === 0 ? "" : "=".repeat(4 - (s.length % 4));
  const bin = atob(s + pad);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

function bytesToB64url(bytes) {
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

async function aesKeyFromSecret(secret) {
  const hash = await crypto.subtle.digest("SHA-256", enc.encode(secret));
  return crypto.subtle.importKey("raw", hash, { name: "AES-GCM" }, false, [
    "encrypt",
    "decrypt",
  ]);
}

/** Seal a payload the same way the bot does, for links minted on a list page. */
async function encryptToken(payload, secret) {
  const key = await aesKeyFromSecret(secret);
  const iv = crypto.getRandomValues(new Uint8Array(IV_LEN));
  const ct = new Uint8Array(
    await crypto.subtle.encrypt(
      { name: "AES-GCM", iv },
      key,
      enc.encode(JSON.stringify(payload))
    )
  );
  const blob = new Uint8Array(1 + IV_LEN + ct.length);
  blob[0] = TOKEN_VERSION;
  blob.set(iv, 1);
  blob.set(ct, 1 + IV_LEN);
  return bytesToB64url(blob);
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

/**
 * Remove the Worker's secrets from text on its way to a visitor. requestdl
 * carries the TorBox API key in its URL, so an upstream error or a fetch
 * failure could otherwise echo it back.
 */
function scrub(text, env) {
  let out = String(text ?? "");
  for (const secret of [env && env.TORBOX_API_KEY, env && env.PROXY_SECRET]) {
    if (secret && secret.length >= 8) out = out.split(secret).join("[redacted]");
  }
  return out.replace(/([?&](?:token|api_key|apikey)=)[^&\s"']+/gi, "$1[redacted]");
}

function escapeHtml(s) {
  return String(s ?? "")
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

// Icons from Remix Icon 4.9.1 (Apache-2.0), https://remixicon.com, inlined so
// pages load nothing from third parties.
const ICONS = {
  film: "M2 3.9934C2 3.44476 2.45531 3 2.9918 3H21.0082C21.556 3 22 3.44495 22 3.9934V20.0066C22 20.5552 21.5447 21 21.0082 21H2.9918C2.44405 21 2 20.5551 2 20.0066V3.9934ZM8 5V19H16V5H8ZM4 5V7H6V5H4ZM18 5V7H20V5H18ZM4 9V11H6V9H4ZM18 9V11H20V9H18ZM4 13V15H6V13H4ZM18 13V15H20V13H18ZM4 17V19H6V17H4ZM18 17V19H20V17H18Z",
  music: "M20 3V17C20 19.2091 18.2091 21 16 21C13.7909 21 12 19.2091 12 17C12 14.7909 13.7909 13 16 13C16.7286 13 17.4117 13.1948 18 13.5351V5H9V17C9 19.2091 7.20914 21 5 21C2.79086 21 1 19.2091 1 17C1 14.7909 2.79086 13 5 13C5.72857 13 6.41165 13.1948 7 13.5351V3H20ZM5 19C6.10457 19 7 18.1046 7 17C7 15.8954 6.10457 15 5 15C3.89543 15 3 15.8954 3 17C3 18.1046 3.89543 19 5 19ZM16 19C17.1046 19 18 18.1046 18 17C18 15.8954 17.1046 15 16 15C14.8954 15 14 15.8954 14 17C14 18.1046 14.8954 19 16 19Z",
  text: "M21 8V20.9932C21 21.5501 20.5552 22 20.0066 22H3.9934C3.44495 22 3 21.556 3 21.0082V2.9918C3 2.45531 3.4487 2 4.00221 2H14.9968L21 8ZM19 9H14V4H5V20H19V9ZM8 7H11V9H8V7ZM8 11H16V13H8V11ZM8 15H16V17H8V15Z",
  archive: "M20 22H4C3.44772 22 3 21.5523 3 21V3C3 2.44772 3.44772 2 4 2H20C20.5523 2 21 2.44772 21 3V21C21 21.5523 20.5523 22 20 22ZM19 20V4H5V20H19ZM14 12V17H10V14H12V12H14ZM12 4H14V6H12V4ZM10 6H12V8H10V6ZM12 8H14V10H12V8ZM10 10H12V12H10V10Z",
  file: "M9 2.00318V2H19.9978C20.5513 2 21 2.45531 21 2.9918V21.0082C21 21.556 20.5551 22 20.0066 22H3.9934C3.44476 22 3 21.5501 3 20.9932V8L9 2.00318ZM5.82918 8H9V4.83086L5.82918 8ZM11 4V9C11 9.55228 10.5523 10 10 10H5V20H19V4H11Z",
  image: "M2.9918 21C2.44405 21 2 20.5551 2 20.0066V3.9934C2 3.44476 2.45531 3 2.9918 3H21.0082C21.556 3 22 3.44495 22 3.9934V20.0066C22 20.5552 21.5447 21 21.0082 21H2.9918ZM20 15V5H4V19L14 9L20 15ZM20 17.8284L14 11.8284L6.82843 19H20V17.8284ZM8 11C6.89543 11 6 10.1046 6 9C6 7.89543 6.89543 7 8 7C9.10457 7 10 7.89543 10 9C10 10.1046 9.10457 11 8 11Z",
  download: "M13 10H18L12 16L6 10H11V3H13V10ZM4 19H20V12H22V20C22 20.5523 21.5523 21 21 21H3C2.44772 21 2 20.5523 2 20V12H4V19Z",
  zip: "M10.4142 3L12.4142 5H21C21.5523 5 22 5.44772 22 6V20C22 20.5523 21.5523 21 21 21H3C2.44772 21 2 20.5523 2 20V4C2 3.44772 2.44772 3 3 3H10.4142ZM18 18H14V15H16V13H14V11H16V9H14V7H11.5858L9.58579 5H4V19H20V7H16V9H18V11H16V13H18V18Z",
  prev: "M10.8284 12.0007L15.7782 16.9504L14.364 18.3646L8 12.0007L14.364 5.63672L15.7782 7.05093L10.8284 12.0007Z",
  next: "M13.1717 12.0007L8.22192 7.05093L9.63614 5.63672L16.0001 12.0007L9.63614 18.3646L8.22192 16.9504L13.1717 12.0007Z",
  folder: "M12.4142 5H21C21.5523 5 22 5.44772 22 6V20C22 20.5523 21.5523 21 21 21H3C2.44772 21 2 20.5523 2 20V4C2 3.44772 2.44772 3 3 3H10.4142L12.4142 5ZM4 7V19H20V7H4Z",
  time: "M12 22C6.47715 22 2 17.5228 2 12C2 6.47715 6.47715 2 12 2C17.5228 2 22 6.47715 22 12C22 17.5228 17.5228 22 12 22ZM12 20C16.4183 20 20 16.4183 20 12C20 7.58172 16.4183 4 12 4C7.58172 4 4 7.58172 4 12C4 16.4183 7.58172 20 12 20ZM13 12H17V14H11V7H13V12Z",
  warn: "M12 22C6.47715 22 2 17.5228 2 12C2 6.47715 6.47715 2 12 2C17.5228 2 22 6.47715 22 12C22 17.5228 17.5228 22 12 22ZM12 20C16.4183 20 20 16.4183 20 12C20 7.58172 16.4183 4 12 4C7.58172 4 4 7.58172 4 12C4 16.4183 7.58172 20 12 20ZM11 15H13V17H11V15ZM11 7H13V13H11V7Z",
  gone: "M19 9H14V4H5V11.8571L6.5 13.25L10 9.5L13 14.5L15 12L18 15L15 14.5L13 17L10 13L7 16.5L5 15.25V20H19V9ZM21 8V20.9932C21 21.5501 20.5552 22 20.0066 22H3.9934C3.44495 22 3 21.556 3 21.0082V2.9918C3 2.45531 3.4487 2 4.00221 2H14.9968L21 8Z",
  up: "M10.0001 19.0001L19 19.0002L19 17.0002L12.0001 17.0001L12 6.8283L15.9497 10.778L17.364 9.36381L11 2.99985L4.63603 9.36381L6.05025 10.778L10 6.82825L10.0001 19.0001Z",
  copy: "M6.9998 6V3C6.9998 2.44772 7.44752 2 7.9998 2H19.9998C20.5521 2 20.9998 2.44772 20.9998 3V17C20.9998 17.5523 20.5521 18 19.9998 18H16.9998V20.9991C16.9998 21.5519 16.5499 22 15.993 22H4.00666C3.45059 22 3 21.5554 3 20.9991L3.0026 7.00087C3.0027 6.44811 3.45264 6 4.00942 6H6.9998ZM5.00242 8L5.00019 20H14.9998V8H5.00242ZM8.9998 6H16.9998V16H18.9998V4H8.9998V6Z",
  check: "M9.9997 15.1709L19.1921 5.97852L20.6063 7.39273L9.9997 17.9993L3.63574 11.6354L5.04996 10.2212L9.9997 15.1709Z",
  lock: "M19 10H20C20.5523 10 21 10.4477 21 11V21C21 21.5523 20.5523 22 20 22H4C3.44772 22 3 21.5523 3 21V11C3 10.4477 3.44772 10 4 10H5V9C5 5.13401 8.13401 2 12 2C15.866 2 19 5.13401 19 9V10ZM5 12V20H19V12H5ZM11 14H13V18H11V14ZM17 10V9C17 6.23858 14.7614 4 12 4C9.23858 4 7 6.23858 7 9V10H17Z",
};

function icon(name, cls = "") {
  return `<svg class="ri ${cls}" viewBox="0 0 24 24" aria-hidden="true"><path d="${ICONS[name]}"/></svg>`;
}

const FILE_TYPES = [
  ["film", /\.(mkv|mp4|m4v|avi|mov|webm|wmv|flv|ts|m2ts|mpg|mpeg)$/i],
  ["music", /\.(mp3|flac|m4a|aac|ogg|opus|wav|alac|wma)$/i],
  ["image", /\.(jpe?g|png|gif|webp|bmp|avif|heic)$/i],
  ["archive", /\.(zip|rar|7z|tar|gz|bz2|xz|iso|r\d\d)$/i],
  ["text", /\.(srt|ass|ssa|sub|vtt|txt|nfo|md|pdf|epub|sfv)$/i],
];

function fileIcon(name) {
  const hit = FILE_TYPES.find(([, re]) => re.test(name));
  return hit ? hit[0] : "file";
}

const PAGE_CSS = `
@font-face{font-family:"Space Grotesk";src:url(/f/grotesk.woff2) format("woff2");font-weight:300 700;font-display:swap}
@font-face{font-family:"Space Mono";src:url(/f/mono.woff2) format("woff2");font-weight:400;font-display:swap}
@font-face{font-family:"Space Mono";src:url(/f/mono-bold.woff2) format("woff2");font-weight:700;font-display:swap}
:root{--bg:#121212;--card:#1a1a1a;--line:#2a2a2a;--text:#ececec;--muted:#9a9a9a;--accent:#ececec;--accent-ink:#121212;--hover:#222222;
--folder:#d8c08a;--film:#e0848f;--music:#b69ae0;--image:#7fb8d4;--archive:#d4a55c;--text-ico:#9cc47a}
*{box-sizing:border-box}
html{background:var(--bg)}
body{margin:0;min-height:100vh;background:var(--bg);color:var(--text);font:15px/1.5 "Space Grotesk",system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;-webkit-font-smoothing:antialiased}
main{max-width:58rem;margin:0 auto;padding:2.5rem 1rem 3rem}
.card{background:var(--card);border:1px solid var(--line);border-radius:14px;overflow:hidden}
.ri{width:1.15em;height:1.15em;fill:currentColor;flex:none;vertical-align:-.2em}
header.top{display:flex;gap:1rem;align-items:flex-start;justify-content:space-between;flex-wrap:wrap;padding:1.25rem 1.25rem 1.1rem}
.title{display:flex;gap:.75rem;align-items:flex-start;min-width:0;flex:1 1 20rem}
.title .badge{display:grid;place-items:center;width:2.5rem;height:2.5rem;border-radius:10px;background:var(--hover);color:var(--accent);flex:none}
.title .badge .ri{width:1.35rem;height:1.35rem}
h1{font-size:1.05rem;font-weight:600;margin:.1rem 0 .2rem;overflow-wrap:anywhere;line-height:1.35}
.meta{color:var(--muted);font-size:.875rem;font-variant-numeric:tabular-nums}
.btn{display:inline-flex;gap:.45rem;align-items:center;background:var(--accent);color:var(--accent-ink);font-weight:600;font-size:.9rem;text-decoration:none;padding:.55rem .95rem;border-radius:9px;white-space:nowrap}
.btn:hover{background:#ffffff}
.btn:focus-visible,a:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
table{width:100%;border-collapse:collapse;table-layout:fixed}
th,td{padding:.7rem 1.25rem;text-align:left;border-top:1px solid var(--line)}
th{font-size:.75rem;font-weight:600;letter-spacing:.04em;text-transform:uppercase;color:var(--muted);background:var(--bg)}
th.size,td.size{width:7.5rem;text-align:right;font-variant-numeric:tabular-nums;color:var(--muted);white-space:nowrap}
th.act,td.act{width:6.25rem;text-align:right;padding-left:0}
tbody tr:hover{background:var(--hover)}
td.name{overflow-wrap:anywhere;font-family:"Space Mono",ui-monospace,monospace;font-size:.84rem}
.meta,th.size,td.size,nav.pages{font-family:"Space Mono",ui-monospace,monospace}
td.name a{display:flex;gap:.65rem;align-items:flex-start;color:var(--text);text-decoration:none}
td.name a:hover span{text-decoration:underline;text-underline-offset:3px}
td.name .ri{margin-top:.15em;color:var(--muted)}
td.name .ri.film{color:var(--film)}td.name .ri.music{color:var(--music)}td.name .ri.image{color:var(--image)}td.name .ri.archive{color:var(--archive)}td.name .ri.text{color:var(--text-ico)}
td.name .ri.folder{color:var(--folder)}
td.name .count{color:var(--muted);font-size:.78rem;white-space:nowrap;margin-left:.6rem}
tr.up td.name a{color:var(--muted)}
nav.crumbs{display:flex;flex-wrap:wrap;gap:.4rem;align-items:center;padding:.7rem 1.25rem;border-top:1px solid var(--line);font-size:.85rem;color:var(--muted);overflow-wrap:anywhere}
nav.crumbs a{color:var(--text);text-decoration:none}
nav.crumbs a:hover{text-decoration:underline;text-underline-offset:3px}
nav.crumbs .sep{opacity:.45}
.dl{display:inline-grid;place-items:center;width:2.1rem;height:2.1rem;border-radius:8px;color:var(--accent);border:1px solid var(--line)}
.dl:hover{background:var(--accent);color:var(--accent-ink);border-color:var(--accent)}
.acts{display:inline-flex;gap:.35rem;vertical-align:middle}
button.dl{background:none;font:inherit;padding:0;cursor:pointer}
button.copy .ri.check,button.copy.done .ri.copy{display:none}
button.copy.done .ri.check{display:block}
button.copy.done,button.copy.done:hover{background:var(--text-ico);color:var(--accent-ink);border-color:var(--text-ico)}
.sr{position:absolute;width:1px;height:1px;overflow:hidden;clip:rect(0 0 0 0);white-space:nowrap}
footer.bar{display:flex;gap:1rem;align-items:center;justify-content:space-between;flex-wrap:wrap;padding:.85rem 1.25rem;border-top:1px solid var(--line);color:var(--muted);font-size:.85rem}
.note{display:inline-flex;gap:.4rem;align-items:center}
nav.pages{display:flex;gap:.5rem;align-items:center;font-variant-numeric:tabular-nums}
nav.pages a,nav.pages span.off{display:inline-grid;place-items:center;width:2rem;height:2rem;border-radius:8px;border:1px solid var(--line);color:var(--text);text-decoration:none}
nav.pages a:hover{background:var(--hover)}
nav.pages span.off{opacity:.35}
.msg{display:flex;flex-direction:column;align-items:center;text-align:center;gap:.6rem;padding:3rem 1.5rem}
.msg .ri{width:2.4rem;height:2.4rem;color:var(--muted)}
.msg h1{font-size:1.1rem;margin:0}
.msg p{margin:0;color:var(--muted);max-width:28rem}
@media (max-width:560px){main{padding:1rem .6rem 2rem}nav.crumbs{padding:.6rem .8rem}td.name .count{display:none}th,td{padding:.65rem .8rem}th.size,td.size{width:5.5rem}td.size{font-size:.85rem}th.act,td.act{width:5.2rem}.dl{width:2rem;height:2rem}.acts{gap:.3rem}header.top{padding:1rem}}
`;

// Nothing from other origins. The only script is the one inline block a page
// passes to htmlPage, allowed by a per-response nonce.
const PAGE_CSP =
  "default-src 'none'; style-src 'unsafe-inline'; font-src 'self'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'";

// Copy link buttons: copy the absolute file link, then show a tick for a
// moment. The textarea fallback covers browsers without the Clipboard API.
const COPY_SCRIPT = `
const say = document.getElementById("copied");
document.addEventListener("click", async (e) => {
  const b = e.target.closest("button.copy");
  if (!b) return;
  const url = new URL(b.dataset.href, location.href).href;
  let ok = false;
  try {
    await navigator.clipboard.writeText(url);
    ok = true;
  } catch {
    const t = document.createElement("textarea");
    t.value = url;
    t.setAttribute("readonly", "");
    t.style.cssText = "position:fixed;opacity:0";
    document.body.append(t);
    t.select();
    try { ok = document.execCommand("copy"); } catch {}
    t.remove();
  }
  b.classList.toggle("done", ok);
  b.title = ok ? "Copied" : "Copy failed";
  say.textContent = ok ? "Link copied" : "Could not copy the link";
  clearTimeout(b.reset);
  b.reset = setTimeout(() => { b.classList.remove("done"); b.title = "Copy link"; }, 1500);
});`;

/** title is plain text; body is trusted HTML built by this Worker; script is
 * trusted inline JavaScript, or "" for none. */
function htmlPage(title, body, status = 200, script = "") {
  let csp = PAGE_CSP;
  let tag = "";
  if (script) {
    const nonce = bytesToB64url(crypto.getRandomValues(new Uint8Array(16)));
    csp += `; script-src 'nonce-${nonce}'`;
    tag = `<script nonce="${nonce}">${script}</script>`;
  }
  return new Response(
    `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"/><meta name="viewport" content="width=device-width,initial-scale=1"/>
<meta name="robots" content="noindex,nofollow"/><meta name="color-scheme" content="dark"/>
<title>${escapeHtml(title)}</title><style>${PAGE_CSS}</style></head><body><main>${body}</main>${tag}</body></html>`,
    {
      status,
      headers: {
        "content-type": "text/html; charset=utf-8",
        "x-content-type-options": "nosniff",
        "cache-control": "no-store",
        "referrer-policy": "no-referrer",
        "x-robots-tag": "noindex, nofollow",
        "content-security-policy": csp,
      },
    }
  );
}

/** A centred one-message page: link expired, download gone, and the like. */
function messagePage(iconName, title, text, status = 200) {
  return htmlPage(
    title,
    `<div class="card msg">${icon(iconName)}<h1>${escapeHtml(title)}</h1><p>${escapeHtml(text)}</p></div>`,
    status
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

async function streamFetch(url, init = {}, filename = "", env = null) {
  const resp = await fetch(url, init);
  if (!resp.ok) {
    const t = await resp.text().catch(() => "");
    return err(resp.status, `Upstream ${resp.status}: ${scrub(t, env).slice(0, 200)}`);
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
  if (filename && !headers.has("content-disposition")) {
    headers.set("content-disposition", `attachment; filename="${safeFilename(filename)}"`);
  }
  headers.set("cache-control", "private, no-store");
  headers.set("x-proxied-by", "torbot-worker");
  headers.set("x-content-type-options", "nosniff");
  headers.set("referrer-policy", "no-referrer");
  return new Response(resp.body, { status: resp.status, headers });
}

function safeFilename(name) {
  return String(name || "download")
    .replace(/[\r\n";\\]/g, "")
    .slice(0, 120);
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
    return err(r.status || 400, `TorBox API: ${scrub(detail, env).slice(0, 200)}`);
  }
  let url = data.data;
  if (url && typeof url === "object") {
    url = url.url || url.download_url || url.link;
  }
  if (!url || typeof url !== "string" || !isAllowedCdnUrl(url)) {
    return err(502, "TorBox returned no safe download URL");
  }
  return streamFetch(url, { headers: { "User-Agent": "TorBot-Worker/1.0" } }, payload.n, env);
}

function kindToList(kind) {
  const k = String(kind || "torrent").toLowerCase();
  if (k === "usenet" || k === "u" || k === "n" || k === "nzb") return "/api/usenet/mylist";
  if (k === "webdl" || k === "web" || k === "w") return "/api/webdl/mylist";
  return "/api/torrents/mylist";
}

function humanSize(bytes) {
  let n = Number(bytes) || 0;
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return i === 0 ? `${n} B` : `${n.toFixed(2).replace(/\.?0+$/, "")} ${units[i]}`;
}

/** Rows per file page. Links are minted only for the rows on screen. */
const PAGE_SIZE = 25;

/**
 * list mode: show a download's files as a paged table, each with its own
 * link, plus the whole download as one zip. A single-file download skips the
 * page and streams the file itself, so it never arrives wrapped in a zip.
 */
async function handleList(payload, env, secret, url) {
  const key = env.TORBOX_API_KEY;
  if (!key) {
    return err(500, "Worker missing TORBOX_API_KEY secret (needed for list mode).");
  }
  const id = Number(payload.id);
  if (!Number.isFinite(id) || id < 0 || id > 1e12) {
    return err(400, "Invalid item id");
  }
  const apiBase = (env.TORBOX_API_BASE || "https://api.torbox.app/v1").replace(/\/$/, "");
  const params = new URLSearchParams({ id: String(Math.trunc(id)), bypass_cache: "true" });
  const r = await fetch(`${apiBase}${kindToList(payload.k)}?${params}`, {
    headers: { Authorization: `Bearer ${key}`, "User-Agent": "TorBot-Worker/1.0" },
  });
  let data;
  try {
    data = await r.json();
  } catch {
    return err(502, "TorBox API returned non-JSON");
  }
  let item = data && data.data;
  if (Array.isArray(item)) item = item[0];
  if (!r.ok || data.success === false || !item || typeof item !== "object") {
    return messagePage("gone", "Download removed", "This download is no longer available.", 404);
  }
  const title = item.name || payload.n || "Download";

  const files = (Array.isArray(item.files) ? item.files : []).filter(
    (f) => f && f.id != null
  );
  const unfinished =
    item.download_finished === false ||
    (typeof item.progress === "number" && item.progress < 1);
  if (unfinished || files.length === 0) {
    const pct =
      typeof item.progress === "number" ? ` It is ${Math.floor(item.progress * 100)}% done.` : "";
    return messagePage("time", "Still downloading", `Come back once it finishes.${pct}`);
  }

  if (files.length === 1) {
    const f = files[0];
    return handleApi({ k: payload.k, id, f: f.id, n: f.short_name || f.name }, env);
  }

  const now = Math.floor(Date.now() / 1000);
  const mint = (claims) =>
    encryptToken(
      { v: 1, iat: now, exp: now + FILE_LINK_TTL, m: "api", k: payload.k, id, ...claims },
      secret
    );

  // TorBox names files by their path inside the download. Drop the top folders
  // every file shares, whatever they are called, so the page opens on what is
  // actually inside rather than one long folder name repeated on every row.
  let paths = files.map((f) => String(f.name || f.short_name || f.id).split("/").filter(Boolean));
  while (paths.every((p) => p.length > 1 && p[0] === paths[0][0])) {
    paths = paths.map((p) => p.slice(1));
  }
  const entries = files.map((f, i) => ({ f, parts: paths[i] }));

  // ?dir= is the folder on screen, as path segments below that root. It only
  // filters TorBox's file list, and a folder that is not there falls back to
  // the top.
  let dir = (url.searchParams.get("dir") || "").split("/").filter(Boolean);
  const inDir = (parts) => parts.length > dir.length && dir.every((seg, i) => parts[i] === seg);
  if (dir.length && !entries.some((e) => inDir(e.parts))) dir = [];

  const folders = new Map();
  const here = [];
  for (const e of entries) {
    if (!inDir(e.parts)) continue;
    const rest = e.parts.slice(dir.length);
    if (rest.length > 1) {
      const folder = folders.get(rest[0]) || { name: rest[0], count: 0, size: 0 };
      folder.count++;
      folder.size += Number(e.f.size) || 0;
      folders.set(rest[0], folder);
    } else {
      here.push({ name: rest[0], f: e.f });
    }
  }
  const byName = (a, b) => a.name.localeCompare(b.name, undefined, { numeric: true });
  const rows = [
    ...[...folders.values()].sort(byName).map((folder) => ({ folder })),
    ...here.sort(byName).map((file) => ({ file })),
  ];

  const pages = Math.max(1, Math.ceil(rows.length / PAGE_SIZE));
  const requested = parseInt(url.searchParams.get("page") || "1", 10);
  const page = Math.min(Math.max(Number.isFinite(requested) ? requested : 1, 1), pages);
  const shown = rows.slice((page - 1) * PAGE_SIZE, page * PAGE_SIZE);

  // Query string for a folder and page; "?" alone is the top, first page.
  const at = (segments, n = 1) => {
    const q = new URLSearchParams();
    if (segments.length) q.set("dir", segments.join("/"));
    if (n > 1) q.set("page", String(n));
    return escapeHtml(`?${q}`);
  };

  const total = files.reduce((sum, f) => sum + (Number(f.size) || 0), 0);
  const zip = await mint({ z: 1, n: title });
  const body = await Promise.all(
    shown.map(async ({ folder, file }) => {
      if (folder) {
        const href = at([...dir, folder.name]);
        const count = `${folder.count} file${folder.count === 1 ? "" : "s"}`;
        return `<tr><td class="name"><a href="${href}">${icon("folder", "folder")}<span>${escapeHtml(folder.name)}</span><span class="count">${count}</span></a></td>` +
          `<td class="size">${humanSize(folder.size)}</td>` +
          `<td class="act"><span class="acts"><a class="dl" href="${href}" aria-label="Open ${escapeHtml(folder.name)}" title="Open">${icon("next")}</a></span></td></tr>`;
      }
      const { f, name } = file;
      const href = `/d/${await mint({ f: f.id, n: f.short_name || name })}`;
      const type = fileIcon(name);
      return `<tr><td class="name"><a href="${href}" rel="nofollow">${icon(type, type)}<span>${escapeHtml(name)}</span></a></td>` +
        `<td class="size">${humanSize(f.size)}</td>` +
        `<td class="act"><span class="acts"><button type="button" class="dl copy" data-href="${href}" aria-label="Copy link to ${escapeHtml(name)}" title="Copy link">${icon("copy", "copy")}${icon("check", "check")}</button>` +
        `<a class="dl" href="${href}" rel="nofollow" aria-label="Download ${escapeHtml(name)}" title="Download">${icon("download")}</a></span></td></tr>`;
    })
  );
  if (dir.length) {
    body.unshift(`<tr class="up"><td class="name"><a href="${at(dir.slice(0, -1))}">${icon("up")}<span>Up one folder</span></a></td><td class="size"></td><td class="act"></td></tr>`);
  }

  const crumbs = dir.length
    ? `<nav class="crumbs" aria-label="Folder"><a href="?">All files</a>` +
      dir
        .map((seg, i) =>
          i === dir.length - 1
            ? `<span class="sep">/</span><span aria-current="page">${escapeHtml(seg)}</span>`
            : `<span class="sep">/</span><a href="${at(dir.slice(0, i + 1))}">${escapeHtml(seg)}</a>`
        )
        .join("") +
      `</nav>`
    : "";

  const pageLink = (n, name, label) =>
    n >= 1 && n <= pages
      ? `<a href="${at(dir, n)}" aria-label="${label}">${icon(name)}</a>`
      : `<span class="off" aria-hidden="true">${icon(name)}</span>`;
  const nav =
    pages > 1
      ? `<nav class="pages" aria-label="Pages">${pageLink(page - 1, "prev", "Previous page")}<span>Page ${page} of ${pages}</span>${pageLink(page + 1, "next", "Next page")}</nav>`
      : "";

  return htmlPage(
    dir.length ? `${dir[dir.length - 1]} · ${title}` : title,
    `<div class="card">
<header class="top"><div class="title"><span class="badge">${icon("folder")}</span><div><h1>${escapeHtml(title)}</h1>
<div class="meta">${files.length} files · ${humanSize(total)}</div></div></div>
<a class="btn" href="/d/${zip}" rel="nofollow">${icon("zip")}Download all</a></header>
${crumbs}<table><thead><tr><th>Name</th><th class="size">Size</th><th class="act"><span hidden>Actions</span></th></tr></thead>
<tbody>${body.join("")}</tbody></table>
<footer class="bar"><span class="note">${icon("time")}Links expire in ${FILE_LINK_TTL / 3600} hours. Reload for fresh ones.</span>${nav}</footer>
</div><p class="sr" id="copied" role="status" aria-live="polite"></p>`,
    200,
    COPY_SCRIPT
  );
}

export default {
  async fetch(request, env) {
    if (request.method !== "GET" && request.method !== "HEAD") {
      return err(405, "GET only");
    }

    const url = new URL(request.url);

    // Say nothing about what lives here: the proxy is only reached by link.
    if (url.pathname === "/" || url.pathname === "") {
      return messagePage("lock", "Nothing here", "This page intentionally left blank.");
    }

    const font = url.pathname.match(/^\/f\/([a-z-]+)\.woff2$/);
    if (font && FONTS[font[1]]) {
      return new Response(Uint8Array.from(atob(FONTS[font[1]]), (c) => c.charCodeAt(0)), {
        headers: {
          "content-type": "font/woff2",
          "cache-control": "public, max-age=31536000, immutable",
          "x-content-type-options": "nosniff",
        },
      });
    }

    if (url.pathname === "/health") {
      return new Response("ok", {
        headers: { "content-type": "text/plain", "cache-control": "no-store" },
      });
    }

    const m = url.pathname.match(/^\/d\/([^/]+)$/);
    if (!m) return messagePage("warn", "Not found", "There is nothing at this address.", 404);

    const limited = await rateLimit(request, env);
    if (limited) return limited;

    const token = decodeURIComponent(m[1]);
    const secret = env.PROXY_SECRET;
    if (!secret || secret.length < 16) {
      return err(500, "Worker PROXY_SECRET missing or too short (min 16)");
    }

    const payload = await decryptToken(token, secret);
    if (!payload) {
      return messagePage("time", "Link expired", "This link is no longer valid. Ask for a new one.", 403);
    }

    const mode = String(payload.m || "").toLowerCase();
    try {
      if (mode === "api") return await handleApi(payload, env);
      if (mode === "list") return await handleList(payload, env, secret, url);
      return err(400, `Unknown mode: ${mode}`);
    } catch (e) {
      // The message can carry the requestdl URL, API key included: log it
      // scrubbed to the Worker's own logs, and tell the visitor nothing more.
      console.error("proxy error", scrub(e && e.message ? e.message : e, env));
      return err(502, "Proxy error. Try again in a moment.");
    }
  },
};
