// Package bot implements the Telegram front end: command routing,
// authorization, TorBox adds, live status, NZB search, and the download channel.
package bot

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/html"
	messagepeer "github.com/gotd/td/telegram/message/peer"
	"github.com/gotd/td/telegram/message/unpack"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/avikekkk/torbox-telegram-bot-go/internal/config"
	"github.com/avikekkk/torbox-telegram-bot-go/internal/nzbhydra"
	"github.com/avikekkk/torbox-telegram-bot-go/internal/proxy"
	"github.com/avikekkk/torbox-telegram-bot-go/internal/store"
	"github.com/avikekkk/torbox-telegram-bot-go/internal/telegraph"
	"github.com/avikekkk/torbox-telegram-bot-go/internal/torbox"
)

const helpText = "<u><b>BOT COMMANDS</b></u>\n\n" +
	"<b>✦ To add a torrent:</b>\n\n" +
	"<code>/torrent [magnet or hash]</code>\n\n" +
	"Or reply to a <code>.torrent</code> file with <code>/torrent</code>. " +
	"You can also use <code>/torrent</code> as the file caption.\n\n" +
	"<b>✦ To add an NZB:</b>\n\n" +
	"<code>/nzb [ID-1] [ID-2]...</code>\n\n" +
	"Use the NZB IDs shown under each search result, separated by spaces, commas or new lines. " +
	"Or reply to a <code>.nzb</code> file with <code>/nzb</code>, or use <code>/nzb</code> as its caption.\n\n" +
	"<b>✦ To search for NZBs:</b>\n\n" +
	"<code>/nzbsearch [query]</code>\n\n" +
	"The flags <code>--mx</code> and <code>--mn</code> are optional and must be the last word of a <code>/nzbsearch</code>.\n\n" +
	"<code>  --mx    [Largest first]\n" +
	"  --mn    [Smallest first]</code>\n\n" +
	"<b>✦ To add a web download:</b>\n\n" +
	"<code>/web [url]</code>\n\n" +
	"<b>✦ To view the status:</b>\n\n" +
	"<code>/server    [system stats]\n" +
	"/status    [download progress]</code>\n\n" +
	"<b>✦ Admin only:</b>\n\n" +
	"<code>/purge           [delete all TorBox content]\n" +
	"/logs            [bot log file]\n" +
	"/auth [id]       [authorize a user or chat]\n" +
	"/unauth [id]     [remove authorization]</code>"

// botCommands fills the "/" menu in Telegram clients. Admin commands stay out
// of it; non-admins are rejected either way.
var botCommands = []tg.BotCommand{
	{Command: "help", Description: "Show the bot commands"},
	{Command: "torrent", Description: "Add magnet, hash, or .torrent file"},
	{Command: "nzb", Description: "Add NZBs by NZB ID, or a .nzb file"},
	{Command: "nzbsearch", Description: "Search NZBs via NZBHydra"},
	{Command: "web", Description: "Debrid hoster / direct URL"},
	{Command: "status", Description: "Live active tasks"},
	{Command: "server", Description: "System stats"},
}

// Bot holds everything the update handlers need.
type Bot struct {
	cfg    *config.Bot
	log    *slog.Logger
	sender *message.Sender
	api    *tg.Client

	torbox *torbox.Client
	// hydra is nil when NZBHYDRA_URL is not set.
	hydra *nzbhydra.Client
	// links is nil when the Worker proxy is not configured.
	links *proxy.Builder

	// runCtx outlives a single update, so long commands are not cancelled when
	// update processing returns. It ends when the process is asked to stop.
	runCtx context.Context

	// reportCtx stays alive for a grace period after runCtx ends, so a handler
	// interrupted by shutdown can still tell the user what happened.
	reportCtx context.Context

	// ready is closed once the Telegram session is fully set up. Updates can
	// arrive before then, and must not run against half-built plumbing.
	ready chan struct{}

	// username is this bot's own @name, so a command addressed to another bot
	// in a group can be left alone.
	username string

	store     *store.Store
	startedAt time.Time
	cooldowns *cooldowns
	channel   *channelPublisher

	// handlers tracks in-flight commands; stopping refuses new ones once
	// shutdown has begun, so Wait is never raced by a late Add.
	handlers sync.WaitGroup
	stopMu   sync.Mutex
	stopping bool

	mu       sync.Mutex
	statuses map[int64]*statusSession // chat ID -> live /status message
	purges   map[string]*purgeSession
	// Search sessions are capped, and searchOrder gives the map an insertion
	// order to evict by.
	searches    map[string]*searchSession
	searchOrder []string

	// One Telegraph account is shared by every search; see telegraphAccount.
	telegraphMu sync.Mutex
	telegraph   *telegraph.Client
}

