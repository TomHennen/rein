// Package sandboxutil holds substrate-neutral pieces shared by the sandbox
// launch/preflight spine (the CA bundle, the CA-trust env vars, the preflight
// Check/Status types, the extra-egress resolver) so backend and command code
// share one copy. Established by the shared-substrate extraction in
// docs/design-nono-pivot.md §5/§7.
package sandboxutil

// Status is a preflight verdict, matching doctor's three-value framing.
type Status int

const (
	StatusOK Status = iota
	StatusWarn
	StatusFail
)

// Check is one preflight result: a stable name, a verdict, and a message that
// on failure names the exact fix (the loud-degrade requirement).
type Check struct {
	Name    string
	Status  Status
	Message string
}

// Hard reports whether this check is a hard gate for launching a sandboxed run.
// A StatusFail on a hard check must fail the launch closed; StatusWarn never
// blocks.
func (c Check) Hard() bool { return c.Status == StatusFail }
