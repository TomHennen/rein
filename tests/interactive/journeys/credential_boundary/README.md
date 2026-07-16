# credential_boundary — the credential hide, proven by an INDEPENDENT scanner (COVERED)

The complementary evidence to `sandbox_filesystem`: it proves the same credential
hide with an INDEPENDENT third-party scanner (`bagel`) run as a **differential** —
finds **4** planted creds `--direct`, **0** sandboxed. A sweep catches un-enumerated
paths the hand-written `cat`-checks can't (the #55 unknown-unknown class). Two
`run_journey` steps (direct vs sandboxed) into one RAW golden; normalize-on-compare.

**`bagel` is GPL-3.0 and used ONLY as an external CLI — never a go.mod dep**
(hard-constraint #4). Without it the journey **exits 3 = SKIP** (the runner prints
"did NOT run"; never reported as PASS). Install it:

```sh
go install github.com/boostsecurityio/bagel/cmd/bagel@latest
```

**Golden contract.** Exit **0** = match (normalized), **1** = drift, **2** = broke, **3** = SKIP if `bagel` absent.

Run (from repo root):

```sh
python3 -m tests.interactive.journeys.credential_boundary.journey                   # exit 0 == matches golden (3 = SKIP if no bagel)
REIN_UPDATE_GOLDEN=1 python3 -m tests.interactive.journeys.credential_boundary.journey   # regenerate the RAW golden
```
