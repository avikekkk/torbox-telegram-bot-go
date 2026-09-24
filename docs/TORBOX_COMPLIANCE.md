# TorBot: TorBox compliance notes

This bot is a **third-party client**. Operators and end users must follow TorBox’s
own policies. Source documents:

| Document | URL |
|----------|-----|
| Terms of Service | https://torbox.app/terms |
| API documentation | https://api-docs.torbox.app/ |
| API rate limits | https://support.torbox.app/en/articles/13726368-api-rate-limits |
| Abuse / fair use | https://support.torbox.app/en/articles/10336778-the-torbox-abuse-system |
| Privacy | https://torbox.app/privacy |

## How TorBot aligns

### Rate limits (do not bypass)

Documented limits (per **API key**):

- Most endpoints: **300/min**
- `POST …/createtorrent`: **60/hour** uncached (300/min if cached)
- `POST …/createusenetdownload`: **60/hour**
- `POST …/createwebdownload`: **60/hour**

TorBot:

- Soft-stops create calls before hard 60/hour (`internal/torbox/limits.go`)
- Token buckets + concurrency caps + 429 retries with backoff (`internal/torbox/torbox.go`)
- No TorBox search endpoints are used; optional NZB search goes to the operator's own NZBHydra2
- Per-user command cooldowns reduce stampeding

### Fair usage / abuse

TorBox flags automated mass transfer, cache-building for others, and mass key sharing.

TorBot:

- Creates only on explicit `/torrent`, `/nzb` (file or search result IDs), or `/web` actions
- NZBHydra adds count toward the same per-user cooldown and usenet create quota
- Operators control access through `OWNER_ID`, `AUTHORIZED_CHAT_IDS` and `/auth`; `/purge` is owner-only
- Status uses TorBox list endpoints only to inspect active tasks

### Private access links

ToS prohibits **sharing private access links** to content on a TorBox account.

TorBot:

- Delivers links to the requesting user for their own key
- Shows Cloudflare Worker links rather than TorBox CDN URLs when the proxy is configured
- Does not publish permanent public indexes of CDN URLs

### Prohibited content & age

ToS prohibits CSAM, adult content (per their list), malware, and illegal content; service is **18+**.

TorBot:

- Does not scan content; **account holders** remain responsible for what they add

### What operators must still do

1. Restrict access to trusted users with legitimate rights to the content they add.
2. Do not market the bot as a way to resell TorBox access.
3. Protect the configured TorBox API key and private download links.
4. Re-read TorBox terms when they update.

This file is guidance for software design, not legal advice.
