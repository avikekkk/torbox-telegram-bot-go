package bot

import (
	"context"
	"errors"
	"fmt"
	"html"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/avikekkk/torbox-telegram-bot-go/internal/torbox"
	"github.com/avikekkk/torbox-telegram-bot-go/internal/util"
)

// statusPollInterval is how often a live /status message is refreshed.
const statusPollInterval = 3 * time.Second

// statusTaskLimit is how many downloads a /status message lists.
const statusTaskLimit = 6

// noActiveTasks is the whole /status message once nothing is running.
const noActiveTasks = "No active tasks"

// unknownETA is TorBox's "no estimate" sentinel: 100 days.
const unknownETA = 8_640_000

// statusSession is a live /status message being refreshed in a chat.
type statusSession struct {
	msgID  int
	cancel context.CancelFunc
	// auto marks a status opened by an add rather than typed. Finding nothing
	// on its first look, it removes itself instead of answering a question
	// nobody asked.
	auto bool
}

func (r *request) handleStatus(ctx context.Context) {
	r.runStatus(ctx, false)
}

// runStatus replaces any previous /status message in the chat and then keeps
// the new one up to date until nothing is left running. auto is set when an
// add opened it.
func (r *request) runStatus(ctx context.Context, auto bool) {
	b := r.bot
	target, err := r.peer()
	if err != nil {
		b.log.Warn("Could not resolve the status chat", "err", err)
		return
	}

	b.mu.Lock()
	previous := b.statuses[r.chatID()]
	delete(b.statuses, r.chatID())
	b.mu.Unlock()
	if previous != nil {
		previous.cancel()
		if err := b.delete(ctx, target, previous.msgID); err != nil {
			b.log.Debug("Could not delete previous status message", "err", err)
		}
	}

	msgID, err := r.reply(ctx, "Fetching status")
	if err != nil {
		b.log.Warn("Failed to post status message", "err", err)
		return
	}

	statusCtx, cancel := context.WithCancel(ctx)
	session := &statusSession{msgID: msgID, cancel: cancel, auto: auto}
	b.mu.Lock()
	b.statuses[r.chatID()] = session
	b.mu.Unlock()

	// Deliberately not tracked by the shutdown WaitGroup: it ends with runCtx.
	go func() {
		defer cancel()
		defer b.recoverPanic("status loop")
		b.pollStatus(statusCtx, target, r.chatID(), session)
	}()
}

func (b *Bot) pollStatus(ctx context.Context, target tg.InputPeerClass, chat int64, session *statusSession) {
	defer b.endStatus(chat, session)

	first := true
	editFailures := 0
	for {
		items, err := b.activeDownloads(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			b.reportInterrupted(ctx, target, session.msgID)
			return
		case err != nil && first:
			b.log.Warn("Error while fetching status", "err", err)
			if err := b.edit(ctx, target, session.msgID, b.errorText(err), nil); err != nil {
				b.log.Debug("Could not report status failure", "err", err)
			}
			return
		case err != nil:
			// A TorBox blip mid-way is retried rather than ending a live view.
			b.log.Warn("Live status refresh failed; retrying", "err", err)
		default:
			wasFirst := first
			first = false
			if len(items) == 0 && (!wasFirst || session.auto) {
				// Drained, or an add's status with nothing to watch: drop the
				// message rather than leave a stale "No active tasks" in the
				// chat, as the NZBGet bot does.
				if err := b.delete(ctx, target, session.msgID); err != nil {
					b.log.Debug("Could not delete drained status message", "err", err)
				}
				return
			}
			text := renderStatus(items)
			switch err := b.edit(ctx, target, session.msgID, text, nil); {
			case err == nil, isNotModified(err):
				editFailures = 0
			case tgerr.Is(err, "MESSAGE_ID_INVALID"), tgerr.Is(err, "MESSAGE_DELETE_FORBIDDEN"):
				return
			case errors.Is(err, context.Canceled):
				b.reportInterrupted(ctx, target, session.msgID)
				return
			default:
				// Backstop: give up rather than poll forever on an error we
				// did not anticipate.
				editFailures++
				b.log.Warn("Could not update status message", "err", err)
				if editFailures >= 3 {
					return
				}
			}
			if len(items) == 0 {
				return
			}
		}

		select {
		case <-ctx.Done():
			b.reportInterrupted(ctx, target, session.msgID)
			return
		case <-time.After(statusPollInterval):
		}
	}
}

// reportInterrupted marks a live status message as cut short by a restart.
// A status replaced by a newer /status is cancelled too, but that one is
// deleted by its successor, so only shutdown is worth announcing.
func (b *Bot) reportInterrupted(ctx context.Context, target tg.InputPeerClass, msgID int) {
	if !b.interrupted() {
		return
	}
	if err := b.edit(ctx, target, msgID, errInterrupted, nil); err != nil {
		b.log.Debug("Could not mark status interrupted", "err", err)
	}
}

// endStatus clears the chat's session, unless a newer /status already replaced
// it.
func (b *Bot) endStatus(chat int64, session *statusSession) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.statuses[chat] == session {
		delete(b.statuses, chat)
	}
}

// activeDownloads lists what is still running across all three libraries and
// the central queue. One source failing does not hide the others; only all of
// them failing is an error.
func (b *Bot) activeDownloads(ctx context.Context) ([]torbox.Item, error) {
	type source struct {
		name  string
		items []torbox.Item
		err   error
	}
	sources := []*source{{name: torbox.KindTorrent}, {name: torbox.KindUsenet}, {name: torbox.KindWebDL}, {name: "queued"}}

	var wg sync.WaitGroup
	for _, s := range sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.name == "queued" {
				s.items, s.err = b.torbox.Queued(ctx)
				return
			}
			s.items, s.err = b.torbox.ListAll(ctx, s.name)
		}()
	}
	wg.Wait()

	var active []torbox.Item
	var firstErr error
	failed := 0
	for _, s := range sources {
		if s.err != nil {
			failed++
			if firstErr == nil {
				firstErr = s.err
			}
			if ctx.Err() == nil {
				b.log.Warn("Status source failed", "source", s.name, "err", s.err)
			}
		}
		for _, item := range s.items {
			if item.IsActive() {
				active = append(active, item)
			}
		}
	}
	if failed == len(sources) {
		return nil, firstErr
	}
	return active, nil
}

