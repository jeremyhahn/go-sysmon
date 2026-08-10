package collector

import (
	"log/slog"
	"sync"
)

// warnedConditions records which permanent conditions have already been
// reported, keyed by a caller-supplied identifier.
var warnedConditions sync.Map

// warnOnce logs a warning the first time a given condition is seen and stays
// silent afterwards.
//
// Several conditions this collector reports cannot change while the process
// runs: an unreadable SMBIOS table, a SMART ioctl refused for lack of
// privileges. Logging them on every collection cycle produced 12 MB of
// identical lines per day on a 1-second poll and buried anything that mattered.
// The condition is still worth reporting once, because it explains missing
// data in the output.
//
// key must identify the condition, including any parameter that makes it
// distinct — for example the device name, so each disk warns separately.
func warnOnce(logger *slog.Logger, key, msg string, args ...any) {
	if logger == nil {
		logger = slog.Default()
	}
	if _, loaded := warnedConditions.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	logger.Warn(msg, args...)
}

// resetWarnOnce clears the recorded conditions. It exists for tests, which
// would otherwise see state carried between cases.
func resetWarnOnce() {
	warnedConditions.Range(func(k, _ any) bool {
		warnedConditions.Delete(k)
		return true
	})
}
