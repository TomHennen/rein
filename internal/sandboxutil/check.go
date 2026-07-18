// Package sandboxutil holds substrate-neutral pieces shared by every sandbox
// backend (srt today, nono during the pivot) so a new backend's package never
// needs to import another backend's package. Extracted from internal/srt per
// docs/design-nono-pivot.md §5/§7 ("shared-substrate extraction").
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
