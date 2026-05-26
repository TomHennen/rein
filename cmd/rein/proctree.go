// Process-tree walk for `detectWriteIntent`'s fallback path.
//
// The Linux and macOS implementations live in proctree_linux.go and
// proctree_darwin.go respectively; all other platforms get a no-op
// stub in proctree_other.go. Each file defines:
//
//   - func detectFromProcTree() (bool, string)
//   - const procTreePlatform string  // "linux" / "darwin" / "unsupported"
//
// Trust model: we walk argv of ancestor processes looking for `git push`
// or `git send-pack`. This is a ROUTING signal, not a security boundary.
// An attacker who fakes their argv to look like `git push` only gets the
// wrong token tier minted (write instead of read); the token's
// permissions ceiling is still enforced server-side by GitHub.
//
// Depth cap: six levels up the tree. Enough to cover normal `agent →
// bash → git → http-backend` chains plus a couple of intermediate
// wrappers, but small enough to stop quickly when the chain is shallow
// (e.g. a top-level shell pid).

package main

const procTreeDepth = 6
