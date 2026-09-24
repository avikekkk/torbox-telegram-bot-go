package torbox

import "testing"

func item(kind, state string, progress float64) Item {
	return Item{Kind: kind, State: state, Progress: progress, HasProgress: true}
}

func TestIsActiveTorrentStates(t *testing.T) {
	active := []string{
		"downloading", "forcedDL", "metaDL", "forcedMetaDL", "stalledDL", "stalled (no seeds)",
		"pausedDL", "queuedDL", "checkingDL", "checkingResumeData", "allocating", "moving", "processing",
	}
	for _, state := range active {
		if !(&Item{Kind: KindTorrent, State: state, Progress: 0.5, HasProgress: true}).IsActive() {
			t.Errorf("torrent %q should be active", state)
		}
	}

	// Seeding and terminal states: the download itself is over.
	inactive := []string{
		"uploading", "uploading (no peers)", "pausedUP", "queuedUP", "stalledUP", "checkingUP",
		"forcedUP", "completed", "cached", "failed", "failed (processing)", "error", "missingFiles",
		"expired", "reported missing", "incomplete",
	}
	for _, state := range inactive {
		it := item(KindTorrent, state, 1)
		if it.IsActive() {
			t.Errorf("torrent %q should not be active", state)
		}
	}
}

func TestIsActiveWebStates(t *testing.T) {
	for _, state := range []string{"downloading", "queued", "pending", "waiting", "processing"} {
		it := item(KindWebDL, state, 0.5)
		if !it.IsActive() {
			t.Errorf("web %q should be active", state)
		}
	}
	for _, state := range []string{"paused", "completed", "cached", "failed", "expired", "reported missing"} {
		it := item(KindWebDL, state, 0.5)
		if it.IsActive() {
			t.Errorf("web %q should not be active", state)
		}
	}
	// Nothing in the state: TorBox's own flag decides.
	flagged := Item{Kind: KindWebDL, Active: true}
	if !flagged.IsActive() {
		t.Error("web download flagged active should be active")
	}
}

func TestIsActiveUsenetPhases(t *testing.T) {
	for _, state := range []string{"grabbing", "verifying", "repairing", "unpacking", "extracting", "processing"} {
		it := item(KindUsenet, state, 1)
		if !it.IsActive() {
			t.Errorf("usenet %q should be active", state)
		}
	}
	// No state and no progress falls back to the active flag.
	if (&Item{Kind: KindUsenet}).IsActive() {
		t.Error("usenet with nothing reported should not be active")
	}
	unfinished := item(KindUsenet, "", 0.4)
	if !unfinished.IsActive() {
		t.Error("usenet at 40% should be active")
	}
}

func TestPausedTorrentIsActiveUntilComplete(t *testing.T) {
	partial := item(KindTorrent, "paused", 0.5)
	full := item(KindTorrent, "paused", 1)
	if !partial.IsActive() || full.IsActive() {
		t.Errorf("paused: partial active=%v, full active=%v", partial.IsActive(), full.IsActive())
	}
}

func TestReadyAndFailed(t *testing.T) {
	cases := []struct {
		state         string
		progress      float64
		ready, failed bool
	}{
		{"completed", 1, true, false},
		{"cached", 1, true, false},
		{"downloading", 0.5, false, false},
		{"failed", 0, false, true},
		{"expired", 0, false, true},
		// Finished by progress alone, once nothing says it is still working.
		{"stalledUP", 1, true, false},
	}
	for _, c := range cases {
		it := item(KindTorrent, c.state, c.progress)
		if it.IsReady() != c.ready || it.IsFailed() != c.failed {
			t.Errorf("%q: ready=%v failed=%v, want %v %v", c.state, it.IsReady(), it.IsFailed(), c.ready, c.failed)
		}
	}
}

func TestWebReadyAndFailed(t *testing.T) {
	cases := []struct {
		state         string
		ready, failed bool
	}{
		{"completed", true, false},
		{"cached", true, false},
		{"downloading", false, false},
		{"failed", false, true},
	}
	for _, c := range cases {
		it := item(KindWebDL, c.state, 1)
		if it.IsReady() != c.ready || it.IsFailed() != c.failed {
			t.Errorf("web %q: ready=%v failed=%v, want %v %v", c.state, it.IsReady(), it.IsFailed(), c.ready, c.failed)
		}
	}
}

func TestPercentAcceptsBothScales(t *testing.T) {
	cases := map[float64]float64{0.5: 50, 1: 100, 44.65: 44.65, 150: 100, -1: 0}
	for progress, want := range cases {
		it := item(KindTorrent, "", progress)
		if got := it.Percent(); got != want {
			t.Errorf("Percent(%v) = %v, want %v", progress, got, want)
		}
	}
}
