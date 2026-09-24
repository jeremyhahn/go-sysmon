package monitor

import "time"

// Ticker is the poll loop's ticker, exported to the external tests so they
// can pace the loop by hand.
type Ticker = ticker

// SubscriberBufSize is subscriberBufSize, exported to the external tests.
const SubscriberBufSize = subscriberBufSize

// SetTicker makes every later run of m's poll loop use a ticker from start
// instead of a time.Ticker. It must be called before Start.
func SetTicker(m *Monitor, start func(time.Duration) Ticker) {
	m.newTicker = start
}

// SetFanningOut makes m call f each time a publication has been stored and is
// about to be broadcast. It must be called before Start.
func SetFanningOut(m *Monitor, f func()) {
	m.fanningOut = f
}
