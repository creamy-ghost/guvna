// Package web embeds the dashboard UI assets (templates + static) so the
// guvna-dashboard binary ships as one file with no external dependencies.
package web

import "embed"

// FS holds web/templates and web/static for embedding into the dashboard
// binary. Static assets are vendored (no CDN) so the UI works offline.
//
//	web/static/htmx.min.js  — htmx 2.0.10 (pinned stable; 2.x supported indefinitely)
//	web/static/pico.min.css — Pico CSS 2.1.1 (classless, dark mode built in)
//
//go:embed templates static
var FS embed.FS
