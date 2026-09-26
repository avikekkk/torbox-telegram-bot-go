# TorBot Cloudflare Worker proxy (free)

Hides TorBox CDN URLs behind your free `*.workers.dev` link.

## Modes

| Mode | Token | Made by | What it does |
|------|--------|---------|--------------|
| **list** | kind + id | the bot | File page for channel posts: pick one file or download all as a zip. A single-file download streams directly |
| **api** | kind + id (+ file or zip) | the Worker, on a file page | Calls TorBox requestdl and streams the result |

Both need `TORBOX_API_KEY` on the Worker.

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

# Your TorBox API key
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
```

5. Restart TorBot. Channel links become `https://…workers.dev/d/…` only.

## Test

```text
https://torbot-dl.yourname.workers.dev/health   → ok
https://torbot-dl.yourname.workers.dev/         → info page
```

## Security features

- AES-GCM encrypted tokens (CDN URLs not readable from the link)
- Expiring links (bot default: 7-day file page; file links on it last 6h)
- Per-IP rate limit (~30 req/min)
- SSRF blocks for private IPs / metadata hosts
- `no-store` / `nosniff` / `referrer-policy: no-referrer` on responses

## Notes

- Free Worker limits apply; fine for personal / light use.
- Do not commit secrets. Redeploy bot + Worker together after secret changes.

## Files

- `worker.js`: the Worker.
- `fonts.js`: Space Grotesk and Space Mono (SIL Open Font License), served by the Worker from
  `/f/`, so the file pages load nothing from third parties. Deploy it together with `worker.js`.
- `test/worker.test.mjs`: tests against a fake TorBox, run with `npm test` (Node 22+).
