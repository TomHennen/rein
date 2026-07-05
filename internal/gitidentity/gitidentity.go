// Package gitidentity resolves the NON-IMPERSONATING git author/committer
// identity that rein stamps into a sandboxed run (CP4).
//
// The problem it solves: a sandboxed agent that runs `git commit` reads the
// developer's host ~/.gitconfig by default, so its commits author as the
// DEVELOPER (verified: user.name="Tom Hennen", user.email=…@gmail.com) — plain
// impersonation. rein instead stamps GIT_AUTHOR_*/GIT_COMMITTER_* env vars (set
// by internal/srt.BuildEnv) with an identity that (a) marks the human behind
// the agent without claiming to BE them, and (b) links the commit to rein's
// GitHub App so the author matches the pusher.
//
//   - NAME  = the developer's real git name run through a template, default
//     "{name} (via rein)" (configurable). Falls back to the App-owner login,
//     then a branded default — NEVER a bare impersonation.
//   - EMAIL = the App bot's GitHub noreply address,
//     "<botUserID>+<slug>[bot]@users.noreply.github.com", which GitHub uses to
//     attribute the commit to the App (the dependabot[bot] convention). Falls
//     back to a non-linking "<slug>[bot]@…" and finally a branded constant —
//     NEVER the developer's real email.
//
// Resolve NEVER returns an error and NEVER blocks: every fallback is a valid,
// non-impersonating identity, so identity resolution degrades gracefully rather
// than failing a launch (fail-open is correct here precisely BECAUSE no fallback
// leaks the developer or grants anything — the security control is that the dev
// email/name never appears, and that holds on every branch).
//
// The two GitHub lookups are injected so the package is unit-testable without a
// network; cmd/rein wires the production closures (App-JWT GET /app for the
// slug; UNAUTHENTICATED GET /users/<slug>[bot] for the id — the latter 401s
// under JWT auth, a live-verified gotcha). The resolved bot email is cached to
// disk so steady-state launches do zero network work.
package gitidentity

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// DefaultNameTemplate marks the human behind the agent without impersonating
// them. "{name}" is substituted with the resolved developer/owner name.
const DefaultNameTemplate = "{name} (via rein)"

// DefaultName is the last-ditch author name when neither a host git name nor an
// owner login is known. Already branded, so the template is NOT applied to it.
const DefaultName = "rein agent"

// namePlaceholder is the token replaced in a name template.
const namePlaceholder = "{name}"

// Identity is the git author/committer identity rein stamps into the sandbox.
// Author and committer are set to the same value (the agent is both).
type Identity struct {
	Name  string // GIT_AUTHOR_NAME / GIT_COMMITTER_NAME
	Email string // GIT_AUTHOR_EMAIL / GIT_COMMITTER_EMAIL
}

// SlugLookup mints an App JWT and returns the App's slug (GET /app).
type SlugLookup func(ctx context.Context) (string, error)

// BotIDLookup returns the numeric user id for a bot login like "myapp[bot]"
// (UNAUTHENTICATED GET /users/<login> — JWT auth 401s on this endpoint).
type BotIDLookup func(ctx context.Context, botLogin string) (int64, error)

// Params are the inputs to Resolve. All fields are optional; Resolve degrades
// through the fallback chain as inputs go missing.
type Params struct {
	// HostGitName is `git config --get user.name` run OUTSIDE the sandbox at
	// launch. Empty if the developer has no git name configured.
	HostGitName string

	// OwnerLogin is the App-owner / repo-owner login, the name fallback when
	// HostGitName is empty. Empty if unknown.
	OwnerLogin string

	// NameTemplate is applied to the resolved name; empty means
	// DefaultNameTemplate. A template lacking the "{name}" placeholder is
	// rejected in favor of the default (so a malformed override can never drop
	// the attribution marker).
	NameTemplate string

	// EmailOverride, when non-empty, is used verbatim as the author email and
	// short-circuits all lookups (REIN_GIT_AUTHOR_EMAIL). The caller is trusted
	// not to set it to the developer's real address.
	EmailOverride string

	// KnownSlug is the App slug if already known (e.g. state.json Primary.Slug),
	// letting Resolve skip the GET /app call. Empty to discover via LookupSlug.
	KnownSlug string

	// AppIdentity keys the on-disk cache to a specific App (the App Client ID).
	// It is the robust cache-invalidation signal on the env-var config path,
	// where no slug is known up front (KnownSlug=="") so a slug-only check can't
	// tell that the developer switched Apps: a cached email whose AppIdentity
	// differs from the current one is ignored (re-resolved). Empty disables the
	// identity check (falls back to the slug check alone).
	AppIdentity string

	// CachePath is the JSON file the resolved bot email is cached to/from.
	// Empty disables caching (every launch re-resolves).
	CachePath string

	// LookupSlug / LookupBotID perform the two GitHub lookups. Nil skips that
	// step (used by tests, and by any caller with no network).
	LookupSlug  SlugLookup
	LookupBotID BotIDLookup

	Logger *log.Logger
}

// cacheFile is the on-disk shape of the bot-email cache. Keyed by App identity
// (Client ID) AND slug so a changed App invalidates a stale entry even on the
// env-var config path where no slug is known up front.
type cacheFile struct {
	AppIdentity string `json:"app_identity,omitempty"`
	Slug        string `json:"slug"`
	BotUserID   int64  `json:"bot_user_id"`
	Email       string `json:"email"`
}

// Resolve returns the non-impersonating identity. It never errors: on any
// failure it degrades to the next fallback, all of which are safe.
func Resolve(ctx context.Context, p Params) Identity {
	return Identity{
		Name:  resolveName(p),
		Email: resolveEmail(ctx, p),
	}
}

