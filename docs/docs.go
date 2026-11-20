// Package docs embeds the public documentation served at /docs. Only the files listed here
// are published; ARCHITECTURE.md and PLAN.md stay internal.
package docs

import "embed"

//go:embed GUIDE.md API.md
var Files embed.FS
