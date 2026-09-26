// Tests for worker.js against a fake TorBox and CDN. Run: npm test
//
// Tokens are sealed here the way the bot seals them (internal/proxy), so these
// tests also pin the format the two sides share.
//
// Set PAGES_OUT=<dir> to save the rendered pages there, for a look in a browser.
import { test, before } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";

const SECRET = "0123456789abcdef-secret";
const KEY = "tbx-live-SECRETKEY-123456";
const ENV = { PROXY_SECRET: SECRET, TORBOX_API_KEY: KEY };

// --- fakes -----------------------------------------------------------------

// Like a real season pack: TorBox's top folder is not named like the torrent,
// and holds episodes, a few subfolders, and one folder nested deeper.
const ROOT = "Show (BD 1080p) [Group]";
const episodes = Array.from({ length: 30 }, (_, i) => {
  const ep = `S01E${String(i + 1).padStart(2, "0")}`;
  return { id: 100 + i, name: `${ROOT}/Show - ${ep}.mkv`, short_name: `Show - ${ep}.mkv`, size: 1500000000 };
});

const ITEMS = {
  1: {
    id: 1, name: "[Group] Show <S1> | Season 1 + OVAs", download_finished: true, progress: 1,
    files: [
      ...episodes,
      { id: 10, name: `${ROOT}/Extras/NCED.mkv`, short_name: "NCED.mkv", size: 180000000 },
      { id: 11, name: `${ROOT}/Extras/Creditless/NCOP.mkv`, short_name: "NCOP.mkv", size: 190000000 },
      { id: 12, name: `${ROOT}/OVA (Sequel)/Show - S00E08.mkv`, short_name: "Show - S00E08.mkv", size: 1450000000 },
      { id: 2, name: `${ROOT}/Specials/Show - S00E10.mkv`, short_name: "Show - S00E10.mkv", size: 2048 },
      { id: 3, name: `${ROOT}/Specials/Show - S00E2.srt`, short_name: "Show - S00E2.srt", size: 1024 },
    ],
  },
  2: { id: 2, name: "Film.mkv", download_finished: true, progress: 1,
       files: [{ id: 5, name: "Film.mkv", short_name: "Film.mkv", size: 100 }] },
  4: { id: 4, name: "Busy", download_finished: false, progress: 0.5, files: [] },
};

let upstream; // swapped by tests that break TorBox on purpose

function fakeFetch(url) {
  const u = new URL(url);
  if (u.pathname.endsWith("/mylist")) {
    const item = ITEMS[u.searchParams.get("id")];
    return Response.json({ success: true, data: item ? [item] : [] });
  }
  if (u.pathname.endsWith("/requestdl")) {
    const which = u.searchParams.get("zip_link") ? "zip" : "file-" + u.searchParams.get("file_id");
    return Response.json({ success: true, data: `https://cdn.example/${which}` });
  }
  if (u.hostname === "cdn.example") {
    return new Response("bytes:" + u.pathname, { headers: { "content-type": "application/octet-stream" } });
  }
  throw new Error("unexpected fetch " + url);
}

let worker;
before(async () => {
  const store = new Map();
  globalThis.caches = { default: { match: async (r) => store.get(r.url), put: async (r, v) => store.set(r.url, v) } };
  globalThis.fetch = async (url) => (upstream || fakeFetch)(url);
  console.error = () => {}; // the Worker logs scrubbed errors; keep test output clean
  ({ default: worker } = await import("../worker.js"));
});

// --- helpers -----------------------------------------------------------------

let ip = 0;
// A fresh client IP per request keeps the Worker's rate limit out of the way.
const get = (path) =>
  worker.fetch(new Request("https://dl.example.workers.dev" + path, { headers: { "cf-connecting-ip": `10.0.0.${++ip % 250}` } }), ENV);

const b64url = (bytes) => btoa(String.fromCharCode(...bytes)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
const aesKey = async (usage) =>
  crypto.subtle.importKey("raw", await crypto.subtle.digest("SHA-256", new TextEncoder().encode(SECRET)), { name: "AES-GCM" }, false, [usage]);

async function seal(claims) {
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const ct = new Uint8Array(await crypto.subtle.encrypt({ name: "AES-GCM", iv }, await aesKey("encrypt"),
    new TextEncoder().encode(JSON.stringify(claims))));
  return b64url(Uint8Array.of(2, ...iv, ...ct));
}

async function open(token) {
  const b = Uint8Array.from(atob(token.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((token.length + 3) % 4)), (c) => c.charCodeAt(0));
  return new TextDecoder().decode(await crypto.subtle.decrypt({ name: "AES-GCM", iv: b.slice(1, 13) }, await aesKey("decrypt"), b.slice(13)));
}

// A page link as the bot makes it: list mode, 7 days.
const pageLink = (id) =>
  seal({ v: 1, exp: Math.floor(Date.now() / 1000) + 7 * 86400, iat: 0, m: "list", k: "torrent", id, n: "fallback" });

// Rows of the table, in order: folders (href "?dir=..."), then files ("/d/...").
// The "Up one folder" row is left out.
const rowsOf = (html) =>
  [...html.matchAll(/<tr>(?:(?!<\/tr>).)*?<td class="name"><a href="([^"]+)"[^>]*>.*?<span>(.*?)<\/span>/g)].map((m) => ({
    href: m[1].replace(/&amp;/g, "&"),
    name: m[2].replace(/&amp;/g, "&"),
  }));

