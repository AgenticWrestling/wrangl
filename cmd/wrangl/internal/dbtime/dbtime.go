// Package dbtime is the one timestamp format stored in the database (UTC, millisecond
// precision, fixed width so it sorts as text), and relative-time rendering for it.
package dbtime

import (
	"fmt"
	"time"
)

// Layout is the stored timestamp format.
const Layout = "2006-01-02T15:04:05.000Z"

// Format renders t in the stored format.
func Format(t time.Time) string { return t.UTC().Format(Layout) }

// Parse reads any RFC 3339 timestamp (the stored format included).
func Parse(ts string) (time.Time, error) { return time.Parse(time.RFC3339Nano, ts) }

// Ago renders a timestamp as a relative age for the web app: "5m ago", and a date once it is
// over two days old. Empty is "-"; unparseable text is returned as is.
func Ago(ts string, now time.Time) string {
	if ts == "" {
		return "-"
	}
	t, err := Parse(ts)
	if err != nil {
		return ts
	}
	d := now.Sub(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1fh ago", d.Hours())
	}
	return t.Format("2 Jan")
}

// AgoShort is Ago for terminal tables: "5m", "1.5h" with no suffix and no date fallback.
func AgoShort(ts string, now time.Time) string {
	if ts == "" {
		return "-"
	}
	t, err := Parse(ts)
	if err != nil {
		return ts
	}
	d := now.Sub(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%.1fh", d.Hours())
	}
}
