//go:build nvml

// Tests for the NVML value decoders, which exist only in the nvml build.
//
// They live here rather than in gpu_test.go because that file must compile
// without cgo: the point of the tag split (#8) is that `CGO_ENABLED=0 go build`
// -- and `go vet`, which builds the test binary -- work on this package, and an
// nvml import in any file in it defeats that however few lines use it.

package collector

import (
	"testing"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// nvmlBusIDString tests
// ---------------------------------------------------------------------------

func TestNvmlBusIDString_NullTerminated(t *testing.T) {
	t.Parallel()
	// Simulates NVML's 32-element C char array: content followed by nulls.
	// The element type is int8 because cgo maps C.char that way.
	var b [32]int8
	for i, c := range []byte("0000:02:00.0") {
		b[i] = int8(c)
	}
	got := nvmlBusIDString(b[:])
	want := "0000:02:00.0"
	if got != want {
		t.Errorf("nvmlBusIDString = %q, want %q", got, want)
	}
}

func TestNvmlBusIDString_EmptyArray(t *testing.T) {
	t.Parallel()
	var b [32]int8
	got := nvmlBusIDString(b[:])
	if got != "" {
		t.Errorf("nvmlBusIDString of all-zero array = %q, want %q", got, "")
	}
}

// ---------------------------------------------------------------------------
// nvmlComputeModeString tests
// ---------------------------------------------------------------------------

func TestNvmlComputeModeString_Known(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode nvml.ComputeMode
		want string
	}{
		{nvml.COMPUTEMODE_DEFAULT, "Default"},
		{nvml.COMPUTEMODE_EXCLUSIVE_THREAD, "Exclusive Thread"},
		{nvml.COMPUTEMODE_PROHIBITED, "Prohibited"},
		{nvml.COMPUTEMODE_EXCLUSIVE_PROCESS, "Exclusive Process"},
	}
	for _, tc := range cases {
		got := nvmlComputeModeString(tc.mode)
		if got != tc.want {
			t.Errorf("nvmlComputeModeString(%d) = %q, want %q", tc.mode, got, tc.want)
		}
	}
}

func TestNvmlComputeModeString_Unknown(t *testing.T) {
	t.Parallel()
	got := nvmlComputeModeString(nvml.ComputeMode(99))
	if got != "Unknown" {
		t.Errorf("nvmlComputeModeString(99) = %q, want %q", got, "Unknown")
	}
}

// ---------------------------------------------------------------------------
// nvmlPerfStateString tests
// ---------------------------------------------------------------------------

func TestNvmlPerfStateString_P0(t *testing.T) {
	t.Parallel()
	got := nvmlPerfStateString(nvml.PSTATE_0)
	if got != "P0" {
		t.Errorf("nvmlPerfStateString(PSTATE_0) = %q, want %q", got, "P0")
	}
}

func TestNvmlPerfStateString_P15(t *testing.T) {
	t.Parallel()
	got := nvmlPerfStateString(nvml.PSTATE_15)
	if got != "P15" {
		t.Errorf("nvmlPerfStateString(PSTATE_15) = %q, want %q", got, "P15")
	}
}

func TestNvmlPerfStateString_Unknown(t *testing.T) {
	t.Parallel()
	got := nvmlPerfStateString(nvml.PSTATE_UNKNOWN)
	if got != "Unknown" {
		t.Errorf("nvmlPerfStateString(PSTATE_UNKNOWN) = %q, want %q", got, "Unknown")
	}
}