// shutdownGrace bounds how long a stop waits for in-flight commands to report
// their final state before the Telegram connection is dropped.
const shutdownGrace = 15 * time.Second

// reportTimeout bounds one final send or edit made after shutdown has begun.
const reportTimeout = 10 * time.Second

// Run connects to Telegram as a bot and serves updates until ctx is cancelled.
func Run(ctx context.Context, cfg *config.Bot, logger *slog.Logger) error {
	db, err := store.Open(cfg.DatabasePath)
	if err != nil {
		return err
	}
	defer db.Close()
	logger.Info("State stored in " + cfg.DatabasePath)

	dispatcher := tg.NewUpdateDispatcher()
	b := &Bot{
		cfg:       cfg,
		log:       logger,
		torbox:    torbox.New(cfg.TorBoxAPIKey, "", cfg.HTTPTimeout),
		links:     proxy.New(cfg),
		store:     db,
		startedAt: time.Now(),
		cooldowns: newCooldowns(),
		ready:     make(chan struct{}),
		statuses:  map[int64]*statusSession{},
		purges:    map[string]*purgeSession{},
		searches:  map[string]*searchSession{},
	}
	if cfg.NZBHydraEnabled() {
		// A search fans out to every indexer Hydra has, which can take a while.
		b.hydra = nzbhydra.New(cfg.NZBHydraURL, max(90*time.Second, cfg.HTTPTimeout))
	}
	b.channel = newChannelPublisher(b)

	dispatcher.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error {
		if !b.waitReady(ctx) {
			return nil
		}
		return b.onMessage(e, u)
	})
	dispatcher.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewChannelMessage) error {
		if !b.waitReady(ctx) {
			return nil
		}
		return b.onMessage(e, u)
	})
	dispatcher.OnBotCallbackQuery(func(ctx context.Context, e tg.Entities, u *tg.UpdateBotCallbackQuery) error {
		if !b.waitReady(ctx) {
			return nil
		}
		return b.onCallbackQuery(e, u)
	})

	client := telegram.NewClient(cfg.TelegramAPIID, cfg.TelegramAPIHash, telegram.Options{
		UpdateHandler: dispatcher,
		Middlewares:   []telegram.Middleware{floodWaiter(logger)},
		// Bots re-authenticate from their token on every start, so an
		// in-memory session is enough and leaves no session file behind.
		SessionStorage: &session.StorageMemory{},
	})
	b.api = tg.NewClient(client)
	b.sender = message.NewSender(b.api)

	// The connection is deliberately not tied to the stop signal: it has to
	// outlive it long enough for interrupted commands to report themselves.
	// The callback below returns, and so ends the connection, once they have.
	connCtx, disconnect := context.WithCancel(context.Background())
	defer disconnect()

	return client.Run(connCtx, func(connCtx context.Context) error {
		status, err := client.Auth().Status(connCtx)
		if err != nil {
			return err
		}
		if !status.Authorized {
			if _, err := client.Auth().Bot(connCtx, cfg.TelegramBotToken); err != nil {
				return err
			}
		}

		self, err := client.Self(connCtx)
		if err != nil {
			return err
		}
		b.username = self.Username
		b.runCtx = ctx
		b.reportCtx = connCtx
		close(b.ready)
		logger.Info("Telegram client initialised", "bot", "@"+self.Username,
			"proxy", b.links != nil, "nzbhydra", b.hydra != nil, "channel", cfg.ChannelEnabled())

		b.setCommands(connCtx)
		b.channel.start(ctx)

		select {
		case <-ctx.Done():
		case <-connCtx.Done():
			return connCtx.Err()
		}

		// Refuse new commands, then give in-flight ones a moment to report
		// their final state before the connection goes away.
		b.stopMu.Lock()
		b.stopping = true
		b.stopMu.Unlock()

		done := make(chan struct{})
		go func() {
			b.handlers.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(shutdownGrace):
			logger.Warn("Shutdown grace period elapsed with commands still running")
		}
		return ctx.Err()
	})
}

