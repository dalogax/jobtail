// Package skills embeds the agent skill that ships with jobtail, so
// `jobtail install-skill` always installs the copy written for the binary
// that's installing it, with no network fetch and no version skew.
package skills

import "embed"

// FS holds the jobtail skill at jobtail/SKILL.md (plus anything else under
// skills/jobtail/).
//
//go:embed jobtail
var FS embed.FS

// Name is the skill's directory name, and its `name:` in SKILL.md.
const Name = "jobtail"
