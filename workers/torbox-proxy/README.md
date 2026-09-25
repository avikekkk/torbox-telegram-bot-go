# TorBot Cloudflare Worker proxy (free)

Hides TorBox CDN / WebDAV URLs behind your free `*.workers.dev` link — same idea as a GDrive CF index.

## Modes

| Mode | Token | Worker needs | Best for |
|------|--------|--------------|----------|
| **webdav** | path(s) | `TORBOX_API_KEY` | Single files from library |
| **api** | kind + id (+ zip) | `TORBOX_API_KEY` | Zip packages / reliable ids |
| **cdn** | short-lived CDN url | secret only | Multi-user keys (bot already did requestdl) |
| **list** | kind + id | `TORBOX_API_KEY` | File page for channel posts and `/dl`: pick one file or download all as a zip. A single-file download streams directly |

Default bot setting: `PROXY_MODE=webdav` (falls back to api for zips, then cdn).

## Deploy (free)

1. Install Node.js, then:

```bash
npm install -g wrangler
wrangler login
cd workers/torbox-proxy
```

2. Put secrets (same secret as the bot):

```bash
# Long random string (16+ chars) — copy into bot .env as PROXY_SECRET
# openssl rand -base64 32
wrangler secret put PROXY_SECRET

# Your TorBox API key (for webdav + api modes)
wrangler secret put TORBOX_API_KEY
```

Tokens are **AES-GCM encrypted** (not just signed). Old cleartext/HMAC tokens
will stop working after you redeploy both bot and Worker.

3. Deploy:

```bash
wrangler deploy
```

Note the URL, e.g. `https://torbot-dl.yourname.workers.dev`

4. Bot `.env`:

```env
PROXY_BASE_URL=https://torbot-dl.yourname.workers.dev
PROXY_SECRET=same_as_worker_secret
PROXY_MODE=webdav
# If you enabled "flatten" in TorBox WebDAV settings:
# PROXY_WEBDAV_FLATTEN=1
```

5. Restart TorBot. `/dl` links become `https://…workers.dev/d/…` only.

## Test

```text
https://torbot-dl.yourname.workers.dev/health   → ok
https://torbot-dl.yourname.workers.dev/         → info page
```

## Security features

- AES-GCM encrypted tokens (CDN URLs not readable from the link)
- Short TTL (bot defaults: 15m CDN / 1h webdav)
- **Single-use** via Cache API (or durable **KV** binding `TOKEN_KV`)
- Per-IP rate limit (~30 req/min)
- SSRF blocks for private IPs / metadata hosts
- `no-store` / `nosniff` / `referrer-policy: no-referrer` on responses

Optional durable single-use:

```bash
npx wrangler kv namespace create TORBOT_JTI
# add binding TOKEN_KV in wrangler.toml with the id, then redeploy
```

## Notes

- **WebDAV** refreshes about every 15 minutes on TorBox; new files may need a moment ([refresh](https://webdav.torbox.app/refresh/)).
- **Zip** whole downloads wrap the CDN URL (encrypted), not WebDAV.
- Free Worker limits apply; fine for personal / light use.
- Do not commit secrets. Redeploy bot + Worker together after secret changes.

## Files

- `worker.js`: the Worker.
- `fonts.js`: Space Grotesk and Space Mono (SIL Open Font License), served by the Worker from
  `/f/`, so the file pages load nothing from third parties. Deploy it together with `worker.js`.
- `test/worker.test.mjs`: tests against a fake TorBox, run with `npm test` (Node 22+).
