package bot

import (
	"strings"
	"testing"
	"time"

	"github.com/avikekkk/torbox-telegram-bot-go/internal/torbox"
)

func counts(torrent, usenet, web, queued int) purgeCounts {
	return purgeCounts{
		torbox.KindTorrent: torrent, torbox.KindUsenet: usenet,
		torbox.KindWebDL: web, torbox.CollectionQueued: queued,
	}
}

func TestPurgeProgressMatchesPurgeUI(t *testing.T) {
	// 45 of 105 deleted, as drawn in purge-ui.txt.
	got := purgeProgressText(counts(100, 5, 0, 0), counts(60, 0, 0, 0), "", 14*time.Second)
	want := "<u><b>PURGING</b></u>\n" +
		"<code>[▓▓▓▓▓░░░░░░░] 42.86%</code>\n" +
		"<code>45 / 105</code>\n" +
		"<code>ETA</code> : <code>14s</code>"
	if got != want {
		t.Errorf("purgeProgressText =\n%s\nwant\n%s", got, want)
	}
}

func TestPurgeProgressComplete(t *testing.T) {
	got := purgeProgressText(counts(3, 0, 0, 0), counts(0, 0, 0, 0), "", -1)
	if !strings.HasPrefix(got, "<u><b>PURGE COMPLETE</b></u>") || !strings.Contains(got, "100.00%") ||
		!strings.Contains(got, "<code>3 / 3</code>") {
		t.Errorf("purgeProgressText = %q", got)
	}
}

func TestPurgeProgressNeverClaimsCompletionWhileGrowing(t *testing.T) {
	// Downloads added mid-purge push the remaining count above the start.
	got := purgeProgressText(counts(2, 0, 0, 0), counts(5, 0, 0, 0), "Monitoring timed out", -1)
	if !strings.Contains(got, "PURGING") || !strings.Contains(got, "0.00%") || !strings.Contains(got, "<code>0 / 2</code>") {
		t.Errorf("purgeProgressText = %q", got)
	}
	if !strings.HasSuffix(got, "<code>ETA</code> : <code>--</code>\n\nMonitoring timed out") {
		t.Errorf("label or ETA missing: %q", got)
	}
}

func TestPurgeSummary(t *testing.T) {
	got := purgeSummary(counts(3, 2, 0, 1))
	want := "<u><b>PURGE TORBOX</b></u>\n\n" +
		"<code>Torrents: 3\nUsenet: 2\nWeb downloads: 0\nQueued downloads: 1</code>\n" +
		"<code>Total: 6</code>\n\n" +
		"Permanently deletes these downloads and their files."
	if got != want {
		t.Errorf("purgeSummary =\n%s\nwant\n%s", got, want)
	}
}

func TestPurgeSubmitText(t *testing.T) {
	got := purgeSubmitText(2, 4, "Processed Usenet")
	if !strings.Contains(got, "PREPARING PURGE") || !strings.Contains(got, "50.00%") ||
		!strings.Contains(got, "2 / 4 deletion requests submitted") || !strings.HasSuffix(got, "\n\nProcessed Usenet") {
		t.Errorf("purgeSubmitText = %q", got)
	}
}
