package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The fixture $HOME run.sh probes instead of the developer's real one, so the
// report is identical on every machine. Fake contents only.

// HomeSentinels are files no specific policy entry names: only the wholesale
// $HOME deny hides them. Each is probed on its own.
var HomeSentinels = []string{
	".rein-containment-read-sentinel",
	".aws/credentials",
	".bash_history",
	".docker/config.json",
	".kube/config",
	".npmrc",
}

// fixtureFiles gives every denyRead and allowRead entry rein configures under
// $HOME (cmd/rein: credentialDenyReadPaths, sandboxAllowReadPaths), and srt's
// default write dirs, real content to be judged against. emitTargets fails if
// the policy grows an entry this list does not cover.
var fixtureFiles = []string{
	// credential denies
	".ssh/id_ed25519", ".gnupg/secring.gpg", ".netrc", ".git-credentials",
	".config/gh/hosts.yml", ".config/git/credentials",
	".local/share/keyrings/login.keyring", ".local/share/kwalletd/kdewallet.kwl",
	".cargo/credentials", ".cargo/credentials.toml",
	".config/rein-credentials/app.pem", ".config/rein/ca-key.pem", ".local/state/rein/state.json",
	".claude/history.jsonl", ".claude.json",
	// toolchain allow-backs
	".local/bin/tool", ".cargo/bin/cargo", ".rustup/settings.toml",
	"go/pkg/mod/fixture.txt", ".pyenv/version", ".cache/ms-playwright/fixture.txt",
	// srt's default write dirs (#153)
	".claude/debug/fixture.log", ".npm/_logs/fixture.log",
}

func writeFixture(home string) error {
	files := map[string]string{
		// A denied file too, and what rein's git-identity lookup reads.
		".gitconfig": "[user]\n\tname = rein containment fixture\n\temail = fixture@example.invalid\n",
	}
	for _, f := range append(append([]string{}, fixtureFiles...), HomeSentinels...) {
		files[f] = "rein containment fixture: not a credential\n"
	}
	for rel, content := range files {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// underHome reports whether p is inside home, in either its given or its
// symlink-resolved spelling (the policy may carry the resolved one).
func underHome(home, p string) bool {
	homes := []string{home}
	if r, err := filepath.EvalSymlinks(home); err == nil && r != home {
		homes = append(homes, r)
	}
	for _, h := range homes {
		if strings.HasPrefix(p, h+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// requirePlanted fails when a policy entry under the fixture home has nothing
// planted: it could not be judged, and would silently drop out of the report.
// A policy with NO entry under home means home is not what rein hid.
func requirePlanted(home string, entries []string) error {
	n := 0
	for _, e := range entries {
		if !underHome(home, e) {
			continue
		}
		n++
		if st, err := os.Stat(e); err != nil || (st.IsDir() && sampleDeep(e) == "") {
			return fmt.Errorf("policy entry %s has no content in the fixture home: add it to fixtureFiles (tests/containment/cmd/classify/fixture.go)", e)
		}
	}
	if n == 0 {
		return fmt.Errorf("no policy entry is under the fixture home %s", home)
	}
	return nil
}

// sampleDeep returns some file (or symlink) under dir, or "".
func sampleDeep(dir string) string {
	found := ""
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	return found
}
