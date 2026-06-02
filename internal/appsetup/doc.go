// Package appsetup orchestrates `rein init`'s GitHub App manifest flow:
// builds the templated browser POST, runs the ephemeral loopback
// listener, exchanges the temporary code, persists the PEM (via
// internal/keystore) and the marker state.json, and bridges to
// Phase 0's REIN_APP_* env-var path.
//
// Two Apps per invocation (primary + audit), sequential. Each gets its
// own ephemeral port and fresh state nonce; the user sees [1/2] / [2/2]
// framing on stdout.
//
// Authoritative spec: docs/init-manifest-design.md. Companion empirical
// research: docs/rein-manifest-flow-research.md.
//
// Scope boundaries this package does not cross:
//   - Token minting (internal/githubapp).
//   - Loading env-var App config (internal/config).
//   - Local scaffolding (cmd/rein/init.go: shim install, symlink, alias).
//   - Install polling, machines list, status, --import: Stage 2 followups.
package appsetup
