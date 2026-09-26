# TorBot

A Telegram bot that adds downloads to TorBox and shows their progress. It works with torrents,
Usenet (NZB) and web downloads.

The bot does not search for torrents. You give it a magnet link, a hash, a `.torrent` file, a
`.nzb` file or a web URL. If you run NZBHydra2, the bot can also search Usenet through it.

## What you need

- Go 1.26 or newer
- Telegram API ID and API hash from https://my.telegram.org/apps
- A bot token from BotFather
- A TorBox API key
- Optional: a Cloudflare account and Node.js, for the download proxy in `workers/torbox-proxy`

## Setup

Copy the example file and fill it in:

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

- These are required: `TELEGRAM_API_ID`, `TELEGRAM_API_HASH`, `TELEGRAM_BOT_TOKEN`, `OWNER_ID`
  and `TORBOX_API_KEY`. Everything else has a default. `.env.example` lists every setting.
- `AUTHORIZED_CHAT_IDS` is optional. It is a comma-separated list of user or group IDs that may
  use the bot. Group IDs look like `-1001234567890`.
- `DATABASE_PATH` (default `torbot.db`) stores users added with `/auth` and the channel post
  queue. Both are kept after a restart.
- Logs are written to the console and to `logs/torbot.log`. The log file starts fresh on each run.

## Run

```bash
./start.sh
```

This builds `bin/torbot` and runs it. Press Ctrl+C to stop.

To build and run it yourself:

```bash
go build -o bin/torbot ./cmd/torbot
./bin/torbot
```

The bot logs in with its token on every start. It does not save a session file.

## Commands

| Command | What it does |
|---|---|
| `/start`, `/help` | Show the list of commands |
| `/torrent <magnet or hash>` | Add a torrent. You can also reply to a message with a magnet |
| `/torrent` with a `.torrent` file | Add a torrent file. Reply to the file, or send it with `/torrent` as the caption |
| `/nzb <nzb-id> [nzb-id...]` | Add up to 10 NZB search results by ID |
| `/nzb` with a `.nzb` file | Add an NZB file. Reply to the file, or send it with `/nzb` as the caption |
| `/nzbsearch <query> [--mx\|--mn]` | Search NZBHydra2. `--mx` shows the largest first, `--mn` the smallest |
| `/web <url>` | Add a file host link or a direct URL |
| `/status` | Show active downloads. Updates every 3 seconds |
| `/server` | Show your TorBox plan and the server's uptime, disk, CPU and RAM |
| `/purge` | Owner only. Delete everything in TorBox, after you confirm. Private chat only |
| `/logs` | Owner only. Send the current log file |
| `/auth [id]`, `/unauth [id]` | Owner only. Allow or remove a user or chat |

- After you add something, the bot opens `/status` so you can watch it.
- Only the person who searched can change pages or close a search result.
- Only the owner can confirm `/purge`.
- `/auth` and `/unauth` work on the user you reply to, an ID you type, or the current chat.

The bot sets its `/` command menu when it starts. To set it in BotFather instead, send
`/setcommands` and paste:

```text
help - Show the bot commands
torrent - [magnet or hash] add a torrent, or reply to a .torrent file
nzb - [ID-1] [ID-2]... add NZBs from search results, or reply to a .nzb file
nzbsearch - [query] search NZBs. Flags: --mx largest first, --mn smallest first
web - [url] add a hoster or direct URL
status - Live download progress
server - TorBox and system stats
purge - Delete all TorBox content (admin only)
logs - Bot log file (admin only)
auth - [id] authorize a user or chat (admin only)
unauth - [id] remove authorization (admin only)
```

## Who can use the bot

- The owner (`OWNER_ID`).
- Any user or chat in `AUTHORIZED_CHAT_IDS`.
- Any user or chat the owner adds with `/auth`.

Everyone else gets `Unauthorized`. IDs in `.env` cannot be removed with `/unauth`; edit `.env`
instead. Commands work the same in groups and private chats, except `/purge`, which only works
in a private chat.

