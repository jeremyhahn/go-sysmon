package main

import (
	"log/slog"
	"sync"
)

// serviceStatus is the one line systemd shows for this unit -- the sdnotify
// STATUS= text, which `systemctl status` prints under the unit's state. It is
// how a condition of the service that does not stop it serving (history being
// written that no chart can draw, say) is visible to whoever looks at the unit,
// alongside the alert raised for the same condition.
//
// Checks that find such conditions start before the notification socket
// exists, so the text is held here and sent when a sink is attached, and on
// every change after. A nil *serviceStatus is valid and records nothing.
type serviceStatus struct {
	mu   sync.Mutex
	text string
	sink func(string) error
}

// Set replaces the status text and, when a sink is attached, sends it. An
// empty text clears the line.
//
// Sends are made holding the lock, so they reach systemd in the order they
// were set, and none is still in flight once Detach returns -- an older line
// cannot overwrite a newer one, or the "shutting down" sent after Detach. A
// send is one datagram on a local socket.
func (s *serviceStatus) Set(text string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.text = text
	send(s.sink, text)
}

// Text returns the current status text.
func (s *serviceStatus) Text() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text
}

// Attach starts sending the status to sink, beginning with whatever was set
// before it existed.
func (s *serviceStatus) Attach(sink func(string) error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sink = sink
	if s.text != "" {
		send(sink, s.text)
	}
}

// Detach stops sending, so nothing set while the service shuts down replaces
// the "shutting down" line.
func (s *serviceStatus) Detach() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sink = nil
}

// send delivers text to sink. A notification that fails is not a reason to do
// anything else: the condition is also logged and alerted.
func send(sink func(string) error, text string) {
	if sink == nil {
		return
	}
	if err := sink(text); err != nil {
		slog.Debug("watchdog: status notification failed", "err", err)
	}
}
