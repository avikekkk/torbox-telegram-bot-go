package bot

import (
	"fmt"
	"strings"
	"testing"

	"github.com/avikekkk/torbox-telegram-bot-go/internal/torbox"
)

func active(kind, state string, progress float64) torbox.Item {
	return torbox.Item{Kind: kind, State: state, Name: state, Progress: progress, HasProgress: true}
}

func TestStatusShowsOnlyActiveDownloads(t *testing.T) {
	items := []torbox.Item{
		{
			Kind: torbox.KindTorrent, ID: 1, Name: "A", Size: 4_000_000_000, State: "downloading",
			Progress: 0.4, HasProgress: true, Speed: 10_000_000, ETA: 14, HasETA: true,
		},
		{Kind: torbox.KindTorrent, ID: 2, Name: "B", State: "completed", Progress: 1, HasProgress: true},
	}
	text := renderStatus(items)
	want := "<u><b>DOWNLOADING</b></u>\n" +
		"<code>[T] A</code>\n" +
		"<code>[▓▓▓▓░░░░░░░░] 40.00%</code>\n" +
		"<code>1.49 GB</code> <code>/</code> <code>3.73 GB</code>\n" +
		"↓<code>9.54 MB/s</code> <code>|</code> <code>ETA</code> : <code>14s</code>\n" +
		"<code>ID: 1</code>"
	if text != want {
		t.Errorf("renderStatus =\n%s\nwant\n%s", text, want)
	}
}

func TestStatusEmptyWhenNothingActive(t *testing.T) {
	seeding := active(torbox.KindTorrent, "uploading", 1)
	if got := renderStatus([]torbox.Item{seeding}); got != noActiveTasks {
		t.Errorf("renderStatus = %q", got)
	}
	if got := renderStatus(nil); got != noActiveTasks {
		t.Errorf("renderStatus(nil) = %q", got)
	}
}

func TestStatusUnknownTelemetryIsDashes(t *testing.T) {
	meta := torbox.Item{Kind: torbox.KindTorrent, ID: 4, State: "metaDL", HasProgress: true, ETA: unknownETA, HasETA: true}
	text := renderStatus([]torbox.Item{meta})
	for _, want := range []string{
		"<u><b>METADATA</b></u>",
		"<code>--</code> <code>/</code> <code>--</code>",
		"↓<code>--</code> <code>|</code> <code>ETA</code> : <code>--</code>",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("renderStatus = %q, missing %q", text, want)
		}
	}

	// A hoster that reports no progress shows an empty bar, not 0.00%.
	web := torbox.Item{Kind: torbox.KindWebDL, ID: 100, Name: "limited.bin", Size: 4_000_000_000, State: "downloading"}
	text = renderStatus([]torbox.Item{web})
	if !strings.Contains(text, "[░░░░░░░░░░░░] --") || !strings.Contains(text, "<code>--</code> <code>/</code> <code>3.73 GB</code>") {
		t.Errorf("renderStatus = %q", text)
	}
	if strings.Contains(text, "0.00%") {
		t.Errorf("unknown progress rendered as zero: %q", text)
	}
}

func TestStatusPhases(t *testing.T) {
	cases := []struct {
		kind, state, phase, flag string
	}{
		{torbox.KindUsenet, "grabbing", "GRABBING", "U"},
		{torbox.KindUsenet, "verifying", "VERIFYING", "U"},
		{torbox.KindUsenet, "repairing", "REPAIRING", "U"},
		{torbox.KindUsenet, "extracting", "UNPACKING", "U"},
		{torbox.KindTorrent, "forcedMetaDL", "METADATA", "T"},
		{torbox.KindTorrent, "stalled (no seeds)", "STALLED", "T"},
		{torbox.KindTorrent, "pausedDL", "PAUSED", "T"},
		{torbox.KindTorrent, "queuedDL", "QUEUED", "T"},
		{torbox.KindTorrent, "checkingResumeData", "CHECKING", "T"},
		{torbox.KindTorrent, "allocating", "ALLOCATING", "T"},
		{torbox.KindWebDL, "pending", "QUEUED", "W"},
		{torbox.KindWebDL, "processing", "PROCESSING", "W"},
	}
	for _, c := range cases {
		text := renderStatus([]torbox.Item{active(c.kind, c.state, 0.5)})
		if !strings.HasPrefix(text, "<u><b>"+c.phase+"</b></u>\n<code>["+c.flag+"] "+c.state) {
			t.Errorf("%s %q: renderStatus = %q", c.kind, c.state, text)
		}
	}
}

func TestStatusOrdersByPhaseAndCountsTheRest(t *testing.T) {
	var items []torbox.Item
	for i := 1; i <= 8; i++ {
		item := active(torbox.KindTorrent, "downloading", 0.5)
		item.ID, item.Name = int64(i), fmt.Sprintf("item-%d", i)
		items = append(items, item)
	}
	queued := active(torbox.KindTorrent, "queuedDL", 0)
	queued.ID = 99
	items = append(items, queued)

	text := renderStatus(items)
	if !strings.HasPrefix(text, "<u><b>QUEUED</b></u>") {
		t.Errorf("queued download should lead: %q", text[:40])
	}
	if got := strings.Count(text, "<u><b>"); got != statusTaskLimit {
		t.Errorf("%d blocks shown, want %d", got, statusTaskLimit)
	}
	if !strings.HasSuffix(text, "\n\n<b>+3 more task(s)</b>") {
		t.Errorf("renderStatus should count the hidden downloads: %q", text[len(text)-40:])
	}
}

func TestClipName(t *testing.T) {
	long := strings.Repeat("é", 100)
	if got := clipName(long, 80); len([]rune(got)) != 80 || !strings.HasSuffix(got, "…") {
		t.Errorf("clipName = %q", got)
	}
}
