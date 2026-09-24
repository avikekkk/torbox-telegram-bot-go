package bot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/telegram/message/markup"
	"github.com/gotd/td/tg"

	"github.com/avikekkk/torbox-telegram-bot-go/internal/torbox"
	"github.com/avikekkk/torbox-telegram-bot-go/internal/util"
)

const (
	// purgeConfirmTTL is how long the CONFIRM button stays live.
	purgeConfirmTTL = 10 * time.Minute
	// purgePollInterval and purgeMonitorTimeout pace the wait for TorBox to
	// finish deleting in the background.
	purgePollInterval   = 3 * time.Second
	purgeMonitorTimeout = 15 * time.Minute
)

// purgeCancelledMessage confirms a /purge the owner backed out of. It reports
// on the message rather than carrying content, so it is plain text.
const purgeCancelledMessage = "PURGE CANCELLED"

// purgeCollections are what /purge empties, in order, with their labels.
var purgeCollections = []struct{ key, label string }{
	{torbox.KindTorrent, "Torrents"},
	{torbox.KindUsenet, "Usenet"},
	{torbox.KindWebDL, "Web downloads"},
	{torbox.CollectionQueued, "Queued downloads"},
}

// purgeCounts is how many downloads each collection holds.
type purgeCounts map[string]int

func (c purgeCounts) total() int {
	total := 0
	for _, count := range c {
		total += count
	}
	return total
}

// purgeSession is a pending /purge waiting on its confirmation button.
type purgeSession struct {
	userID  int64
	counts  purgeCounts
	expires time.Time
}

// handlePurge shows what /purge would delete and asks for confirmation.
func (r *request) handlePurge(ctx context.Context) {
	if r.rejectGroup(ctx) {
		return
	}
	counts, err := r.bot.purgeCounts(ctx)
	if err != nil {
		r.bot.log.Warn("Purge preview failed", "err", err)
		r.replyLogged(ctx, r.bot.errorText(err))
		return
	}
	r.bot.log.Info("Purge preview created", "user_id", r.userID(), "total", counts.total())
	if counts.total() == 0 {
		r.replyLogged(ctx, "Nothing to delete")
		return
	}

	token, err := newToken(12)
	if err != nil {
		r.bot.log.Error("Could not create purge token", "err", err)
		r.replyLogged(ctx, errUnexpected)
		return
	}
	r.bot.mu.Lock()
	for key, session := range r.bot.purges {
		if time.Now().After(session.expires) {
			delete(r.bot.purges, key)
		}
	}
	r.bot.purges[token] = &purgeSession{userID: r.userID(), counts: counts, expires: time.Now().Add(purgeConfirmTTL)}
	r.bot.mu.Unlock()

	keyboard := markup.InlineKeyboard(markup.Row(
		markup.Callback("CONFIRM", []byte("purge:confirm:"+token)),
		markup.Callback("CANCEL", []byte("purge:cancel:"+token)),
	))
	if _, err := r.replyMarkup(ctx, purgeSummary(counts), keyboard); err != nil {
		r.bot.log.Warn("Failed to send purge confirmation", "err", err)
	}
}

