# git_author — delegated commit author "(via rein)" (COVERED)

A sandboxed agent's commit is NON-impersonating. rein stamps
`GIT_AUTHOR_*`/`GIT_COMMITTER_*` (`internal/srt/env.go`, from `internal/gitidentity`)
to `<developer name> (via rein)` + the App-bot NOREPLY email
(`<id>+<slug>[bot]@users.noreply.github.com`), **NEVER** the developer's real email.

The agent prints `git log -1 --format='%an <%ae>'` (visible in the golden); after the
push, the HOST asserts the same identity on the pushed commit via the API — and that
it is NOT the developer. Direct mode differs (it layers the real `~/.gitconfig`, so
commits author as the developer), which is why this runs sandboxed. RAW golden,
normalize-on-compare.

**Golden contract.** Exit **0** = match (normalized), **1** = drift, **2** = broke.

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.git_author.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.git_author.journey   # regenerate the RAW golden
```
