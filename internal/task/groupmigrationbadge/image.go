package groupmigrationbadge

import (
	"fmt"
	"html"
	"os"
	"path/filepath"
)

// ImageFileName is the badge SVG's file name.
const ImageFileName = "azdo-migrated.svg"

const (
	labelText = "AZDO Migrated"
	color     = "#0078d4" // Azure blue
	charWidth = 6.6       // average advance of 11px Verdana, as shields.io estimates
	padding   = 10.0
)

// SVG draws the badge: "AZDO Migrated" on Azure blue.
func SVG() string {
	chars := len(labelText)
	width := int(float64(chars)*charWidth + 2*padding)
	text := html.EscapeString(labelText)
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%[1]d" height="20" role="img" aria-label="%[2]s">
  <title>%[2]s</title>
  <linearGradient id="s" x2="0" y2="100%%"><stop offset="0" stop-color="#bbb" stop-opacity=".1"/><stop offset="1" stop-opacity=".1"/></linearGradient>
  <clipPath id="r"><rect width="%[1]d" height="20" rx="3" fill="#fff"/></clipPath>
  <g clip-path="url(#r)">
    <rect width="%[1]d" height="20" fill="%[3]s"/>
    <rect width="%[1]d" height="20" fill="url(#s)"/>
  </g>
  <g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">
    <text x="%[4]d" y="15" fill="#010101" fill-opacity=".3">%[2]s</text>
    <text x="%[4]d" y="14">%[2]s</text>
  </g>
</svg>
`, width, text, color, width/2)
}

// WriteImage writes the badge SVG into dir and returns its path.
func WriteImage(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, ImageFileName)
	return path, os.WriteFile(path, []byte(SVG()), 0o644)
}
