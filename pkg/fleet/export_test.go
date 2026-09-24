package fleet

import "time"

// ManualClock exposes manualClock to the external tests.
type ManualClock = manualClock

// NewManualClock exposes newManualClock to the external tests.
var NewManualClock = newManualClock

// SetClock replaces p's clock. Call it before p polls.
func SetClock(p *Poller, now func() time.Time) {
	p.now = now
}

// Refresh runs one poll cycle of p against its current configuration, so an
// external test can step the poller cycle by cycle instead of racing Run.
func Refresh(p *Poller) {
	p.refresh(p.cfgFn())
}