## Download channel

The bot can post finished downloads to a Telegram channel. Make the bot an admin in the channel
with permission to post, then set:

```env
DOWNLOAD_CHANNEL_ID=-1001234567890
```

This needs the Cloudflare proxy (see below), so the channel never shows real TorBox links.

A post looks like this:

```text
File.Name.2026.1080p - [42]

SUCCESS • 8.45 GB • DL
```

- Downloads TorBox already has are posted right away. Others are posted when they finish.
- `DL` opens a page that lists every file in the download. Each file has a download button and a
  copy link button, and there is a "Download all" button for a zip.
- If the download is a single file, `DL` downloads it directly.
- The page link works for 7 days (`PROXY_PAGE_TTL_SECONDS`) and can be opened many times. The
  file links on the page work for 6 hours. Reload the page to get new ones.
- A failed download is posted as `FAILED`.
- If a download is deleted before it finishes, it is skipped. If it is deleted later, its page
  says so.

## NZB search (NZBHydra2)

`/nzbsearch` and `/nzb` with IDs need NZBHydra2. Hydra sends the NZB to TorBox using the TorBox
downloader you set up in Hydra.

```env
# Hydra URL, with the Hydra login in it
NZBHYDRA_URL=https://user:password@hydra.example.com
# The downloader name exactly as it appears in Hydra (Config -> Downloaders)
NZBHYDRA_DOWNLOADER_NAME=TorBox
NZBSEARCH_RESULTS_PER_PAGE=5
# Hide search results after this many seconds. 0 means only when you press CLOSE
NZBSEARCH_AUTOREDACT=0
```

## Cloudflare download proxy

The proxy is a free Cloudflare Worker. It gives out links on your own `workers.dev` address, so
real TorBox links are never shared.

```bash
cd workers/torbox-proxy
npx wrangler login
npx wrangler secret put PROXY_SECRET
npx wrangler secret put TORBOX_API_KEY
npx wrangler deploy
```

Then add the Worker address and the same secret to `.env`:

```env
PROXY_BASE_URL=https://your-worker.workers.dev
PROXY_SECRET=the_same_long_secret
```

`PROXY_SECRET` must be at least 16 characters. More details are in
`workers/torbox-proxy/README.md`.

## Rate limits

The bot stays under TorBox's limits on its own:

- About 4 requests per second.
- At most 55 new downloads per hour of each type (TorBox allows 60).
- A 2.5 second wait between one user's adds.
- Failed requests (429 and 5xx) are retried with a growing delay.

If Telegram asks the bot to wait (up to 30 seconds), it waits and tries again. See
`docs/TORBOX_COMPLIANCE.md` for more.

## Tests

```bash
go test ./...
```

The tests use fake TorBox and NZBHydra servers, so they need no accounts. The Worker has its own
tests: run `npm test` in `workers/torbox-proxy`.

## Project layout

- `cmd/torbot/main.go`: starts the bot, sets up logging, handles Ctrl+C.
- `internal/config`: reads and checks `.env`.
- `internal/bot/bot.go`: Telegram setup, command routing, access checks, help text.
- `internal/bot/add.go`: `/torrent`, `/nzb`, `/web`.
- `internal/bot/status.go`: `/status`.
- `internal/bot/purge.go`: `/purge`.
- `internal/bot/search.go`, `nzb.go`: `/nzbsearch` and `/nzb`.
- `internal/bot/channel.go`: download channel posts.
- `internal/bot/admin.go`, `stats.go`: `/logs`, `/auth`, `/unauth`, `/server`.
- `internal/torbox`: TorBox API client and rate limits.
- `internal/nzbhydra`: NZBHydra2 client.
- `internal/proxy`: builds the encrypted Worker links.
- `internal/store`: SQLite storage for access and the channel queue.
- `workers/torbox-proxy`: the Cloudflare Worker.

## Responsibility

You are responsible for how you use this bot and for following TorBox's terms. Never commit
`.env` to git. It holds your bot token and TorBox API key.