// setCommands registers the "/" menu, so it needs no BotFather step.
func (b *Bot) setCommands(ctx context.Context) {
	_, err := b.api.BotsSetBotCommands(ctx, &tg.BotsSetBotCommandsRequest{
		Scope:    &tg.BotCommandScopeDefault{},
		Commands: botCommands,
	})
	if err != nil {
		b.log.Warn("Failed to set bot commands; the slash menu may be empty", "err", err)
	}
}

// waitReady blocks an update until the session is set up. It reports false if
// the update was abandoned first.
func (b *Bot) waitReady(ctx context.Context) bool {
	select {
	case <-b.ready:
		return true
	case <-ctx.Done():
		return false
	}
}

// goHandle runs a handler off the update loop so a slow TorBox call does not
// block further updates.
func (b *Bot) goHandle(name string, fn func(ctx context.Context)) {
	b.stopMu.Lock()
	if b.stopping {
		b.stopMu.Unlock()
		b.log.Info("Ignoring update during shutdown", "what", name)
		return
	}
	b.handlers.Add(1)
	b.stopMu.Unlock()
	go func() {
		defer b.handlers.Done()
		defer b.recoverPanic(name)
		fn(b.runCtx)
	}()
}

func (b *Bot) recoverPanic(where string) {
	if r := recover(); r != nil {
		b.log.Error("Recovered from panic", "where", where, "panic", r)
	}
}

// access is who may run a command.
type access int

const (
	// authorized: the owner, an allowlisted chat, or an ID granted with /auth.
	authorized access = iota
	ownerOnly
)

// runCommand checks access, runs a command handler through goHandle and, once
// it returns, logs one summary line. lag is how long the command sat between
// being typed and reaching the handler, which is the half of a slow command
// this bot does not control; without it a delayed delivery and a slow handler
// look alike.
func (b *Bot) runCommand(command string, req *request, level access, fn func(ctx context.Context)) {
	b.goHandle(command, func(ctx context.Context) {
		if req.reject(ctx, command, level) {
			return
		}
		start := time.Now()
		lag := start.Sub(time.Unix(int64(req.msg.Date), 0))
		ok := false
		defer func() {
			b.log.Info("Command handled", "command", command, "chat_id", req.chatID(),
				"user_id", req.userID(), "lag", lag.Round(time.Millisecond),
				"took", time.Since(start).Round(time.Millisecond), "ok", ok)
		}()
		fn(ctx)
		ok = true
	})
}

// messageUpdate is satisfied by both private/basic-group and channel updates.
type messageUpdate interface {
	message.AnswerableMessageUpdate
}

func (b *Bot) onMessage(e tg.Entities, u messageUpdate) error {
	msg, ok := u.GetMessage().(*tg.Message)
	if !ok || msg.Out {
		return nil
	}

	command, args, payload, ok := b.parseCommand(msg.Message)
	req := &request{bot: b, entities: e, update: u, msg: msg, args: args, payload: payload}
	if req.userID() == 0 {
		return nil
	}
	if !ok {
		// A bare .torrent or .nzb gets a nudge towards the command that adds it.
		if _, name, found := documentOf(msg); found && (isTorrentFile(name) || isNZBFile(name)) {
			b.goHandle("document hint", req.handleDocumentHint)
		}
		return nil
	}

	switch command {
	case "start", "help":
		b.runCommand(command, req, authorized, req.handleHelp)
	case "torrent":
		b.runCommand(command, req, authorized, req.handleTorrent)
	case "nzb":
		b.runCommand(command, req, authorized, req.handleNZB)
	case "web":
		b.runCommand(command, req, authorized, req.handleWeb)
	case "status":
		b.runCommand(command, req, authorized, req.handleStatus)
	case "nzbsearch":
		b.runCommand(command, req, authorized, req.handleSearch)
	case "server":
		b.runCommand(command, req, authorized, req.handleServer)
	case "purge":
		b.runCommand(command, req, ownerOnly, req.handlePurge)
	case "logs":
		b.runCommand(command, req, ownerOnly, req.handleLogs)
	case "auth":
		b.runCommand(command, req, ownerOnly, req.handleAuth)
	case "unauth":
		b.runCommand(command, req, ownerOnly, req.handleUnauth)
	}
	return nil
}

