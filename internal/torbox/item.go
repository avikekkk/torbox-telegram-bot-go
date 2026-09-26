package torbox

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// Item is one download in a TorBox library: a torrent, an NZB or a web
// download. Telemetry TorBox did not report is flagged rather than zeroed, so
// the status message can say "--" instead of a misleading 0.
type Item struct {
	Kind  string
	ID    int64
	Name  string
	Hash  string
	Size  int64
	State string

	// Progress is as TorBox reports it: 0-1 or 0-100. Use Percent.
	Progress    float64
	HasProgress bool

	Downloaded    int64
	HasDownloaded bool
	Speed         float64
	ETA           int64
	HasETA        bool
	// Active is TorBox's own flag, the fallback when the state says nothing.
	Active bool

	Files []File
}

// File is one file inside a download.
type File struct {
	ID    int64
	HasID bool
	Name  string
	Size  int64
}

// Percent returns progress as 0-100, or 0 when unknown.
func (it *Item) Percent() float64 { return percent(it.Progress, it.HasProgress) }

func percent(progress float64, known bool) float64 {
	if !known {
		return 0
	}
	if progress <= 1 {
		progress *= 100
	}
	return math.Max(0, math.Min(100, progress))
}

// NormalizedState is the state lower-cased with separators dropped, so
// "stalled (no seeds)" and "stalledDL" compare the way TorBox means them.
func (it *Item) NormalizedState() string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(it.State)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// terminalStates end a download one way or another.
var terminalStates = []string{
	"complete", "cached", "done", "finished", "failed", "error", "dead",
	"deleted", "expired", "cancel", "incomplete",
}

// torrentSeedingStates are qBittorrent's upload states: the download itself is
// over.
var torrentSeedingStates = map[string]bool{
	"uploading": true, "uploadingnopeers": true, "pausedup": true, "queuedup": true,
	"stalledup": true, "checkingup": true, "forcedup": true,
}

var torrentActiveStates = map[string]bool{
	"downloading": true, "forceddl": true, "metadl": true, "forcedmetadl": true,
	"stalleddl": true, "stallednoseeds": true, "pauseddl": true, "queueddl": true,
	"checkingdl": true, "checkingresumedata": true, "allocating": true, "moving": true,
	"processing": true,
}

var webActiveStates = []string{"download", "queue", "pending", "waiting", "process"}

var activeStates = []string{
	"download", "queue", "pending", "meta", "grab", "check", "verify", "repair",
	"unpack", "extract", "process", "active", "working", "pause",
}

// IsActive reports whether the item is still being worked on, which is what
// /status lists.
func (it *Item) IsActive() bool {
	state := strings.ToLower(strings.TrimSpace(it.State))
	normalized := it.NormalizedState()
	if containsAny(state, terminalStates...) || strings.Contains(normalized, "missing") {
		return false
	}

	switch it.Kind {
	case KindTorrent:
		if torrentSeedingStates[normalized] || strings.HasSuffix(normalized, "up") {
			return false
		}
		if torrentActiveStates[normalized] {
			return true
		}
		if normalized == "paused" {
			return it.Percent() < 100
		}
	case KindWebDL:
		if containsAny(state, webActiveStates...) {
			return true
		}
		return it.Active
	}

	if containsAny(state, activeStates...) {
		return true
	}
	if !it.HasProgress {
		return it.Active
	}
	return it.Percent() < 100
}

// failedStates mark a download that will never produce files.
var failedStates = []string{"failed", "error", "dead", "cancelled", "canceled", "deleted", "expired", "invalid"}

// readyStates mark a download whose files can be linked.
var readyStates = []string{"complete", "cached", "downloaded", "finished", "done"}

// IsFailed reports whether the download ended without files.
func (it *Item) IsFailed() bool {
	return containsAny(strings.ToLower(strings.TrimSpace(it.State)), failedStates...)
}

// IsReady reports whether the download finished and can be linked. A failed
// download is never ready, whatever progress it reached first.
func (it *Item) IsReady() bool {
	if it.IsFailed() {
		return false
	}
	if containsAny(strings.ToLower(strings.TrimSpace(it.State)), readyStates...) {
		return true
	}
	return it.Percent() >= 99.9 && !it.IsActive()
}

func containsAny(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}

// itemIDKeys are the fields each library uses for a download's ID.
var itemIDKeys = map[string][]string{
	KindTorrent: {"id", "torrent_id"},
	KindUsenet:  {"id", "usenet_id", "usenetdownload_id"},
	KindWebDL:   {"id", "webdl_id", "webdownload_id"},
}

// parseItem reads one download from its JSON object.
func parseItem(raw map[string]any, kind string) Item {
	item := Item{Kind: kind}
	for _, key := range itemIDKeys[kind] {
		if id, ok := toInt(raw[key]); ok {
			item.ID = id
			break
		}
	}
	item.Name = firstNonEmpty(toString(raw["name"]), toString(raw["title"]), "unknown")
	item.Hash = firstNonEmpty(toString(raw["hash"]), toString(raw["info_hash"]))
	item.Size, _ = toInt(raw["size"])
	item.State = firstNonEmpty(toString(raw["download_state"]), toString(raw["downloadState"]))
	item.Progress, item.HasProgress = toFloat(raw["progress"])
	item.Downloaded, item.HasDownloaded = toInt(raw["total_downloaded"])
	item.Speed, _ = toFloat(raw["download_speed"])
	item.ETA, item.HasETA = toInt(raw["eta"])
	item.Active, _ = raw["active"].(bool)

	files, _ := raw["files"].([]any)
	for _, entry := range files {
		object, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		file := File{
			Name: firstNonEmpty(toString(object["name"]), toString(object["short_name"]), toString(object["path"])),
		}
		file.ID, file.HasID = toInt(object["id"])
		if !file.HasID {
			file.ID, file.HasID = toInt(object["file_id"])
		}
		file.Size, _ = toInt(object["size"])
		item.Files = append(item.Files, file)
	}
	return item
}

// toInt reads a JSON number or numeric string.
func toInt(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), true
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return n, true
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return int64(f), true
		}
	}
	return 0, false
}

func toFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

func toString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
