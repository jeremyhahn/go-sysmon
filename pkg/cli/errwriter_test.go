package cli

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// errAfterN is an io.Writer that succeeds for the first n writes and then
// fails on every subsequent one, simulating a pipe closed mid-render.
type errAfterN struct {
	n     int
	fail  error
	calls int
	buf   bytes.Buffer
}

func (w *errAfterN) Write(p []byte) (int, error) {
	w.calls++
	if w.calls > w.n {
		return 0, w.fail
	}
	return w.buf.Write(p)
}

// ---- errWriter ------------------------------------------------------------

// TestErrWriter_PassesThroughWhenHealthy verifies the wrapper is transparent
// while the underlying writer keeps succeeding.
func TestErrWriter_PassesThroughWhenHealthy(t *testing.T) {
	var buf bytes.Buffer
	ew := newErrWriter(&buf)

	n, err := ew.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write() error = %v, want nil", err)
	}
	if n != 5 {
		t.Errorf("Write() n = %d, want 5", n)
	}
	if got := buf.String(); got != "hello" {
		t.Errorf("underlying buffer = %q, want %q", got, "hello")
	}
	if ew.Err() != nil {
		t.Errorf("Err() = %v, want nil", ew.Err())
	}
}

// TestErrWriter_RecordsFirstErrorAndStopsWriting verifies that the first
// failure is retained and that later writes neither reach the underlying
// writer nor overwrite the recorded error.
func TestErrWriter_RecordsFirstErrorAndStopsWriting(t *testing.T) {
	first := errors.New("broken pipe")
	second := errors.New("a later, different error")

	under := &errAfterN{n: 1, fail: first}
	ew := newErrWriter(under)

	if _, err := ew.Write([]byte("ok")); err != nil {
		t.Fatalf("first Write() error = %v, want nil", err)
	}
	if _, err := ew.Write([]byte("fails")); !errors.Is(err, first) {
		t.Fatalf("second Write() error = %v, want %v", err, first)
	}

	// Swap the underlying failure; the writer must not consult it again.
	under.fail = second
	callsBefore := under.calls

	n, err := ew.Write([]byte("ignored"))
	if n != 0 {
		t.Errorf("Write() after error n = %d, want 0", n)
	}
	if !errors.Is(err, first) {
		t.Errorf("Write() after error = %v, want the first error %v", err, first)
	}
	if under.calls != callsBefore {
		t.Errorf("underlying writer called %d extra times, want 0", under.calls-callsBefore)
	}
	if !errors.Is(ew.Err(), first) {
		t.Errorf("Err() = %v, want %v", ew.Err(), first)
	}
}

// ---- renderer error propagation -------------------------------------------

// TestRenderers_PropagateWriteError is the regression test for renderers that
// return error but previously discarded every write failure. Each renderer is
// driven with a writer that fails immediately.
func TestRenderers_PropagateWriteError(t *testing.T) {
	broken := errors.New("write: broken pipe")
	snap := testSnapshot()

	renderers := map[string]func(io.Writer, *types.Snapshot) error{
		"RenderOverview": RenderOverview,
		"RenderHost":     RenderHost,
		"RenderCPU":      RenderCPU,
		"RenderMemory":   RenderMemory,
		"RenderStorage":  RenderStorage,
		"RenderNetwork":  RenderNetwork,
		"RenderGPU":      RenderGPU,
	}

	for name, render := range renderers {
		t.Run(name, func(t *testing.T) {
			w := &errAfterN{n: 0, fail: broken}
			err := render(w, snap)
			if err == nil {
				t.Fatalf("%s() error = nil, want the write error to propagate", name)
			}
			if !errors.Is(err, broken) {
				t.Errorf("%s() error = %v, want %v", name, err, broken)
			}
		})
	}
}

// TestRenderers_PropagateWriteErrorFromNestedHelper fails the writer partway
// through a render, so the failing write happens inside a nested helper
// (renderDisk, renderInterface, renderGPU or formatTable) rather than in the
// Render* function itself. This is what proves the errWriter reaches the whole
// call tree and not just the top-level writes.
func TestRenderers_PropagateWriteErrorFromNestedHelper(t *testing.T) {
	broken := errors.New("write: broken pipe")
	snap := testSnapshot()

	renderers := map[string]func(io.Writer, *types.Snapshot) error{
		"RenderOverview": RenderOverview,
		"RenderCPU":      RenderCPU,
		"RenderMemory":   RenderMemory,
		"RenderStorage":  RenderStorage,
		"RenderNetwork":  RenderNetwork,
		"RenderGPU":      RenderGPU,
	}

	for name, render := range renderers {
		t.Run(name, func(t *testing.T) {
			// Count the writes a successful render performs, then fail on the
			// second half so the failure lands deep in the call tree.
			var counter errAfterN
			counter.n = 1 << 30
			if err := render(&counter, snap); err != nil {
				t.Fatalf("%s() baseline error = %v, want nil", name, err)
			}
			total := counter.calls
			if total < 4 {
				t.Skipf("%s performs only %d writes; no nested helper to exercise", name, total)
			}

			w := &errAfterN{n: total / 2, fail: broken}
			err := render(w, snap)
			if err == nil {
				t.Fatalf("%s() error = nil after failing at write %d of %d", name, total/2, total)
			}
			if !errors.Is(err, broken) {
				t.Errorf("%s() error = %v, want %v", name, err, broken)
			}
		})
	}
}

// TestRenderers_SucceedOnHealthyWriter confirms the errWriter change did not
// introduce a spurious error on the normal path.
func TestRenderers_SucceedOnHealthyWriter(t *testing.T) {
	snap := testSnapshot()

	renderers := map[string]func(io.Writer, *types.Snapshot) error{
		"RenderOverview": RenderOverview,
		"RenderHost":     RenderHost,
		"RenderCPU":      RenderCPU,
		"RenderMemory":   RenderMemory,
		"RenderStorage":  RenderStorage,
		"RenderNetwork":  RenderNetwork,
		"RenderGPU":      RenderGPU,
	}

	for name, render := range renderers {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := render(&buf, snap); err != nil {
				t.Fatalf("%s() error = %v, want nil", name, err)
			}
			if buf.Len() == 0 {
				t.Errorf("%s() wrote no output", name)
			}
		})
	}
}
