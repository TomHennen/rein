// Shell-rc alias management for `rein init` (Phase 0.5 CP3).
//
// `rein init` writes a small managed block to the user's shell rc that
// aliases `claude` to `rein run -- claude`. Without this, users who
// forget to type `rein run --` get the unwrapped agent, which silently
// bypasses the broker. The alias is opt-out (--no-alias).
//
// Per-shell handling:
//
//   - bash / zsh use `alias claude='rein run -- claude'`. Bypass with
//     `\claude` (literal backslash before the alias name).
//   - fish uses a function (`alias` in fish silently drops $argv).
//     Bypass with `command claude`.
//
// The managed block is bracketed by uniquely-prefixed BEGIN/END markers
// so re-runs splice the new block over the old in place, leaving any
// surrounding user content untouched.

package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// Long enough to never collide accidentally with another tool's
	// managed-block markers; short enough that re-readers grok it.
	aliasBeginMarker = "# BEGIN rein-credentials managed block"
	aliasEndMarker   = "# END rein-credentials managed block"
)

// aliasPlan captures everything the shell-specific path needs the
// generic install routine to know: where the rc lives, what to splice
// in, and what to tell the user about the bypass escape.
type aliasPlan struct {
	shell      string
	rcPath     string
	snippet    string // managed block including BEGIN/END markers
	bypassHint string
}

// detectShell picks the shell from (in order) the explicit override,
// $SHELL's basename if supported, else "bash". Falls back to bash for
// unknown shells because bash is the spec's nominal default and the
// user can always rerun with --shell= if wrong.
func detectShell(override string) string {
	if override != "" {
		return override
	}
	base := filepath.Base(os.Getenv("SHELL"))
	switch base {
	case "bash", "zsh", "fish":
		return base
	}
	return "bash"
}

// buildAliasPlan returns the rc path + managed-block content for shell.
// Returns an error for shells outside {bash, zsh, fish}.
func buildAliasPlan(shell, home string) (aliasPlan, error) {
	switch shell {
	case "bash":
		return aliasPlan{
			shell:      "bash",
			rcPath:     filepath.Join(home, ".bashrc"),
			snippet:    aliasBeginMarker + "\nalias claude='rein run -- claude'\n" + aliasEndMarker,
			bypassHint: "`\\claude` (literal backslash before the name)",
		}, nil
	case "zsh":
		return aliasPlan{
			shell:      "zsh",
			rcPath:     filepath.Join(home, ".zshrc"),
			snippet:    aliasBeginMarker + "\nalias claude='rein run -- claude'\n" + aliasEndMarker,
			bypassHint: "`\\claude` (literal backslash before the name)",
		}, nil
	case "fish":
		// fish autoloads functions from $XDG_CONFIG_HOME/fish/functions/<name>.fish.
		// Putting the function there (rather than in config.fish) means: no
		// per-shell-startup re-evaluation; the function integrates with fish's
		// `functions --erase` and `funced` UX; the file is conventionally rein's
		// alone (one function per file is fish's strong convention). BEGIN/END
		// markers stay so we can recognize the file as ours on re-run vs a
		// user-authored claude.fish.
		xdg := os.Getenv("XDG_CONFIG_HOME")
		if xdg == "" {
			xdg = filepath.Join(home, ".config")
		}
		return aliasPlan{
			shell:      "fish",
			rcPath:     filepath.Join(xdg, "fish", "functions", "claude.fish"),
			snippet:    aliasBeginMarker + "\nfunction claude\n    rein run -- claude $argv\nend\n" + aliasEndMarker,
			bypassHint: "`command claude`",
		}, nil
	}
	return aliasPlan{}, fmt.Errorf("unsupported shell %q (want bash, zsh, or fish; use --shell= to override)", shell)
}

// spliceManagedBlock removes all existing BEGIN/END regions from body
// and writes a single fresh snippet in their place (at the position of
// the FIRST removed block, or appended if none existed). The strip-all
// pass means a buggy prior run that wrote two blocks self-heals on the
// next splice instead of becoming permanently un-runnable.
//
// Returns an error if any BEGIN marker is present without a matching
// END — that's a malformed file (likely a hand-edit gone wrong) and we
// should surface it rather than silently mangle.
func spliceManagedBlock(body []byte, snippet string) ([]byte, error) {
	text := string(body)
	stripped, firstBeginPos, err := stripAllManagedBlocks(text)
	if err != nil {
		return nil, err
	}
	if firstBeginPos == -1 {
		if len(stripped) > 0 && !strings.HasSuffix(stripped, "\n") {
			stripped += "\n"
		}
		return []byte(stripped + snippet + "\n"), nil
	}
	// stripAllManagedBlocks consumes the END's trailing \n to avoid
	// blank-line gaps; add it back here so the inserted snippet stays
	// separated from whatever followed the original block (or from
	// EOF if nothing followed).
	return []byte(stripped[:firstBeginPos] + snippet + "\n" + stripped[firstBeginPos:]), nil
}

// stripAllManagedBlocks removes every BEGIN…END managed region from
// text and returns the cleaned text plus the byte offset where the
// FIRST removed block started (-1 if none existed). A BEGIN with no
// matching END returns an error — same malformed-file signal the
// splice path surfaces.
func stripAllManagedBlocks(text string) (string, int, error) {
	firstBeginPos := -1
	for {
		beginIdx := strings.Index(text, aliasBeginMarker)
		if beginIdx == -1 {
			return text, firstBeginPos, nil
		}
		if firstBeginPos == -1 {
			firstBeginPos = beginIdx
		}
		tail := text[beginIdx:]
		endOff := strings.Index(tail, aliasEndMarker)
		if endOff == -1 {
			return "", -1, fmt.Errorf("found %q but no matching %q; refusing to edit (fix the file by hand and re-run)", aliasBeginMarker, aliasEndMarker)
		}
		endIdx := beginIdx + endOff + len(aliasEndMarker)
		// Also consume a trailing newline if present so consecutive
		// blocks don't leave gappy blank lines behind.
		if endIdx < len(text) && text[endIdx] == '\n' {
			endIdx++
		}
		text = text[:beginIdx] + text[endIdx:]
	}
}