// parseCommand splits "/cmd@bot arg1 arg2" into its parts. A command addressed
// to a different bot is not ours to answer: several bots usually share a group,
// and "@name" is how a user picks between them.
func (b *Bot) parseCommand(text string) (command string, args []string, payload string, ok bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", nil, "", false
	}

	fields := strings.Fields(text)
	if len(fields) == 0 {
		return "", nil, "", false
	}

	command = strings.TrimPrefix(fields[0], "/")
	if name, target, found := strings.Cut(command, "@"); found {
		if !strings.EqualFold(target, b.username) {
			return "", nil, "", false
		}
		command = name
	}
	command = strings.ToLower(command)
	if command == "" {
		return "", nil, "", false
	}

	args = fields[1:]
	// Everything after the command word, whatever whitespace separated them:
	// "/nzb\n<id>\n<id>" is how a list of IDs pastes out of a search.
	payload = strings.TrimSpace(text[len(fields[0]):])
	return command, args, payload, true
}

// request bundles one command invocation with the plumbing needed to answer it.
type request struct {
	bot      *Bot
	entities tg.Entities
	update   messageUpdate
	msg      *tg.Message
	args     []string
	payload  string

	// replied caches the message this command replied to, fetched on demand.
	replied      *tg.Message
	repliedErr   error
	repliedFetch bool
}

func (r *request) userID() int64 {
	if from, ok := r.msg.FromID.(*tg.PeerUser); ok {
		return from.UserID
	}
	// In private chats the sender is implied by the peer.
	if peer, ok := r.msg.PeerID.(*tg.PeerUser); ok {
		return peer.UserID
	}
	return 0
}

// chatID is rendered in the Bot API numbering scheme so AUTHORIZED_CHAT_IDS
// values copied from other bots keep working.
func (r *request) chatID() int64 { return chatID(r.msg.PeerID) }

func chatID(p tg.PeerClass) int64 {
	switch peer := p.(type) {
	case *tg.PeerUser:
		return peer.UserID
	case *tg.PeerChat:
		return -peer.ChatID
	case *tg.PeerChannel:
		return -1000000000000 - peer.ChannelID
	}
	return 0
}

// private reports whether the command was sent in a one-to-one chat.
func (r *request) private() bool {
	_, ok := r.msg.PeerID.(*tg.PeerUser)
	return ok
}

func (r *request) firstName() string {
	if user, ok := r.entities.Users[r.userID()]; ok {
		return user.FirstName
	}
	return ""
}

func (r *request) isOwner() bool { return r.userID() == r.bot.cfg.OwnerID }

// isAuthorized allows the owner, the static .env allowlist, and anything
// granted at runtime with /auth, by either chat or user ID.
func (r *request) isAuthorized() bool {
	if r.isOwner() || r.bot.cfg.IsAuthorizedChat(r.chatID()) {
		return true
	}
	authorized, err := r.bot.store.IsAuthorized(r.chatID(), r.userID())
	if err != nil {
		r.bot.log.Error("Failed to check authorization", "err", err)
		return false
	}
	return authorized
}

// reject answers and reports true when the sender may not run the command.
func (r *request) reject(ctx context.Context, command string, level access) bool {
	allowed := r.isOwner()
	if level == authorized {
		allowed = r.isAuthorized()
	}
	if allowed {
		return false
	}
	r.bot.log.Warn("Unauthorized command rejected", "command", command, "chat_id", r.chatID(), "user_id", r.userID())
	if _, err := r.reply(ctx, "<code>Unauthorized</code>"); err != nil {
		r.bot.log.Warn("Failed to send rejection", "err", err)
	}
	return true
}

// privateOnlyHint answers a command that only works one-to-one.
const privateOnlyHint = "Use this command in a private chat with the bot."

// rejectGroup answers and reports true when the command was sent in a group.
func (r *request) rejectGroup(ctx context.Context) bool {
	if r.private() {
		return false
	}
	r.replyLogged(ctx, privateOnlyHint)
	return true
}