// resolveName runs the base name (host git name, else owner login) through the
// template. With no base name it returns DefaultName unchanged (already
// branded — applying "(via rein)" to "rein agent" would be silly).
func resolveName(p Params) string {
	base := strings.TrimSpace(p.HostGitName)
	if base == "" {
		base = strings.TrimSpace(p.OwnerLogin)
	}
	if base == "" {
		return DefaultName
	}
	tmpl := p.NameTemplate
	if !strings.Contains(tmpl, namePlaceholder) {
		// Empty or malformed override (no "{name}") — fall back to the default
		// so the attribution marker is never silently dropped.
		if tmpl != "" && p.Logger != nil {
			p.Logger.Printf("git identity: name template %q lacks %s; using default %q", tmpl, namePlaceholder, DefaultNameTemplate)
		}
		tmpl = DefaultNameTemplate
	}
	return strings.ReplaceAll(tmpl, namePlaceholder, base)
}

// resolveEmail walks the fallback chain:
//
//  1. EmailOverride (verbatim).
//  2. cache hit (slug matches when KnownSlug is set).
//  3. GET /app slug + GET /users/<slug>[bot] id -> linking noreply; cache it.
//  4. slug known but id lookup failed -> non-linking "<slug>[bot]@…" (uncached).
//  5. nothing resolvable -> branded default constant.
//
// It never returns the developer's real email on any branch.
func resolveEmail(ctx context.Context, p Params) string {
	if e := strings.TrimSpace(p.EmailOverride); e != "" {
		return e
	}

	if p.CachePath != "" {
		if c, ok := readCache(p.CachePath); ok && c.Email != "" && cacheMatches(c, p) {
			return c.Email
		}
	}

	slug := strings.TrimSpace(p.KnownSlug)
	if slug == "" && p.LookupSlug != nil {
		s, err := p.LookupSlug(ctx)
		if err != nil {
			if p.Logger != nil {
				p.Logger.Printf("git identity: GET /app slug lookup failed (%v); using non-linking bot email fallback", err)
			}
		} else {
			slug = strings.TrimSpace(s)
		}
	}

	if slug == "" {
		// No slug at all — cannot build any bot address. Branded default.
		if p.Logger != nil {
			p.Logger.Printf("git identity: no App slug resolvable; using default bot email %q", DefaultEmail())
		}
		return DefaultEmail()
	}

	botLogin := slug + "[bot]"
	if p.LookupBotID != nil {
		id, err := p.LookupBotID(ctx, botLogin)
		if err == nil && id > 0 {
			email := BotEmail(id, slug)
			if p.CachePath != "" {
				writeCache(p.CachePath, cacheFile{AppIdentity: p.AppIdentity, Slug: slug, BotUserID: id, Email: email}, p.Logger)
			}
			return email
		}
		if p.Logger != nil {
			p.Logger.Printf("git identity: bot-id lookup for %q failed (%v); using non-linking fallback", botLogin, err)
		}
	}

	// Slug known, id not — a valid noreply that won't LINK to the App but is
	// still non-impersonating. Not cached: retry the id lookup next launch.
	return NonLinkingBotEmail(slug)
}

// BotEmail is the LINKING GitHub noreply address that attributes a commit to
// the App bot (the "<id>+<login>@users.noreply.github.com" convention).
func BotEmail(botUserID int64, slug string) string {
	return fmt.Sprintf("%d+%s[bot]@users.noreply.github.com", botUserID, slug)
}

// NonLinkingBotEmail is the id-less noreply used when the bot id can't be
// resolved. Non-impersonating but not attributed to the App.
func NonLinkingBotEmail(slug string) string {
	return fmt.Sprintf("%s[bot]@users.noreply.github.com", slug)
}

// DefaultEmail is the branded last-ditch address when no slug is known.
func DefaultEmail() string {
	return "rein[bot]@users.noreply.github.com"
}

// cacheMatches reports whether a cached entry may be trusted for these Params.
// It is invalidated when EITHER the App identity (Client ID) OR a known slug
// differs — so switching Apps re-resolves even on the env-var path (no slug up
// front) via the identity key. An empty key on either side skips that check
// (best-effort back-compat with pre-key cache files).
func cacheMatches(c cacheFile, p Params) bool {
	if p.AppIdentity != "" && c.AppIdentity != "" && !strings.EqualFold(c.AppIdentity, p.AppIdentity) {
		return false
	}
	if p.KnownSlug != "" && c.Slug != "" && !strings.EqualFold(c.Slug, p.KnownSlug) {
		return false
	}
	return true
}

// readCache reads the bot-email cache file. Returns ok=false on any error
// (missing/corrupt) — a cache miss is never fatal.
func readCache(path string) (cacheFile, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return cacheFile{}, false
	}
	var c cacheFile
	if err := json.Unmarshal(b, &c); err != nil {
		return cacheFile{}, false
	}
	return c, true
}

// writeCache atomically writes the cache (temp + rename, mode 0600). Best-effort:
// a failure only means the next launch re-resolves over the network.
func writeCache(path string, c cacheFile, logger *log.Logger) {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		if logger != nil {
			logger.Printf("git identity: cache dir create failed (%v); not caching", err)
		}
		return
	}
	tmp, err := os.CreateTemp(dir, ".bot-identity-*.json")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	if err := os.Rename(tmpName, path); err != nil && logger != nil {
		logger.Printf("git identity: cache rename failed (%v); not caching", err)
	}
}