function save(name, html) {
  if (process.env.PAGES_OUT) fs.writeFileSync(`${process.env.PAGES_OUT}/${name}.html`, html);
}

// --- tests -------------------------------------------------------------------

test("file page: common top folder dropped, folders first, paged, strict headers", async () => {
  const token = await pageLink(1);
  const r = await get("/d/" + token);
  const html = await r.text();
  save("page1", html);

  assert.equal(r.status, 200);
  assert.match(r.headers.get("content-security-policy"), /default-src 'none'/);
  assert.equal(r.headers.get("referrer-policy"), "no-referrer");
  assert.ok(html.includes("<h1>[Group] Show &lt;S1&gt; | Season 1 + OVAs</h1>"), "title escaped");
  assert.ok(!html.includes("<S1>"), "no raw name anywhere");
  assert.ok(!html.includes(ROOT), "shared top folder is not shown");
  assert.ok(html.includes("35 files · 43.6 GB"), html.match(/class="meta">([^<]+)/)[1]);
  assert.ok(!html.includes('class="crumbs"') && !html.includes('class="up"'), "no breadcrumb at the top");

  const rows = rowsOf(html);
  assert.equal(rows.length, 25, "25 rows per page");
  assert.deepEqual(rows.slice(0, 5).map((x) => x.name), ["Extras", "OVA (Sequel)", "Specials", "Show - S01E01.mkv", "Show - S01E02.mkv"]);
  assert.equal(rows[0].href, "?dir=Extras");
  assert.ok(html.includes('<span class="count">2 files</span>') && html.includes('<span class="count">1 file</span>'));
  assert.ok(html.includes("Page 1 of 2") && html.includes('href="?page=2"'));

  const page2 = await (await get("/d/" + token + "?page=2")).text();
  save("page2", page2);
  assert.equal(rowsOf(page2).length, 8, "33 rows: 3 folders and 30 episodes");
  assert.ok(page2.includes("Page 2 of 2") && page2.includes('href="?"'));

  // Out-of-range pages clamp rather than fail.
  assert.ok((await (await get("/d/" + token + "?page=99")).text()).includes("Page 2 of 2"));
  // The page link is reusable: a channel post is opened by many people.
  assert.equal((await get("/d/" + token)).status, 200);
});

test("folders open, nest, and lead back up", async () => {
  const token = await pageLink(1);

  const specials = await (await get("/d/" + token + "?dir=Specials")).text();
  save("specials", specials);
  assert.deepEqual(rowsOf(specials).map((x) => x.name), ["Show - S00E2.srt", "Show - S00E10.mkv"], "file names only, sorted");
  assert.ok(specials.includes('<a href="?">All files</a>') && specials.includes('<span aria-current="page">Specials</span>'));
  assert.match(specials, /<tr class="up"><td class="name"><a href="\?">/);
  assert.ok(specials.includes("<title>Specials · [Group] Show &lt;S1&gt;"));

  const extras = await (await get("/d/" + token + "?dir=Extras")).text();
  assert.deepEqual(rowsOf(extras).map((x) => x.name), ["Creditless", "NCED.mkv"]);
  assert.equal(rowsOf(extras)[0].href, "?dir=Extras%2FCreditless");

  const deep = await (await get("/d/" + token + "?dir=Extras%2FCreditless")).text();
  assert.deepEqual(rowsOf(deep).map((x) => x.name), ["NCOP.mkv"]);
  assert.ok(deep.includes('<a href="?dir=Extras">Extras</a>'), "breadcrumb links the parent");
  assert.match(deep, /<tr class="up"><td class="name"><a href="\?dir=Extras">/);

  // A file inside a folder downloads like any other.
  assert.equal(await (await get(rowsOf(deep)[0].href)).text(), "bytes:/file-11");
});

test("unknown or hostile folders fall back to the top", async () => {
  const token = await pageLink(1);
  for (const dir of ["Nope", "..", "<script>alert(1)</script>", "Specials/../../x"]) {
    const html = await (await get("/d/" + token + "?dir=" + encodeURIComponent(dir))).text();
    assert.ok(!html.includes("<script>"), dir);
    assert.ok(!html.includes('class="crumbs"'), `${dir}: shown as the top`);
    assert.equal(rowsOf(html)[0].name, "Extras", dir);
  }
});

