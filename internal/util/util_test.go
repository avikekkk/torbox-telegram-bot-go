package util

import (
	"testing"
	"time"
)

// TestConvertSize pins the wording to what the Python helper produced, since
// these strings go straight into user-facing status messages.
func TestConvertSize(t *testing.T) {
	cases := map[int64]string{
		0:             "0B",
		1:             "1.0 B",
		3:             "3.0 B",
		999:           "999.0 B",
		1023:          "1023.0 B",
		1024:          "1.0 KB",
		1536:          "1.5 KB",
		1048576:       "1.0 MB",
		2147483648:    "2.0 GB",
		1234567890123: "1.12 TB",
	}
	for size, want := range cases {
		if got := ConvertSize(size); got != want {
			t.Errorf("ConvertSize(%d) = %q, want %q", size, got, want)
		}
	}
}

func TestTimeFormat(t *testing.T) {
	cases := map[int64]string{
		0:     "-",
		-5:    "-",
		9:     "09s",
		75:    "01m15s",
		3661:  "01h01m01s",
		90061: "01d01h01m01s",
	}
	for seconds, want := range cases {
		if got := TimeFormat(seconds); got != want {
			t.Errorf("TimeFormat(%d) = %q, want %q", seconds, got, want)
		}
	}
}

func TestReadableTime(t *testing.T) {
	if got, want := ReadableTime(90061), "1d1h1m1s"; got != want {
		t.Errorf("ReadableTime = %q, want %q", got, want)
	}
	if got, want := ReadableTime(59), "59s"; got != want {
		t.Errorf("ReadableTime = %q, want %q", got, want)
	}
}

func TestAge(t *testing.T) {
	cases := []struct {
		ago  time.Duration
		want string
	}{
		{0, "< 1 Minute"},
		{30 * time.Second, "< 1 Minute"},
		{-time.Minute, "< 1 Minute"}, // clock skew: posted "in the future"
		{time.Minute, "1 Minute"},
		{45 * time.Minute, "45 Minutes"},
		{time.Hour, "1 Hour"},
		{90 * time.Minute, "1 Hour"},
		{23 * time.Hour, "23 Hours"},
		{24 * time.Hour, "1 Day"},
		{47 * time.Hour, "1 Day"},
		{41 * 24 * time.Hour, "41 Days"},
	}
	for _, c := range cases {
		if got := Age(time.Now().Add(-c.ago)); got != c.want {
			t.Errorf("Age(%v ago) = %q, want %q", c.ago, got, c.want)
		}
	}
}

func TestAgeFromEpoch(t *testing.T) {
	if got := AgeFromEpoch(0); got != "Not Found" {
		t.Errorf("AgeFromEpoch(0) = %q", got)
	}
	if got := AgeFromEpoch(time.Now().Add(-90 * time.Minute).Unix()); got != "1 Hour" {
		t.Errorf("AgeFromEpoch(90m ago) = %q, want 1 Hour", got)
	}
}

// TestProgressBar covers the fix the Python version carried: 12 blocks spread
// over 100%, so the last block is reachable and only a full bar is full.
func TestProgressBar(t *testing.T) {
	cases := []struct {
		pct    float64
		blocks int
	}{{0, 0}, {50, 6}, {96, 11}, {100, 12}, {150, 12}, {-10, 0}}
	for _, c := range cases {
		got := ProgressBar(c.pct)
		full := 0
		for _, r := range got {
			if r == '▓' {
				full++
			}
		}
		if full != c.blocks {
			t.Errorf("ProgressBar(%v) = %q, want %d filled blocks", c.pct, got, c.blocks)
		}
	}
}
