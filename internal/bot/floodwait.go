package bot

import (
	"context"
	"log/slog"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// maxFloodWait is the longest pause worth sitting through. Telegram hands out
// short waits for ordinary bursts; a very long one means the account is being
// throttled properly, and holding a command open for minutes helps no one.
const maxFloodWait = 30 * time.Second

// floodWaitRetries bounds how many times one request may be re-sent. Telegram
// can answer a retry with a second, longer wait.
const floodWaitRetries = 2

// floodWaiter re-sends a request that Telegram answered with FLOOD_WAIT.
//
// gotd only handles flood waits while connecting, so without this every
// throttled call fails outright: a search in a busy group would answer with
// nothing rather than a moment later. A bot shares one rate limit across every
// chat, so how much of it is left when a command lands depends on what the
// group has been doing, not on who typed the command.
func floodWaiter(log *slog.Logger) telegram.Middleware {
	return telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
		return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
			var err error
			for attempt := 0; ; attempt++ {
				err = next.Invoke(ctx, input, output)

				wait, ok := tgerr.AsFloodWait(err)
				if !ok || attempt >= floodWaitRetries {
					return err
				}
				// Telegram's own figure is the floor; a request sent the moment
				// it expires is often thrown back with a fresh wait.
				wait += time.Second
				if wait > maxFloodWait {
					log.Warn("Flood wait too long to sit out", "wait", wait)
					return err
				}
				log.Info("Flood wait, retrying", "wait", wait, "attempt", attempt+1)

				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(wait):
				}
			}
		}
	})
}