// phaseOrder is the order downloads are listed in: the ones furthest from done
// first, so what needs attention sits at the top.
var phaseOrder = []string{
	"GRABBING", "QUEUED", "METADATA", "ALLOCATING", "CHECKING", "DOWNLOADING",
	"STALLED", "PAUSED", "VERIFYING", "REPAIRING", "UNPACKING", "MOVING", "PROCESSING",
}

func phaseRank(phase string) int {
	for i, candidate := range phaseOrder {
		if candidate == phase {
			return i
		}
	}
	return len(phaseOrder)
}

// statusPhase names what a download is doing, as the header above it.
func statusPhase(item torbox.Item) string {
	state := strings.ToLower(strings.TrimSpace(item.State))
	normalized := item.NormalizedState()
	switch item.Kind {
	case torbox.KindTorrent:
		switch {
		case strings.Contains(normalized, "stalled"):
			return "STALLED"
		case strings.Contains(normalized, "meta"):
			return "METADATA"
		case strings.Contains(normalized, "paused"):
			return "PAUSED"
		case strings.Contains(normalized, "queued"):
			return "QUEUED"
		case strings.Contains(normalized, "checking"):
			return "CHECKING"
		case strings.Contains(normalized, "allocat"):
			return "ALLOCATING"
		case strings.Contains(normalized, "moving"):
			return "MOVING"
		case strings.Contains(normalized, "process"):
			return "PROCESSING"
		}
		return "DOWNLOADING"
	case torbox.KindWebDL:
		switch {
		case containsAny(normalized, "queued", "pending", "waiting"):
			return "QUEUED"
		case strings.Contains(normalized, "process"):
			return "PROCESSING"
		}
		return "DOWNLOADING"
	}
	switch {
	case strings.Contains(state, "grab"):
		return "GRABBING"
	case strings.Contains(state, "verify"), strings.Contains(state, "check"):
		return "VERIFYING"
	case strings.Contains(state, "repair"):
		return "REPAIRING"
	case strings.Contains(state, "unpack"), strings.Contains(state, "extract"):
		return "UNPACKING"
	case strings.Contains(state, "process"):
		return "PROCESSING"
	}
	return "DOWNLOADING"
}

// kindFlags mark each download with its library: [T]orrent, [U]senet, [W]eb.
var kindFlags = map[string]string{torbox.KindTorrent: "T", torbox.KindUsenet: "U", torbox.KindWebDL: "W"}

// renderStatus lays out the active downloads, one block each.
func renderStatus(items []torbox.Item) string {
	var active []torbox.Item
	for _, item := range items {
		if item.IsActive() {
			active = append(active, item)
		}
	}
	if len(active) == 0 {
		return noActiveTasks
	}

	sort.SliceStable(active, func(i, j int) bool {
		return phaseRank(statusPhase(active[i])) < phaseRank(statusPhase(active[j]))
	})
	shown := active[:min(len(active), statusTaskLimit)]

	blocks := make([]string, 0, len(shown))
	for _, item := range shown {
		blocks = append(blocks, header(statusPhase(item))+"\n"+statusBlock(item))
	}
	text := strings.Join(blocks, "\n\n")
	if extra := len(active) - statusTaskLimit; extra > 0 {
		text += fmt.Sprintf("\n\n<b>+%d more task(s)</b>", extra)
	}
	return text
}

// statusBlock is one download's lines. Telemetry TorBox did not report shows
// as "--", never as a made-up zero.
func statusBlock(item torbox.Item) string {
	name := item.Name
	if name == "" {
		name = "untitled"
	}
	name = clipName(name, 80)

	bar := "[" + strings.Repeat("░", 12) + "] --"
	if item.HasProgress {
		bar = util.ProgressBar(item.Percent())
	}

	total, downloaded := "--", "--"
	if item.Size > 0 {
		total = util.ConvertSize(item.Size)
		switch {
		case item.HasDownloaded:
			downloaded = util.ConvertSize(item.Downloaded)
		case item.HasProgress:
			downloaded = util.ConvertSize(float64(item.Size) * item.Percent() / 100)
		}
	}

	speed := "--"
	if item.Speed > 0 {
		speed = util.ConvertSize(item.Speed) + "/s"
	}

	eta := "--"
	if item.HasETA && item.ETA < unknownETA {
		eta = etaText(item.ETA)
	}

	return codeBlock("["+kindFlags[item.Kind]+"] "+name) + "\n" +
		codeBlock(bar) + "\n" +
		codeBlock(downloaded) + " <code>/</code> " + codeBlock(total) + "\n" +
		"↓" + codeBlock(speed) + " <code>|</code> <code>ETA</code> : " + codeBlock(eta) + "\n" +
		codeBlock("ID: "+itoa(item.ID))
}

// etaText renders a known ETA; zero reads as done rather than as unknown.
func etaText(seconds int64) string {
	if seconds <= 0 {
		return "0s"
	}
	return util.TimeFormat(seconds)
}

// clipName shortens a name for one status line.
func clipName(name string, limit int) string {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\n", " "))
	if runes := []rune(name); len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return name
}

// escape is html.EscapeString, kept short for the text builders.
func escape(s string) string { return html.EscapeString(s) }
