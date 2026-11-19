// Package web embeds the built UI (web/dist). Build it with `npm run build`.
package web

import "embed"

//go:embed all:dist
var Dist embed.FS
