// Package util holds the small formatting helpers shared by the handlers.
package util

import (
	"fmt"
	"html"
	"math"
	"strconv"
	"strings"
	"time"
)

var sizeNames = []string{"B", "KB", "MB", "GB", "TB", "PB", "EB", "ZB", "YB"}

// ConvertSize renders a byte count the way the Python bot did: the value is
// rounded to two places and printed without padding, so 1.5 GB stays "1.5 GB"
// rather than "1.50 GB".
func ConvertSize[T int64 | int | float64](bytes T) string {
	size := float64(bytes)
	if size <= 0 || math.IsNaN(size) || math.IsInf(size, 0) {
		return fmt.Sprintf("%dB", int64(size))
	}
	i := int(math.Floor(math.Log(size) / math.Log(1024)))
	if i < 0 {
		i = 0
	}
	if i >= len(sizeNames) {
		i = len(sizeNames) - 1
	}
	value := math.Round(size/math.Pow(1024, float64(i))*100) / 100
	text := strconv.FormatFloat(value, 'f', -1, 64)
	// Python prints a rounded float, so a whole number keeps its ".0".
	if !strings.Contains(text, ".") {
		text += ".0"
	}
	return text + " " + sizeNames[i]
}

// TimeFormat renders a duration as the largest unit that applies downwards,
// e.g. "02h13m40s". Zero or negative durations have no useful reading.
func TimeFormat(seconds int64) string {
	if seconds <= 0 {
		return "-"
	}
	d := seconds / (3600 * 24)
	h := seconds / 3600 % 24
	m := seconds % 3600 / 60
	s := seconds % 3600 % 60
	switch {
	case d > 0:
		return fmt.Sprintf("%02dd%02dh%02dm%02ds", d, h, m, s)
	case h > 0:
		return fmt.Sprintf("%02dh%02dm%02ds", h, m, s)
	case m > 0:
		return fmt.Sprintf("%02dm%02ds", m, s)
	default:
		return fmt.Sprintf("%02ds", s)
	}
}

// ReadableTime renders an uptime as "1d4h12m3s", skipping leading zero units.
func ReadableTime(seconds int64) string {
	periods := []struct {
		name string
		secs int64
	}{{"d", 86400}, {"h", 3600}, {"m", 60}, {"s", 1}}
	var b strings.Builder
	for _, p := range periods {
		if seconds >= p.secs {
			value := seconds / p.secs
			seconds %= p.secs
			fmt.Fprintf(&b, "%d%s", value, p.name)
		}
	}
	return b.String()
}

// ProgressBar draws 12 blocks spread over 100%, not one block per 8% -- the
// latter fills the bar at 96% and never uses the last block.
func ProgressBar(pct float64) string {
	p := math.Min(math.Max(pct, 0), 100)
	full := int(p / 100 * 12)
	if full > 12 {
		full = 12
	}
	return fmt.Sprintf("[%s%s] %.2f%%", strings.Repeat("▓", full), strings.Repeat("░", 12-full), p)
}

// Age words how long ago a search result was posted.
//
// Only the largest unit that applies is shown. Everything under a day used to
// collapse into "< 1 Day", which hid exactly the detail that matters when
// judging a fresh post -- an hour old and twenty hours old are not the same
// prospect for retention or completion.
func Age(posted time.Time) string {
	difference := time.Since(posted)
	switch {
	case difference < time.Minute:
		// Also covers a timestamp slightly in the future, from clock skew
		// between the indexer and this host.
		return "< 1 Minute"
	case difference < time.Hour:
		return plural(int(difference.Minutes()), "Minute")
	case difference < 24*time.Hour:
		return plural(int(difference.Hours()), "Hour")
	default:
		return plural(int(difference.Hours()/24), "Day")
	}
}

func plural(count int, unit string) string {
	if count == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", count, unit)
}

// AgeFromEpoch parses the internal API's Unix timestamp.
func AgeFromEpoch(seconds int64) string {
	if seconds <= 0 {
		return "Not Found"
	}
	return Age(time.Unix(seconds, 0))
}

// Escape mirrors Python's html.escape(quote=True).
func Escape(s string) string {
	return html.EscapeString(s)
}
