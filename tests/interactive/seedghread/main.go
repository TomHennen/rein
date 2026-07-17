// Command seedghread mints a REAL, currently-valid gh-read token scoped to a
// SINGLE repository and writes it as a tokencache.Entry to a caller-named path.
//
// It exists ONLY to seed the #95 regression journey's stale-cache fixture
// (tests/interactive/journeys/sandbox_gh_read_staleness/journey.py): that journey needs
// a genuine, narrowly-scoped (repo-A-only) leftover token planted at the LEGACY
// untagged cache path a prior single-repo run would have written, so a pre-fix
// broker serves it to a wider [A,B] run and 404s on repo B — faithfully
// reproducing issue #95 rather than a garbage-token 401.
//
// It is TEST-SUPPORT, deliberately NOT a `rein` subcommand: it is a standalone
// main under tests/, never wired into the rein CLI, never shipped by the release
// build (.goreleaser builds only ./cmd/rein). It mints exactly what
// cmd/rein/issue95_live_test.go mints for the same purpose — the narrow token an
// earlier run leaves behind — and nothing more (no arbitrary-scope surface).
//
// Usage:
//
//	seedghread --repo <owner>/<name> --out <path/to/gh-read-token.json>
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/TomHennen/rein/internal/config"
	"github.com/TomHennen/rein/internal/githubapp"
	"github.com/TomHennen/rein/internal/tokencache"
)

func main() {
	repo := flag.String("repo", "", "owner/name to scope the gh-read token to (REQUIRED)")
	out := flag.String("out", "", "path to write the tokencache.Entry JSON to (REQUIRED)")
	flag.Parse()

	if *repo == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "seedghread: --repo and --out are both required")
		os.Exit(2)
	}

	if err := run(*repo, *out); err != nil {
		fmt.Fprintf(os.Stderr, "seedghread: %v\n", err)
		os.Exit(1)
	}
}

func run(repo, out string) error {
	// Resolve the App config the same way rein does (env REIN_APP_* or state),
	// then narrow the token to the SINGLE repo — exactly the earlier-run token
	// issue95_live_test.go mints as its stale-cache seed.
	appCfg, ks, _, err := config.ResolveApp()
	if err != nil {
		return fmt.Errorf("resolve App config: %w", err)
	}
	appCfg.RepoNames = []string{bareName(repo)}

	client, err := githubapp.NewClient(appCfg, ks, config.AppKeystoreRole)
	if err != nil {
		return fmt.Errorf("new client (scoped to %s): %w", repo, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tok, exp, err := client.MintGhReadOnlyToken(ctx)
	if err != nil {
		return fmt.Errorf("mint gh-read token scoped to %s: %w", repo, err)
	}

	if err := tokencache.Write(out, tokencache.Entry{Token: tok, ExpiresAt: exp}); err != nil {
		return fmt.Errorf("write token cache to %s: %w", out, err)
	}
	fmt.Fprintf(os.Stderr, "seedghread: wrote %s-scoped gh-read token to %s (expires %s)\n",
		repo, out, exp.Format(time.RFC3339))
	return nil
}

// bareName strips the owner from an owner/name repo, matching how rein scopes
// installation tokens (githubapp.Config.RepoNames are owner-less names).
func bareName(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}