// peer resolves the chat this command came from.
func (r *request) peer() (tg.InputPeerClass, error) {
	return messagepeer.EntitiesFromUpdate(r.entities).ExtractPeer(r.msg.PeerID)
}

// reply posts an HTML reply and returns the new message ID.
func (r *request) reply(ctx context.Context, text string) (int, error) {
	return r.replyMarkup(ctx, text, nil)
}

// replyMarkup posts an HTML reply with an inline keyboard.
func (r *request) replyMarkup(ctx context.Context, text string, markup tg.ReplyMarkupClass) (int, error) {
	ctx, cancel := r.bot.reporting(ctx)
	defer cancel()
	builder := r.bot.sender.Answer(r.entities, r.update).NoWebpage().ReplyMsg(r.msg)
	if markup != nil {
		builder = builder.Markup(markup)
	}
	return unpack.MessageID(builder.StyledText(ctx, html.String(nil, text)))
}

// edit replaces the text of a message this command previously sent.
func (r *request) edit(ctx context.Context, msgID int, text string, markup tg.ReplyMarkupClass) error {
	target, err := r.peer()
	if err != nil {
		return err
	}
	return r.bot.edit(ctx, target, msgID, text, markup)
}

// replyLogged sends a reply, logging rather than propagating a send failure.
func (r *request) replyLogged(ctx context.Context, text string) {
	if _, err := r.reply(ctx, text); err != nil {
		r.bot.log.Warn("Failed to send reply", "err", err)
	}
}

// editLogged edits a message, treating a no-op edit as success.
func (r *request) editLogged(ctx context.Context, msgID int, text string) {
	if err := r.edit(ctx, msgID, text, nil); err != nil && !isNotModified(err) {
		r.bot.log.Warn("Failed to edit message", "err", err)
	}
}

// deleteLogged removes a message this command sent, such as an "Adding" notice.
func (r *request) deleteLogged(ctx context.Context, msgID int) {
	target, err := r.peer()
	if err == nil {
		err = r.bot.delete(ctx, target, msgID)
	}
	if err != nil {
		r.bot.log.Debug("Could not delete notice", "err", err)
	}
}

// edit replaces a message's text and keyboard. A nil markup drops the old
// keyboard: sending an empty ReplyInlineMarkup instead is rejected outright
// with REPLY_MARKUP_INVALID.
func (b *Bot) edit(ctx context.Context, target tg.InputPeerClass, msgID int, text string, markup tg.ReplyMarkupClass) error {
	ctx, cancel := b.reporting(ctx)
	defer cancel()
	builder := b.sender.To(target).NoWebpage()
	if markup != nil {
		builder = builder.Markup(markup)
	}
	_, err := builder.Edit(msgID).StyledText(ctx, html.String(nil, text))
	return err
}

func (b *Bot) delete(ctx context.Context, target tg.InputPeerClass, msgID int) error {
	ctx, cancel := b.reporting(ctx)
	defer cancel()
	_, err := b.sender.To(target).Revoke().Messages(ctx, msgID)
	return err
}

// send posts a new HTML message to target.
func (b *Bot) send(ctx context.Context, target tg.InputPeerClass, text string) error {
	ctx, cancel := b.reporting(ctx)
	defer cancel()
	_, err := b.sender.To(target).NoWebpage().StyledText(ctx, html.String(nil, text))
	return err
}

// reporting returns the context a message send or edit should use. Normally
// that is the caller's own; once shutdown has cancelled it, a bounded context
// on the still-open connection takes over so the final state still reaches
// the user rather than a notice stuck at "Adding torrent".
func (b *Bot) reporting(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil || b.reportCtx == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(b.reportCtx, reportTimeout)
}

// interrupted reports whether the bot is shutting down, which is the one
// reason a cancelled handler should tell the user to try again.
func (b *Bot) interrupted() bool {
	return b.runCtx != nil && b.runCtx.Err() != nil
}

func (r *request) handleHelp(ctx context.Context) {
	r.replyLogged(ctx, helpText)
}

// isNotModified reports the benign error Telegram returns for a no-op edit.
func isNotModified(err error) bool {
	return tgerr.Is(err, "MESSAGE_NOT_MODIFIED")
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
