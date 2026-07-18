package srt

import "github.com/TomHennen/rein/internal/sandboxutil"

// ResolveExtraAllowedDomains and EnvAllowDomains moved to
// internal/sandboxutil (substrate-neutral: nono's profile generator needs the
// same REIN_ALLOW_DOMAINS resolver without importing srt — see
// docs/design-nono-pivot.md §5/§7). Re-exported here so existing srt callers
// are unaffected.
const EnvAllowDomains = sandboxutil.EnvAllowDomains

var ResolveExtraAllowedDomains = sandboxutil.ResolveExtraAllowedDomains
