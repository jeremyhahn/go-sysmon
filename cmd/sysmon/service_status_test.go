package main

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/await"
)

func TestServiceStatus_HeldUntilASinkIsAttached(t *testing.T) {
	var sent []string
	sink := func(text string) error { sent = append(sent, text); return nil }

	s := &serviceStatus{}
	s.Set("history cadence check pending")
	s.Attach(sink)
	s.Set("degraded: history too slow")
	s.Set("")
	s.Detach()
	s.Set("after shutdown began")

	want := []string{"history cadence check pending", "degraded: history too slow", ""}
	if len(sent) != len(want) {
		t.Fatalf("sent %q, want %q", sent, want)
	}
	for i := range want {
		if sent[i] != want[i] {
			t.Fatalf("sent %q, want %q", sent, want)
		}
	}
	if s.Text() != "after shutdown began" {
		t.Errorf("Text() = %q", s.Text())
	}
}

func TestServiceStatus_NilAndFailingSinks(t *testing.T) {
	var s *serviceStatus
	s.Set("x")
	s.Attach(func(string) error { return nil })
	s.Detach()
	if s.Text() != "" {
		t.Error("a nil status reported text")
	}

	// A sink that fails does not stop later sends.
	calls := 0
	live := &serviceStatus{}
	live.Attach(func(string) error { calls++; return errors.New("socket gone") })
	live.Set("a")
	live.Set("b")
	if calls != 2 {
		t.Errorf("sink called %d times, want 2", calls)
	}
}

// TestServiceStatus_DetachWaitsForASendInFlight. Once Detach returns, no
// earlier Set may still be delivering: the "shutting down" line sent next
// would be overwritten by it.
func TestServiceStatus_DetachWaitsForASendInFlight(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	var sent []string
	s := &serviceStatus{}
	s.Attach(func(text string) error {
		if text == "slow" {
			close(entered)
			<-release
		}
		mu.Lock()
		sent = append(sent, text)
		mu.Unlock()
		return nil
	})

	go s.Set("slow")
	await.Recv(t, "the slow send to begin", entered)
	detached := make(chan struct{})
	go func() { s.Detach(); close(detached) }()
	// Detach must be parked behind the send in flight -- or the test fails
	// here, having seen it return.
	await.Until(t, "Detach to wait on the send in flight", func(context.Context) bool {
		select {
		case <-detached:
			t.Fatal("Detach returned while a send was still in flight")
		default:
		}
		return await.Parked("(*serviceStatus).Detach(") == 1
	})
	close(release)
	await.Recv(t, "Detach to return once the send finished", detached)
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 1 || sent[0] != "slow" {
		t.Fatalf("sent %q", sent)
	}
}