test("file links stream one file with its name, reusably; download all is the zip", async () => {
  const html = await (await get("/d/" + (await pageLink(1)) + "?dir=Specials")).text();
  const special = rowsOf(html)[1].href;

  let r = await get(special);
  assert.equal(await r.text(), "bytes:/file-2");
  assert.match(r.headers.get("content-disposition"), /filename="Show - S00E10.mkv"/);
  assert.equal(await (await get(special)).text(), "bytes:/file-2", "reusable, so downloads can resume");

  const zip = html.match(/class="btn" href="(\/d\/[^"]+)"/)[1];
  assert.equal(await (await get(zip)).text(), "bytes:/zip");
});

test("file rows carry a copy link button; the script runs only by nonce", async () => {
  const r = await get("/d/" + (await pageLink(1)) + "?dir=Specials");
  const html = await r.text();
  const buttons = [...html.matchAll(/<button type="button" class="dl copy" data-href="([^"]+)"/g)].map((m) => m[1]);
  assert.deepEqual(buttons, rowsOf(html).map((x) => x.href), "one per file, copying its download link");

  const nonce = r.headers.get("content-security-policy").match(/script-src 'nonce-([\w-]+)'/)[1];
  assert.equal([...html.matchAll(/<script\b[^>]*>/g)].length, 1, "one script");
  assert.ok(html.includes(`<script nonce="${nonce}">`));
  assert.notEqual(nonce, (await (await get("/d/" + (await pageLink(1)))).headers.get("content-security-policy")).match(/nonce-([\w-]+)/)[1], "fresh per page");

  const top = await (await get("/d/" + (await pageLink(1)))).text();
  assert.ok(!/<tr><td class="name"><a href="\?dir=[^"]*">(?:(?!<\/tr>).)*class="dl copy"/.test(top), "folders have no copy button");

  // Message pages stay script-free.
  const gone = await get("/d/" + (await pageLink(3)));
  assert.ok(!gone.headers.get("content-security-policy").includes("script-src"));
  assert.ok(!(await gone.text()).includes("<script"));
});

test("single-file download skips the page", async () => {
  const r = await get("/d/" + (await pageLink(2)));
  assert.equal(await r.text(), "bytes:/file-5");
  assert.match(r.headers.get("content-disposition"), /filename="Film.mkv"/);
});

test("removed, unfinished and expired links get a message page", async () => {
  let r = await get("/d/" + (await pageLink(3)));
  const gone = await r.text();
  save("gone", gone);
  assert.equal(r.status, 404);
  assert.ok(gone.includes("Download removed"));

  const busy = await (await get("/d/" + (await pageLink(4)))).text();
  save("busy", busy);
  assert.ok(busy.includes("Still downloading") && busy.includes("50% done"));

  const token = await pageLink(1);
  r = await get("/d/" + token.slice(0, -2) + "AA");
  const expired = await r.text();
  save("expired", expired);
  assert.equal(r.status, 403);
  assert.ok(expired.includes("Link expired"));
});

test("root page gives nothing away; fonts are served locally", async () => {
  const root = await (await get("/")).text();
  save("root", root);
  assert.ok(!/proxy|token|single-use|\/d\//i.test(root), "root is a placeholder");

  for (const name of ["grotesk", "mono", "mono-bold"]) {
    const r = await get(`/f/${name}.woff2`);
    assert.equal(r.headers.get("content-type"), "font/woff2");
    assert.equal(new TextDecoder().decode((await r.arrayBuffer()).slice(0, 4)), "wOF2");
  }
  assert.equal((await get("/f/nope.woff2")).status, 404);
});

test("nothing leaks: no key or CDN URL in pages, links, errors or crashes", async () => {
  const html = await (await get("/d/" + (await pageLink(1)))).text();
  assert.ok(!html.includes(KEY) && !html.includes("cdn.example"));
  for (const [, token] of html.matchAll(/href="\/d\/([^"?]+)"/g)) {
    const claims = await open(token);
    assert.ok(!claims.includes(KEY) && !claims.includes("cdn.example"), claims);
  }
  const fileLink = rowsOf(html).find((x) => x.href.startsWith("/d/")).href;

  // TorBox echoing the key back in an error.
  upstream = (url) =>
    new URL(url).pathname.endsWith("/requestdl")
      ? Response.json({ success: false, detail: `bad token=${KEY} for ${url}` }, { status: 400 })
      : fakeFetch(url);
  let body = await (await get(fileLink)).text();
  assert.ok(!body.includes(KEY), body);

  // A crash whose message carries the full requestdl URL, key included.
  upstream = (url) => {
    if (new URL(url).pathname.endsWith("/requestdl")) throw new TypeError("Fetch API cannot load: " + url);
    return fakeFetch(url);
  };
  const r = await get(fileLink);
  body = await r.text();
  assert.equal(r.status, 502);
  assert.ok(!body.includes(KEY) && !body.includes("token="), body);
  upstream = undefined;
});