// hasForeignClaudeAlias reports whether body defines a `claude` alias,
// function, or shell function outside rein's managed block. Lines
// starting with `#` are skipped to avoid false-positives on commented-
// out examples. ALL managed blocks are stripped first, so a duplicate
// rein block isn't mis-attributed to the user.
//
// Forms detected:
//   - bash/zsh:  `alias claude=...` / `alias claude ...`
//   - zsh:       `alias -g claude=...` (global alias)
//   - fish:      `function claude ...`
//   - POSIX/bash function: `claude()` / `claude ()` / `claude () {`
func hasForeignClaudeAlias(body []byte) bool {
	stripped, _, err := stripAllManagedBlocks(string(body))
	if err != nil {
		// Malformed file. Be conservative: treat as foreign so the
		// caller skips the write rather than racing the malformed
		// state.
		return true
	}
	for _, line := range strings.Split(stripped, "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		if strings.HasPrefix(s, "alias claude=") || strings.HasPrefix(s, "alias claude ") {
			return true
		}
		// zsh global alias: `alias -g claude=...` (and other flag
		// combinations starting with `-`).
		if strings.HasPrefix(s, "alias -") {
			fields := strings.Fields(s)
			for _, f := range fields[1:] {
				if strings.HasPrefix(f, "claude=") || f == "claude" {
					return true
				}
				if strings.HasPrefix(f, "-") {
					continue
				}
				break
			}
		}
		// fish: `function claude` (field-aware to avoid matching
		// `function claudette`).
		fields := strings.Fields(s)
		if len(fields) >= 2 && fields[0] == "function" && fields[1] == "claude" {
			return true
		}
		// POSIX/bash function: `claude()` / `claude ()` / `claude () {`.
		// Strip leading whitespace already done; check for the name
		// followed by zero-or-more spaces then `(`.
		if strings.HasPrefix(s, "claude(") || strings.HasPrefix(s, "claude (") {
			return true
		}
	}
	return false
}

// aliasOutcome reports what installShellAlias did.
//   - active: true iff a rein-managed alias is in place on disk after
//     the call (freshly written, freshly updated, or already correct).
//     Callers use this to decide between "open a new shell" and
//     "rein run --" advice in the user-facing summary.
//   - changed: true iff the rc file actually got rewritten this call.
//     Callers use this to gate "informational" WARNings that should
//     only fire when something was newly written, not on every no-op
//     re-run.
type aliasOutcome struct {
	summary string
	active  bool
	changed bool
}

// installShellAlias writes (or refreshes) the managed alias block in
// the rc file dictated by `plan`. Returns an aliasOutcome summarizing
// what happened and whether the alias is now active on disk.
//
// File handling: read existing → splice block → atomic write. Mode is
// preserved if the file exists (typical 0644 stays 0644); 0644 default
// when creating. Parent directory created 0755 if missing — needed for
// fish's $XDG_CONFIG_HOME/fish/ which may not exist yet.
func installShellAlias(plan aliasPlan) (aliasOutcome, error) {
	if err := os.MkdirAll(filepath.Dir(plan.rcPath), 0o755); err != nil {
		return aliasOutcome{}, fmt.Errorf("create %s: %w", filepath.Dir(plan.rcPath), err)
	}

	var existing []byte
	// Mode preservation: if the rc file exists, keep its mode bits as-is
	// — the user's intent (typical 0644; occasionally 0640 in shared-group
	// setups) wins over our default. When creating, 0644 matches the
	// conventional rc-file mode.
	mode := os.FileMode(0o644)
	if info, err := os.Stat(plan.rcPath); err == nil {
		b, rerr := os.ReadFile(plan.rcPath)
		if rerr != nil {
			return aliasOutcome{}, fmt.Errorf("read %s: %w", plan.rcPath, rerr)
		}
		existing = b
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return aliasOutcome{}, fmt.Errorf("stat %s: %w", plan.rcPath, err)
	}

	if hasForeignClaudeAlias(existing) {
		return aliasOutcome{
			summary: fmt.Sprintf("skipped — %s already defines `claude` outside rein's managed block; remove that line and re-run, or use --no-alias to skip", plan.rcPath),
		}, nil
	}

	updated, err := spliceManagedBlock(existing, plan.snippet)
	if err != nil {
		return aliasOutcome{}, err
	}
	if bytes.Equal(updated, existing) {
		return aliasOutcome{
			summary: fmt.Sprintf("managed block in %s already current", plan.rcPath),
			active:  true,
		}, nil
	}

	if err := atomicWriteFile(plan.rcPath, updated, mode); err != nil {
		return aliasOutcome{}, err
	}
	verb := "updated"
	if len(existing) == 0 || !bytes.Contains(existing, []byte(aliasBeginMarker)) {
		verb = "added"
	}
	return aliasOutcome{
		summary: fmt.Sprintf("%s managed block in %s (bypass with %s; open a new shell or `source %s` to activate)", verb, plan.rcPath, plan.bypassHint, plan.rcPath),
		active:  true,
		changed: true,
	}, nil
}

// atomicWriteFile writes data to path via a temp file + rename, so a
// crash mid-write cannot truncate the user's rc file.
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".rein-tmp-*")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	return os.Rename(tmpName, path)
}
