// Package skills embeds the agent skill so the CLI can print or install it
// (it always matches the CLI's version).
package skills

import _ "embed"

//go:embed pulse/SKILL.md
var Pulse []byte
