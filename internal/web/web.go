// Package web embeds the browser UI so chirpd ships as one static binary.
package web

import "embed"

// Files holds the UI under "static/".
//
//go:embed static
var Files embed.FS
