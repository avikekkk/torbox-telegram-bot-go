package bot

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"

	"github.com/avikekkk/torbox-telegram-bot-go/internal/util"
)

func statLine(label, value string) string {
	return "<code>" + label + ":</code> " + codeBlock(value)
}

// handleServer reports the TorBox account alongside the host the bot runs on.
func (r *request) handleServer(ctx context.Context) {
	log := r.bot.log.With("chat_id", r.chatID(), "user_id", r.userID())
	log.Info("Server stats requested")

	statsMsgID, err := r.reply(ctx, "Fetching stats")
	if err != nil {
		log.Warn("Failed to post status message", "err", err)
		return
	}

	text := r.systemStats()
	// The account block is a bonus: a TorBox hiccup should not cost the host
	// stats, which need nothing from TorBox.
	if account, err := r.bot.torbox.Me(ctx); err != nil {
		log.Warn("Could not read the TorBox account", "err", err)
	} else {
		text = torboxStats(account.Plan, account.PremiumExpires) + "\n\n" + text
	}
	r.editLogged(ctx, statsMsgID, text)
}

func torboxStats(plan, expires string) string {
	lines := []string{header("TORBOX STATS")}
	if plan != "" {
		lines = append(lines, statLine("Plan", plan))
	}
	if expires != "" {
		if parsed, err := time.Parse(time.RFC3339, expires); err == nil {
			expires = parsed.Format("2006-01-02")
		}
		lines = append(lines, statLine("Expires", expires))
	}
	return strings.Join(lines, "\n")
}

// systemStats describes the host the bot runs on.
func (r *request) systemStats() string {
	lines := []string{
		header("SYSTEM STATS"),
		statLine("Bot Uptime", util.ReadableTime(int64(time.Since(r.bot.startedAt).Seconds()))),
	}

	var diskPercent float64
	if usage, err := disk.Usage("/"); err == nil {
		diskPercent = usage.UsedPercent
		lines = append(lines, statLine("Disk", util.ConvertSize(int64(usage.Total)))+" <code>|</code> "+
			statLine("Free", util.ConvertSize(int64(usage.Free))))
	}

	var cpuPercent float64
	if percents, err := cpu.Percent(500*time.Millisecond, false); err == nil && len(percents) > 0 {
		cpuPercent = percents[0]
	}
	var ramPercent float64
	if memory, err := mem.VirtualMemory(); err == nil {
		ramPercent = memory.UsedPercent
	}

	lines = append(lines, fmt.Sprintf(
		"\n<code>CPU:</code> <code>%.1f%%</code> <code>|</code> <code>RAM:</code> <code>%.1f%%</code> "+
			"<code>|</code> <code>DISK:</code> <code>%.1f%%</code>",
		cpuPercent, ramPercent, diskPercent,
	))

	return strings.Join(lines, "\n")
}
