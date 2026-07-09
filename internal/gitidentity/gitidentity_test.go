package gitidentity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveName(t *testing.T) {
	cases := []struct {
		name string
		p    Params
		want string
	}{
		{"host name templated", Params{HostGitName: "Tom Hennen"}, "Tom Hennen (via rein)"},
		{"custom template", Params{HostGitName: "Ada", NameTemplate: "{name} [agent]"}, "Ada [agent]"},
		{"owner fallback templated", Params{OwnerLogin: "octo-org"}, "octo-org (via rein)"},
		{"host wins over owner", Params{HostGitName: "Tom", OwnerLogin: "octo-org"}, "Tom (via rein)"},
		{"branded default when nothing", Params{}, DefaultName},
		{"malformed template falls back to default", Params{HostGitName: "Ada", NameTemplate: "no placeholder"}, "Ada (via rein)"},
		{"whitespace host treated as empty", Params{HostGitName: "   ", OwnerLogin: "octo"}, "octo (via rein)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveName(tc.p); got != tc.want {
				t.Errorf("resolveName = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveEmailOverrideWins(t *testing.T) {
	// An explicit override short-circuits every lookup.
	id := Resolve(context.Background(), Params{
		EmailOverride: "custom[bot]@users.noreply.github.com",
		LookupSlug:    func(context.Context) (string, error) { t.Fatal("must not call slug lookup"); return "", nil },
	})
	if id.Email != "custom[bot]@users.noreply.github.com" {
		t.Errorf("email = %q, want the override", id.Email)
	}
}

func TestResolveEmailLinkingAndCache(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "bot.json")
	slugCalls, idCalls := 0, 0
	p := Params{
		KnownSlug:  "myapp",
		CachePath:  cache,
		LookupSlug: func(context.Context) (string, error) { slugCalls++; return "myapp", nil },
		LookupBotID: func(_ context.Context, login string) (int64, error) {
			idCalls++
			if login != "myapp[bot]" {
				t.Errorf("login=%q", login)
			}
			return 42, nil
		},
	}
	want := "42+myapp[bot]@users.noreply.github.com"

	id := Resolve(context.Background(), p)
	if id.Email != want {
		t.Fatalf("email = %q, want %q", id.Email, want)
	}
	if slugCalls != 0 {
		t.Errorf("slug lookup called %d times despite KnownSlug", slugCalls)
	}
	if idCalls != 1 {
		t.Errorf("bot-id lookup called %d times, want 1", idCalls)
	}

	// Second call must hit the cache — no further network.
	id2 := Resolve(context.Background(), p)
	if id2.Email != want {
		t.Errorf("cached email = %q, want %q", id2.Email, want)
	}
	if idCalls != 1 {
		t.Errorf("bot-id lookup called %d times total, want 1 (cache miss)", idCalls)
	}
}

func TestResolveEmailStaleCacheIgnoredOnSlugChange(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "bot.json")
	// Seed the cache for oldapp.
	writeCache(cache, cacheFile{Slug: "oldapp", BotUserID: 1, Email: "1+oldapp[bot]@users.noreply.github.com"}, nil)
	id := Resolve(context.Background(), Params{
		KnownSlug:   "newapp",
		CachePath:   cache,
		LookupBotID: func(context.Context, string) (int64, error) { return 99, nil },
	})
	if id.Email != "99+newapp[bot]@users.noreply.github.com" {
		t.Errorf("email = %q, want the newapp email (stale cache must be ignored)", id.Email)
	}
}

func TestResolveEmailCacheInvalidatedOnAppChange(t *testing.T) {
	// The env-var config path carries no slug (KnownSlug==""), so only the
	// AppIdentity (Client ID) key can tell that the developer switched Apps.
	cache := filepath.Join(t.TempDir(), "bot.json")
	writeCache(cache, cacheFile{AppIdentity: "Iv-OLD", Slug: "oldapp", BotUserID: 1, Email: "1+oldapp[bot]@users.noreply.github.com"}, nil)

	// Same App id -> cache trusted (no lookups).
	same := Resolve(context.Background(), Params{
		AppIdentity: "Iv-OLD",
		CachePath:   cache,
		LookupSlug:  func(context.Context) (string, error) { t.Fatal("must not re-resolve on a cache hit"); return "", nil },
	})
	if same.Email != "1+oldapp[bot]@users.noreply.github.com" {
		t.Errorf("same-app email = %q, want the cached value", same.Email)
	}

	// Different App id, no slug known -> cache ignored, re-resolve.
	changed := Resolve(context.Background(), Params{
		AppIdentity: "Iv-NEW",
		CachePath:   cache,
		LookupSlug:  func(context.Context) (string, error) { return "newapp", nil },
		LookupBotID: func(context.Context, string) (int64, error) { return 7, nil },
	})
	if changed.Email != "7+newapp[bot]@users.noreply.github.com" {
		t.Errorf("changed-app email = %q, want the re-resolved newapp value (stale cache must be ignored)", changed.Email)
	}
}

func TestResolveEmailNonLinkingFallbackWhenIDFails(t *testing.T) {
	id := Resolve(context.Background(), Params{
		KnownSlug:   "myapp",
		LookupBotID: func(context.Context, string) (int64, error) { return 0, errors.New("boom") },
	})
	if id.Email != "myapp[bot]@users.noreply.github.com" {
		t.Errorf("email = %q, want the non-linking fallback", id.Email)
	}
}

func TestResolveEmailDefaultWhenNoSlug(t *testing.T) {
	// No KnownSlug, slug lookup fails -> branded default, never a dev email.
	id := Resolve(context.Background(), Params{
		LookupSlug: func(context.Context) (string, error) { return "", errors.New("no net") },
	})
	if id.Email != DefaultEmail() {
		t.Errorf("email = %q, want %q", id.Email, DefaultEmail())
	}
}

func TestResolveNeverLeaksDevEmail(t *testing.T) {
	// No branch may produce anything resembling a real developer address.
	for _, p := range []Params{
		{HostGitName: "Tom Hennen", LookupSlug: func(context.Context) (string, error) { return "", errors.New("x") }},
		{HostGitName: "Tom Hennen", KnownSlug: "myapp", LookupBotID: func(context.Context, string) (int64, error) { return 0, errors.New("x") }},
		{},
	} {
		id := Resolve(context.Background(), p)
		if id.Email == "" {
			t.Errorf("email must never be empty")
		}
		if got := id.Email; got == "tom.hennen@gmail.com" {
			t.Errorf("dev email leaked: %q", got)
		}
	}
}

func TestResolveEmailCorruptCacheReResolves(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "bot.json")
	if err := os.WriteFile(cache, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := Resolve(context.Background(), Params{
		CachePath:   cache,
		KnownSlug:   "myapp",
		LookupBotID: func(context.Context, string) (int64, error) { return 5, nil },
	})
	if id.Email != "5+myapp[bot]@users.noreply.github.com" {
		t.Errorf("email = %q, want the re-resolved value (corrupt cache must be ignored)", id.Email)
	}
	if id.Email == "" {
		t.Error("email must never be empty even with a corrupt cache")
	}
}

func TestBotEmailFormat(t *testing.T) {
	// The linking format proven live against dependabot[bot] (id 49699333).
	if got := BotEmail(49699333, "dependabot"); got != "49699333+dependabot[bot]@users.noreply.github.com" {
		t.Errorf("BotEmail = %q", got)
	}
}
