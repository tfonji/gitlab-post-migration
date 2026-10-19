package projectmigrationbadge

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
	"time"
)

// months in the style of the badge text: "Oct. 19, 2026", "March 3, 2026".
var months = [...]string{"", "Jan.", "Feb.", "March", "April", "May", "June", "July", "Aug.", "Sept.", "Oct.", "Nov.", "Dec."}

// FormatDate renders a migration date for the badge, e.g. "Oct. 19, 2026".
func FormatDate(d time.Time) string {
	return fmt.Sprintf("%s %d, %d", months[d.Month()], d.Day(), d.Year())
}

// ImageFileName is the SVG file name for a date (one image per date).
func ImageFileName(d time.Time) string {
	return "azdo-migrated-" + d.Format("2006-01-02") + ".svg"
}

const (
	labelText  = "AZDO Migrated"
	labelColor = "#555555"
	dateColor  = "#0078d4" // Azure blue
	charWidth  = 6.6       // average advance of 11px Verdana, as shields.io estimates
	padding    = 10.0
)

// SVG draws a two-part badge: a grey "AZDO Migrated" and a blue date.
func SVG(d time.Time) string {
	date := FormatDate(d)
	left := float64(len(labelText))*charWidth + 2*padding
	right := float64(len(date))*charWidth + 2*padding
	// Whole pixels; the right half takes the remainder so the halves meet.
	l := int(left)
	total := int(left + right)
	r := total - l
	title := html.EscapeString(labelText + " " + date)
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%[1]d" height="20" role="img" aria-label="%[2]s">
  <title>%[2]s</title>
  <linearGradient id="s" x2="0" y2="100%%"><stop offset="0" stop-color="#bbb" stop-opacity=".1"/><stop offset="1" stop-opacity=".1"/></linearGradient>
  <clipPath id="r"><rect width="%[1]d" height="20" rx="3" fill="#fff"/></clipPath>
  <g clip-path="url(#r)">
    <rect width="%[3]d" height="20" fill="%[6]s"/>
    <rect x="%[3]d" width="%[4]d" height="20" fill="%[7]s"/>
    <rect width="%[1]d" height="20" fill="url(#s)"/>
  </g>
  <g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">
    <text x="%[8]d" y="15" fill="#010101" fill-opacity=".3">%[9]s</text>
    <text x="%[8]d" y="14">%[9]s</text>
    <text x="%[10]d" y="15" fill="#010101" fill-opacity=".3">%[11]s</text>
    <text x="%[10]d" y="14">%[11]s</text>
  </g>
</svg>
`, total, title, l, r, 0, labelColor, dateColor,
		l/2, html.EscapeString(labelText), l+r/2, html.EscapeString(date))
}

// WriteImage writes the badge image for d into dir and returns its path.
func WriteImage(dir string, d time.Time) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, ImageFileName(d))
	return path, os.WriteFile(path, []byte(SVG(d)), 0o644)
}
