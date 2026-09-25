package bot

import (
	"context"
	"errors"
	"fmt"
	"html"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"github.com/avikekkk/torbox-telegram-bot-go/internal/store"
	"github.com/avikekkk/torbox-telegram-bot-go/internal/torbox"
	"github.com/avikekkk/torbox-telegram-bot-go/internal/util"
)

// channelPollInterval is how often a pending download is checked.
const channelPollInterval = 10 * time.Second

// channelMissingLimit is how many polls in a row a download may be missing
// before its job is dropped: deleted or purged, it will never finish.
const channelMissingLimit = 30

// channelPublisher posts each torrent, NZB and web download to DOWNLOAD_CHANNEL_ID
// once, when
// it finishes or fails. Jobs are stored, so a restart picks up where it left
// off. Cached downloads post as soon as they have a link.
type channelPublisher struct {
	bot *Bot

	mu      sync.Mutex
	ctx     context.Context
	running map[store.Job]bool

	peerMu sync.Mutex
	peer   tg.InputPeerClass
}

func newChannelPublisher(b *Bot) *channelPublisher {
	return &channelPublisher{bot: b, running: map[store.Job]bool{}}
}

// start resumes every job left pending by an earlier run.
func (p *channelPublisher) start(ctx context.Context) {
	if !p.bot.cfg.ChannelEnabled() {
		return
	}
	p.mu.Lock()
	p.ctx = ctx
	p.mu.Unlock()

	jobs, err := p.bot.store.PendingJobs()
	if err != nil {
		p.bot.log.Error("Could not load pending channel posts", "err", err)
		return
	}
	for _, job := range jobs {
		p.schedule(job)
	}
	p.bot.log.Info("Channel publisher ready", "pending", len(jobs))
}

// enqueue stores a new download and starts watching it.
func (p *channelPublisher) enqueue(ownerID int64, link *torbox.Link) {
	if !p.bot.cfg.ChannelEnabled() {
		return
	}
	job := store.Job{OwnerID: ownerID, Kind: link.Kind, ItemID: link.ID, Name: link.Name}
	if job.Name == "" {
		job.Name = fmt.Sprintf("%s #%d", link.Kind, link.ID)
	}
	added, err := p.bot.store.EnqueueJob(job)
	if err != nil {
		p.bot.log.Error("Could not queue channel post", "kind", job.Kind, "id", job.ItemID, "err", err)
		return
	}
	if added {
		p.schedule(job)
	}
}

func (p *channelPublisher) schedule(job store.Job) {
	// The name is display only; the job is keyed like its database row.
	key := store.Job{OwnerID: job.OwnerID, Kind: job.Kind, ItemID: job.ItemID}
	p.mu.Lock()
	ctx := p.ctx
	if ctx == nil || p.running[key] {
		p.mu.Unlock()
		return
	}
	p.running[key] = true
	p.mu.Unlock()

	// Deliberately not tracked by the shutdown WaitGroup: a watch can last
	// hours, and a restart resumes it from the database.
	go func() {
		defer func() {
			p.mu.Lock()
			delete(p.running, key)
			p.mu.Unlock()
		}()
		defer p.bot.recoverPanic("channel monitor")
		p.monitor(ctx, job)
	}()
}

func (p *channelPublisher) monitor(ctx context.Context, job store.Job) {
	b := p.bot
	log := b.log.With("kind", job.Kind, "id", job.ItemID)
	missing := 0
	for {
		done, err := p.check(ctx, job)
		switch {
		case done:
			return
		case torbox.IsNotFound(err):
			missing++
			if missing >= channelMissingLimit {
				log.Info("Channel post dropped: download no longer exists")
				if err := b.store.FinishJob(job, store.JobFailed); err != nil {
					log.Error("Could not close channel job", "err", err)
				}
				return
			}
		case err != nil && ctx.Err() == nil:
			missing = 0
			log.Debug("Channel monitor waiting", "err", err)
		default:
			missing = 0
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(channelPollInterval):
		}
	}
}

// check looks at a download once and posts it if it is over. It reports
// whether the job is finished.
func (p *channelPublisher) check(ctx context.Context, job store.Job) (bool, error) {
	b := p.bot
	item, err := b.torbox.Get(ctx, job.Kind, job.ItemID)
	if err != nil {
		return false, err
	}
	name := item.Name
	if name == "" || name == "unknown" {
		name = job.Name
	}

	if item.IsFailed() {
		return p.post(ctx, job, channelFailedText(name, job.ItemID), store.JobFailed)
	}
	if !item.IsReady() {
		return false, nil
	}

	// The Worker lists the files when the link is opened, so nothing needs
	// requesting from TorBox now.
	url := b.links.Page(job.Kind, job.ItemID, name)
	if url == "" {
		return false, errors.New("no Worker link yet")
	}
	return p.post(ctx, job, channelSuccessText(name, job.ItemID, item.Size, url), store.JobSuccess)
}

func (p *channelPublisher) post(ctx context.Context, job store.Job, text, status string) (bool, error) {
	b := p.bot
	target, err := p.channel(ctx)
	if err != nil {
		b.log.Error("Could not resolve the download channel", "err", err)
		return false, err
	}
	if err := b.send(ctx, target, text); err != nil {
		b.log.Error("Channel post failed; retrying", "kind", job.Kind, "id", job.ItemID, "err", err)
		return false, err
	}
	if err := b.store.FinishJob(job, status); err != nil {
		b.log.Error("Could not close channel job", "kind", job.Kind, "id", job.ItemID, "err", err)
	}
	b.log.Info("Channel post sent", "kind", job.Kind, "id", job.ItemID, "status", status)
	return true, nil
}

// channel resolves DOWNLOAD_CHANNEL_ID once. A bot may name a channel it is a
// member of with access hash 0; the real hash comes back and is kept.
func (p *channelPublisher) channel(ctx context.Context) (tg.InputPeerClass, error) {
	p.peerMu.Lock()
	defer p.peerMu.Unlock()
	if p.peer != nil {
		return p.peer, nil
	}

	channelID := -p.bot.cfg.DownloadChannelID - 1000000000000
	chats, err := p.bot.api.ChannelsGetChannels(ctx, []tg.InputChannelClass{
		&tg.InputChannel{ChannelID: channelID},
	})
	if err != nil {
		return nil, err
	}
	for _, chat := range chats.GetChats() {
		if channel, ok := chat.(*tg.Channel); ok && channel.ID == channelID {
			p.peer = &tg.InputPeerChannel{ChannelID: channel.ID, AccessHash: channel.AccessHash}
			return p.peer, nil
		}
	}
	return nil, fmt.Errorf("channel %d not found; is the bot an admin there?", p.bot.cfg.DownloadChannelID)
}

func channelSuccessText(name string, id, size int64, url string) string {
	sizeText := "Unknown size"
	if size > 0 {
		sizeText = util.ConvertSize(size)
	}
	return codeBlock(name) + " - [" + codeBlock(itoa(id)) + "]\n\n" +
		"<b>SUCCESS • " + sizeText + ` • <a href="` + html.EscapeString(url) + `">DL</a></b>`
}

func channelFailedText(name string, id int64) string {
	return codeBlock(name) + " - [" + codeBlock(itoa(id)) + "]\n\n<b>FAILED</b>"
}