// purgeCounts counts every collection in full. Unlike /status it fails on any
// error: a purge preview that silently undercounts is worse than none.
func (b *Bot) purgeCounts(ctx context.Context) (purgeCounts, error) {
	counts := purgeCounts{}
	errs := make([]error, len(purgeCollections))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, collection := range purgeCollections {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var items []torbox.Item
			var err error
			if collection.key == torbox.CollectionQueued {
				items, err = b.torbox.Queued(ctx)
			} else {
				items, err = b.torbox.ListAll(ctx, collection.key)
			}
			mu.Lock()
			defer mu.Unlock()
			counts[collection.key] = len(items)
			errs[i] = err
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return counts, nil
}

func purgeSummary(counts purgeCounts) string {
	rows := make([]string, 0, len(purgeCollections))
	for _, collection := range purgeCollections {
		rows = append(rows, fmt.Sprintf("%s: %d", collection.label, counts[collection.key]))
	}
	return header("PURGE TORBOX") + "\n\n" +
		codeBlock(strings.Join(rows, "\n")) + "\n" +
		codeBlock(fmt.Sprintf("Total: %d", counts.total())) + "\n\n" +
		"Permanently deletes these downloads and their files."
}

func purgeBar(done, total int) string {
	ratio := 1.0
	if total > 0 {
		ratio = min(1, max(0, float64(done)/float64(total)))
	}
	return codeBlock(util.ProgressBar(ratio * 100))
}

// purgeSubmitText tracks the bulk-delete requests, one per collection. It
// counts requests, not deleted downloads: TorBox deletes in the background.
func purgeSubmitText(done, total int, label string) string {
	return header("PREPARING PURGE") + "\n" +
		codeBlock("TorBox content") + "\n" +
		purgeBar(done, total) + "\n" +
		codeBlock(fmt.Sprintf("%d / %d deletion requests submitted", done, total)) + "\n\n" +
		escape(label)
}

// purgeProgressText renders confirmed deletions, as laid out in purge-ui.txt.
// eta is negative when there is no estimate yet.
func purgeProgressText(initial, current purgeCounts, label string, eta time.Duration) string {
	initialTotal := initial.total()
	remaining := current.total()
	deleted := max(0, initialTotal-remaining)
	title := "PURGING"
	etaValue := "--"
	switch {
	case remaining == 0:
		title = "PURGE COMPLETE"
		etaValue = etaText(0)
	case eta >= 0:
		etaValue = etaText(int64(eta.Seconds()))
	}
	// Downloads added meanwhile can grow the remaining count; the bar must
	// never claim completion then.
	progressTotal := max(initialTotal, remaining)

	text := header(title) + "\n" +
		purgeBar(deleted, progressTotal) + "\n" +
		codeBlock(fmt.Sprintf("%d / %d", deleted, initialTotal)) + "\n" +
		"<code>ETA</code> : " + codeBlock(etaValue)
	if label != "" {
		text += "\n\n" + escape(label)
	}
	return text
}

// onPurgeCallback drives the confirm and cancel buttons.
func (b *Bot) onPurgeCallback(cb *callback) {
	action, token, found := strings.Cut(strings.TrimPrefix(cb.data, "purge:"), ":")
	if !found {
		b.answerCallback(cb.ctx, cb.queryID, "", false)
		return
	}

	b.mu.Lock()
	session := b.purges[token]
	b.mu.Unlock()
	if session == nil || time.Now().After(session.expires) {
		b.answerCallback(cb.ctx, cb.queryID, "Purge request expired.", true)
		return
	}
	if cb.userID != session.userID {
		b.answerCallback(cb.ctx, cb.queryID, "Only requester can use this button.", true)
		return
	}

	// Take the session before acting: a double tap or a replayed button then
	// finds nothing, and cannot delete twice.
	b.mu.Lock()
	taken := b.purges[token] == session
	delete(b.purges, token)
	b.mu.Unlock()
	if !taken {
		b.answerCallback(cb.ctx, cb.queryID, "Purge request expired.", true)
		return
	}

	switch action {
	case "cancel":
		b.editLogged(cb.ctx, cb.peer, cb.msgID, purgeCancelledMessage)
		b.answerCallback(cb.ctx, cb.queryID, "Cancelled", false)
		return
	case "confirm":
	default:
		b.answerCallback(cb.ctx, cb.queryID, "", false)
		return
	}

	b.answerCallback(cb.ctx, cb.queryID, "", false)
	b.log.Info("Purge confirmed; submitting bulk deletes", "user_id", cb.userID, "total", session.counts.total())
	b.runPurge(cb.ctx, cb.peer, cb.msgID, session.counts)
}

// runPurge submits one bulk delete per non-empty collection, then watches the
// counts fall until TorBox has finished.
func (b *Bot) runPurge(ctx context.Context, target tg.InputPeerClass, msgID int, initial purgeCounts) {
	steps := len(purgeCollections)
	b.editLogged(ctx, target, msgID, purgeSubmitText(0, steps, "Starting…"))
	for done, collection := range purgeCollections {
		if initial[collection.key] > 0 {
			if err := b.torbox.DeleteAll(ctx, collection.key); err != nil {
				b.log.Error("Purge stopped by TorBox error", "step", done, "collection", collection.key, "err", err)
				b.editLogged(ctx, target, msgID,
					purgeSubmitText(done, steps, "Stopped")+"\n\n"+b.errorText(err))
				return
			}
		}
		b.log.Info("Purge collection submitted", "collection", collection.key, "count", initial[collection.key])
		b.editLogged(ctx, target, msgID, purgeSubmitText(done+1, steps, "Processed "+collection.label))
	}

	started := time.Now()
	deadline := started.Add(purgeMonitorTimeout)
	last := initial
	changed := true
	for time.Now().Before(deadline) {
		current, err := b.purgeCounts(ctx)
		switch {
		case ctx.Err() != nil:
			b.editLogged(ctx, target, msgID, purgeProgressText(initial, last,
				"Monitoring stopped by a bot restart; TorBox may still be deleting. Run /purge to refresh.", -1))
			return
		case err != nil:
			b.log.Warn("Purge progress poll failed; retrying", "err", err)
		default:
			changed = current.total() != last.total() || changed
			last = current
		}

		if remaining := last.total(); remaining == 0 {
			b.log.Info("Purge complete", "deleted", initial.total(), "took", time.Since(started).Round(time.Second))
			b.editLogged(ctx, target, msgID, purgeProgressText(initial, last, "", -1))
			return
		} else if changed {
			changed = false
			eta := time.Duration(-1)
			deleted := initial.total() - remaining
			if elapsed := time.Since(started); deleted > 0 && elapsed >= time.Second {
				eta = time.Duration(float64(elapsed) * float64(remaining) / float64(deleted))
			}
			b.editLogged(ctx, target, msgID, purgeProgressText(initial, last, "", eta))
		}

		select {
		case <-ctx.Done():
		case <-time.After(purgePollInterval):
		}
	}

	b.log.Warn("Purge monitoring timed out", "remaining", last.total())
	b.editLogged(ctx, target, msgID, purgeProgressText(initial, last,
		"Monitoring timed out; TorBox may still be deleting. Run /purge to refresh.", -1))
}
