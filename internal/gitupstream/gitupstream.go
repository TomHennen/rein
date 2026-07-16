// Package gitupstream carries the branch-upstream-tracking intent of a
// sandboxed `git push -u` from the in-sandbox rein-git shim (which STRIPS the
// -u so real git doesn't fault on the read-only .git/config, #64/#102/#119) to
// the host-side rein process, which sets the tracking on the operator's real
// checkout AFTER the run.
//
// The two parties never share memory: rein-git APPENDS a rendezvous line, rein
// READS it post-run. The rendezvous file lives in the sandbox-writable part of
// .git, so the agent can also write it — every value that crosses this boundary
// is therefore UNTRUSTED and must pass Validate before rein acts on it. rein
// only ever writes branch.<local>.remote/.merge (never a code-exec key), so the
// worst a forged line can do is set benign tracking on an already-existing local
// branch to an already-existing remote.
package gitupstream

import (
	"fmt"
	"strings"
)

// Intent is what `git push -u` would have written: for the pushed local branch,
// branch.<Local>.remote=<Remote> and branch.<Local>.merge=<Merge> (a
// refs/heads/... ref).
type Intent struct {
	Remote string
	Local  string
	Merge  string
}

// pushOptionsThatTakeArg are `git push` options whose value is a SEPARATE argv
// token (so the token after them is not a positional). The --name=value form is
// handled generically. Valueless flags (-u, -f, --tags, …) are not listed.
var pushOptionsThatTakeArg = map[string]bool{
	"--repo":         true,
	"-o":             true,
	"--push-option":  true,
	"--receive-pack": true,
	"--exec":         true,
}

// declineFlags mark a push with no single-branch upstream to record: a deletion
// (-d/--delete), or a bulk push (--all/--tags/--mirror) that sets tracking for
// many branches at once. ParsePush handles only the single-refspec form and
// declines these rather than mis-recording the current branch.
var declineFlags = map[string]bool{
	"-d":       true,
	"--delete": true,
	"--all":    true,
	"--tags":   true,
	"--mirror": true,
}

// HasSetUpstream reports whether a `git push` argv carries the upstream-setting
// flag (-u / --set-upstream). Capture + strip are gated on this: a push WITHOUT
// it writes no tracking in real git, so rein must not synthesize any.
func HasSetUpstream(args []string) bool {
	for _, a := range args {
		if a == "-u" || a == "--set-upstream" {
			return true
		}
	}
	return false
}

// ParsePush derives the upstream Intent from a `git push` argv (args[0]=="push").
// currentBranch resolves HEAD to a short branch name (the shim runs real git for
// it); it is only called when needed, and its error makes ParsePush decline.
//
// ok is false when no sensible single-branch upstream can be derived (detached
// HEAD, a delete push, an empty/`:`-only refspec, or currentBranch failing).
// Callers still strip -u regardless — capture is best-effort, the strip is not.
func ParsePush(args []string, currentBranch func() (string, error)) (Intent, bool) {
	// Collect positionals (remote, then first refspec), skipping options; bail on
	// a delete push.
	var positionals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "push" || a == "" {
			continue
		}
		if strings.HasPrefix(a, "-") {
			if declineFlags[a] {
				return Intent{}, false
			}
			if !strings.Contains(a, "=") && pushOptionsThatTakeArg[a] {
				i++ // skip the option's separate-token value
			}
			continue
		}
		positionals = append(positionals, a)
	}

	remote := "origin"
	if len(positionals) > 0 {
		remote = positionals[0]
	}
	var refspec string
	if len(positionals) > 1 {
		refspec = positionals[1]
	}

	// Resolve the local branch and the merge ref from the (optional) refspec.
	// refspec forms handled: "" (current branch), "NAME", "SRC:DST" (with an
	// optional leading '+' force marker on either whole-refspec form).
	refspec = strings.TrimPrefix(refspec, "+")

	resolveLocal := func(src string) (string, bool) {
		if src == "" || src == "HEAD" {
			b, err := currentBranch()
			if err != nil || b == "" {
				return "", false
			}
			return b, true
		}
		return strings.TrimPrefix(src, "refs/heads/"), true
	}

	var local, mergeRef string
	switch {
	case refspec == "":
		l, ok := resolveLocal("")
		if !ok {
			return Intent{}, false
		}
		local, mergeRef = l, "refs/heads/"+l
	case strings.Contains(refspec, ":"):
		src, dst, _ := strings.Cut(refspec, ":")
		if dst == "" { // "src:" is not a normal upstream-setting push
			return Intent{}, false
		}
		l, ok := resolveLocal(src)
		if !ok {
			return Intent{}, false
		}
		local = l
		if strings.HasPrefix(dst, "refs/") {
			mergeRef = dst
		} else {
			mergeRef = "refs/heads/" + dst
		}
	default: // "NAME"
		l, ok := resolveLocal(refspec)
		if !ok {
			return Intent{}, false
		}
		local, mergeRef = l, "refs/heads/"+l
	}

	return Intent{Remote: remote, Local: local, Merge: mergeRef}, true
}

// EncodeLine serializes an Intent as one tab-separated rendezvous line
// (no trailing newline). The fields never legitimately contain a tab or
// newline; ParseLine + Validate reject any that do (e.g. an agent-forged line).
func EncodeLine(in Intent) string {
	return strings.Join([]string{in.Remote, in.Local, in.Merge}, "\t")
}

// ParseLine parses one rendezvous line. It does NOT validate the values
// (that is Validate's job) — it only enforces the 3-field tab shape.
func ParseLine(line string) (Intent, error) {
	parts := strings.Split(line, "\t")
	if len(parts) != 3 {
		return Intent{}, fmt.Errorf("want 3 tab-separated fields, got %d", len(parts))
	}
	return Intent{Remote: parts[0], Local: parts[1], Merge: parts[2]}, nil
}

// Validate is rein's gate before it touches the host .git/config. It rejects
// anything that is not an obviously-safe tracking triple: the remote and local
// branch must be well-formed AND actually exist in the repo (existsRemote /
// existsBranch), and the merge target must be a refs/heads/<valid> ref. This is
// what makes a forged rendezvous line harmless — rein will not create tracking
// for a branch or remote that isn't already there, and never writes any key
// other than branch.<local>.remote/.merge.
func Validate(in Intent, existsRemote, existsBranch func(string) bool) bool {
	if !validRemoteName(in.Remote) || !existsRemote(in.Remote) {
		return false
	}
	if !ValidRefName(in.Local) || !existsBranch(in.Local) {
		return false
	}
	rest := strings.TrimPrefix(in.Merge, "refs/heads/")
	if rest == in.Merge || !ValidRefName(rest) {
		return false
	}
	return true
}

// validRemoteName allows a conservative remote-name charset (no '/').
func validRemoteName(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// ValidRefName is a conservative safe subset of git's ref rules: a
// slash-separated name over [A-Za-z0-9._-], no leading '-', no empty or "."/".."
// component, no leading/trailing slash. Stricter than git; that is fine — a
// legitimate agent branch (agent/<n>/<nonce>) passes, and anything weird is
// simply not honored.
func ValidRefName(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") {
		return false
	}
	if strings.Contains(s, "..") { // git forbids the ".." sequence anywhere in a ref
		return false
	}
	for _, comp := range strings.Split(s, "/") {
		if comp == "" || comp == "." {
			return false
		}
		for _, r := range comp {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			case r == '.' || r == '_' || r == '-':
			default:
				return false
			}
		}
	}
	return true
}
