# TorBot

Telegram bot for adding and monitoring TorBox torrent, Usenet, and web downloads.

Written in Go on [gotd/td](https://github.com/gotd/td) (MTProto), with persistent authorization,
a live status view, NZBHydra2 search, a download channel, and host stats. SQLite is the pure-Go
`modernc.org/sqlite`, so there is nothing to link against and no cgo.

TorBot does not search for torrents. Users supply a magnet/hash, `.torrent` file, `.nzb` file, or
web URL. Usenet search goes through the operator's own NZBHydra2 instance (optional).

## Prerequisites

- Go 1.26+
- Telegram API credentials (`api_id` / `api_hash`) from https://my.telegram.org/apps and a bot token
- A TorBox API key
- Optional: a Cloudflare account and Node.js for the download proxy in `workers/torbox-proxy`

## Setup

1. Create `.env` from the template and fill in the values.
```bash
cp .env.example .env
```

```env
TELEGRAM_API_ID=123456
TELEGRAM_API_HASH=your_api_hash_here
TELEGRAM_BOT_TOKEN=your_bot_token_here
OWNER_ID=123456789
AUTHORIZED_CHAT_IDS=-1001234567890,987654321

TORBOX_API_KEY=your_torbox_api_key
```

Notes:
- `TELEGRAM_API_ID`, `TELEGRAM_API_HASH`, `TELEGRAM_BOT_TOKEN`, `OWNER_ID`, and `TORBOX_API_KEY`
  are required; the rest have defaults. `.env.example` documents every setting.
- `AUTHORIZED_CHAT_IDS` is an optional comma-separated list of user or group IDs (Bot API style,
  e.g. `-1001234567890`) allowed to use the bot. A user ID there works in that user's private chat.
- `DATABASE_PATH` (default `torbot.db`) holds IDs authorized with `/auth` and the channel post
  queue. Both survive a restart.
- Logs go to `logs/torbot.log` as well as the console, starting fresh on every run.
- Switches such as `PROXY_SINGLE_USE` take `1` or `0`; anything else stops startup with a
  `Configuration error:` line naming the setting.

## Run

```bash
./start.sh
```

`start.sh` builds `bin/torbot` and runs it in the foreground. Press Ctrl+C to stop; commands still
running get a few seconds to report their final state.

Or build and run manually:

```bash
go build -o bin/torbot ./cmd/torbot
./bin/torbot
```

The bot keeps its Telegram session in memory and re-authenticates from the bot token on every
start, so no session file is written.

## Commands

| Command | Description |
|---|---|
| `/start`, `/help` | Command reference |
| `/torrent <magnet-or-hash>` | Add a magnet or info hash, or reply to a magnet |
| `/torrent` with `.torrent` | Upload a torrent file, by reply or as its caption |
| `/nzb <nzb-id> [nzb-id...]` | Add search results by NZB ID (up to 10) |
| `/nzb` with `.nzb` | Upload an NZB file, by reply or as its caption |
| `/nzbsearch <query> [--mx\|--mn]` | Search NZBHydra2, paged, with a Telegraph mirror |
| `/web <url>` | Add a supported hoster or direct URL |
| `/dl [t\|u\|w] <id> [file-id]` | Download link; the type is detected when omitted |
| `/status` | Live active downloads, refreshed every three seconds |
| `/server` | TorBox plan plus host uptime, disk, CPU, and RAM |
| `/purge` | Owner only. Permanently delete all TorBox content, after confirmation (private chat) |
| `/logs` | Owner only. Uploads the current log file |
| `/auth [id]`, `/unauth [id]` | Owner only. Target is the replied-to user, an explicit ID, or the current chat |

Every successful add (`/torrent`, `/nzb`, `/web`) opens a live `/status` right after its
confirmation, as the NZBGet bot does. Download IDs come from `/status` or the TorBox dashboard. NZB IDs use the same encoding as the
NZBGet usenet bot, so IDs copied from either bot work with `/nzb`.

Only the requester can page or close a search result, and only the owner who asked can confirm a
`/purge`.

The `/` menu is registered on startup. To set it through BotFather instead, send `/setcommands`
and paste:

```text
help - Show the bot commands
torrent - [magnet or hash] add a torrent, or reply to a .torrent file
nzb - [ID-1] [ID-2]... add NZBs from search results, or reply to a .nzb file
nzbsearch - [query] search NZBs. Flags: --mx largest first, --mn smallest first
web - [url] add a hoster or direct URL
dl - [id] get a download link
status - Live download progress
server - TorBox and system stats
purge - Delete all TorBox content (admin only)
logs - Bot log file (admin only)
auth - [id] authorize a user or chat (admin only)
unauth - [id] remove authorization (admin only)
```

## Authorization

- Access is granted if any one applies: owner, ID in `AUTHORIZED_CHAT_IDS`, or ID authorized at
  runtime with `/auth` (by chat or by user).
- `/purge`, `/logs`, `/auth` and `/unauth` are owner-only.
- IDs in `AUTHORIZED_CHAT_IDS` live in `.env` and cannot be revoked with `/unauth`.
- Anyone else gets `Unauthorized`.

## Groups and private chats

Every command works the same in an authorized group as in an authorized private chat, except
`/purge`, which the owner runs one-to-one. `/dl` answers where it was asked; behind the proxy its
link is a Worker link, never a TorBox CDN URL.

## Download channel

Add the bot to the target channel as an administrator allowed to post, then set:

```env
DOWNLOAD_CHANNEL_ID=-1004452601845
```

Every torrent, Usenet and web download added through the bot is stored and watched. Cached downloads are posted as soon as a
link exists; others when they finish. The channel only ever gets Worker links, so it needs the
proxy below.

```text
File.Name.2026.1080p - [42]

SUCCESS • 8.45 GB • DL
```

The name is monospace and `DL` is the Worker link. Failed downloads show `FAILED` instead. A
download deleted before it finishes is dropped after five minutes.

## NZB search (NZBHydra2)

`/nzbsearch` and `/nzb` with NZB IDs use NZBHydra2's internal API. Results reach TorBox through the TorBox
downloader configured in Hydra; the bot then finds the new TorBox download so `/dl` and channel
posts keep working.

```env
# Hydra login as basic auth in the URL; a trailing /api is accepted
NZBHYDRA_URL=https://user:password@hydra.example.com
# Downloader name exactly as configured in Hydra (Config -> Downloaders)
NZBHYDRA_DOWNLOADER_NAME=TorBox
NZBSEARCH_RESULTS_PER_PAGE=5
# Redact results (message + Telegraph page) after N seconds; 0 = only on CLOSE
NZBSEARCH_AUTOREDACT=0
```

## Cloudflare download proxy

The optional Worker hides TorBox CDN and WebDAV URLs behind your Worker domain.

```bash
cd workers/torbox-proxy
npx wrangler login
npx wrangler secret put PROXY_SECRET
npx wrangler secret put TORBOX_API_KEY
npx wrangler deploy
```

Configure the bot with the same secret:

```env
PROXY_BASE_URL=https://your-worker.workers.dev
PROXY_SECRET=the_same_long_secret
PROXY_MODE=webdav
PROXY_SINGLE_USE=1
```

Tokens are unchanged from the Python bot, so an already deployed Worker keeps working. See
`workers/torbox-proxy/README.md` for details.

## Rate limits

TorBox limits are applied per API key before TorBox has to enforce them: about 4 requests a
second, a soft stop at 55 creates an hour per kind (TorBox allows 60), a 2.5 second gap between
one user's adds, and retries with backoff on 429 and 5xx. Telegram flood waits of up to 30 seconds
are sat out and retried. See `docs/TORBOX_COMPLIANCE.md`.

## Tests

```bash
go test ./...
```

HTTP tests run against local fake TorBox and NZBHydra servers.

## Project structure

- `cmd/torbot/main.go` : Entry point, logging, signal handling.
- `internal/config/config.go` : Environment-based configuration and validation.
- `internal/bot/bot.go` : Client setup, command routing, authorization, help text.
- `internal/bot/add.go` : `/torrent`, `/nzb`, `/web`.
- `internal/bot/dl.go` : `/dl` and link delivery.
- `internal/bot/status.go` : Live `/status`.
- `internal/bot/purge.go` : `/purge` confirmation and progress.
- `internal/bot/search.go`, `nzb.go` : `/nzbsearch`, `/nzb`, Telegraph mirror, redaction.
- `internal/bot/channel.go` : Download channel publisher.
- `internal/bot/admin.go`, `stats.go` : `/logs`, `/auth`, `/unauth`, `/server`.
- `internal/torbox` : TorBox API client, rate limits, download model.
- `internal/nzbhydra` : NZBHydra2 internal API client.
- `internal/proxy` : Encrypted Worker links.
- `internal/store/store.go` : SQLite authorizations and channel queue.
- `workers/torbox-proxy` : The Cloudflare Worker.

## Safety

Operators and TorBox account holders remain responsible for lawful use and compliance with TorBox
terms. Keep `.env` out of git; it holds the bot token and the TorBox API key.
