# declare_new — `rein declare --new "<title>"` files the run's issue (#180) (COVERED)

The bootstrap for a repo/run with **no issue to declare yet**. `gh issue create` is
write-tier (locked until a declare) and `rein declare <n>` needs a GitHub-assigned
number that doesn't exist. `rein declare --new "<title>" [--body "<text>"]` lets the
agent PROPOSE an issue instead:

- the human sees a Form A prompt naming the repo, the proposed title (shown WHOLE,
  never truncated — what they read is what gets filed) and a body excerpt;
- **the typed approval token is the proposed title's FIRST WORD**
  (`issuemeta.FirstWord`), not a number — there is no number yet. Still typed, still
  undeliverable by a process with no tty;
- on approval, rein files the issue itself — under a broker-minted `issues:write`
  token that never reaches the sandbox — and it becomes **this run's declared
  issue**: writes unlock, and `agent/<n>/<nonce>` is now pushable using the number
  rein just returned;
- the body gets rein's attribution trailer appended (`_Filed by rein on behalf of
  @<owner>._`) — the issue is filed under the App's bot identity, so the body says
  whose work it is.

One real `rein run`, one sandboxed script, four phases: **(1)** `rein declare --new`
(blocks for the human, approved with the real first word) **(2)** push
`agent/$NEW_ISSUE/<nonce>` — `$NEW_ISSUE` is parsed IN-SCRIPT from `rein declare
--new`'s own output, so the push is the in-script proof the filed issue really is
the run's declared issue **(3)** `gh issue view` (read) of the filed issue from
inside the sandbox **(4)** `rein declare --new` again, this time answered with the
WRONG word — folded into the SAME sandboxed run rather than a second `rein run`
launch (a second ~120-180s srt spin-up buys nothing) — must be refused.

- **Golden contract.** Exit **0** = ceremony held AND the normalized fresh run
  matches `golden.txt`; **1** = drift; **2** = the ceremony broke (a phase rc /
  prompt count / GitHub ground truth was wrong). RAW golden, normalize-on-compare
  (real repo/issue/nonce). The proposed **title and body are FIXED, not per-run
  nonce-suffixed** (like `gh_write`'s `PR_TITLE`/`COMMENT_BODY`): a per-run nonce in
  the title would appear un-normalized in the Form A prompt, the typed-answer echo,
  and rein's script re-echo, and nothing in `_NORMALIZE_RULES` normalizes a title
  word BY CONSTRUCTION. Each run files and closes its own fresh-numbered issue, so a
  fixed title never collides across runs.
- **Host-side GROUND TRUTH, independent of the golden**: the filed issue exists with
  the EXACT proposed title, authored by the App's **bot** identity (never the
  developer), its body ends with the attribution trailer, the pushed branch exists
  at GitHub, and the run's own `sandbox-<runID>.log` records both
  `confirmed-new-issue` and `refused-declare-new-denied`.
- **Self-contained.** Files and closes its own issue, deletes the pushed branch, and
  removes its checkout in a `finally`.

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.declare_new.journey                   # exit 0 == matches golden
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.declare_new.journey   # regenerate the RAW golden
```
