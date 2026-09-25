//go:build integration

package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/jeremyhahn/go-sysmon/internal/await"
	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// indexed lists the subcommands that take --index, with the JSON field
// holding their list and the error a missing index reports.
var indexed = []struct {
	command string
	list    func(raw string) (int, error)
}{
	{"cpu", listLen("cpus")},
	{"memory", listLen("dimms")},
	{"storage", topLen},
	{"network", topLen},
	{"gpu", listLen("gpus")},
	{"npu", listLen("npus")},
	{"containers", listLen("containers")},
	{"vms", listLen("vms")},
}

// listLen returns a counter for the array under field of a JSON object.
func listLen(field string) func(string) (int, error) {
	return func(raw string) (int, error) {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &obj); err != nil {
			return 0, err
		}
		var list []json.RawMessage
		if len(obj[field]) == 0 || string(obj[field]) == "null" {
			return 0, nil
		}
		if err := json.Unmarshal(obj[field], &list); err != nil {
			return 0, err
		}
		return len(list), nil
	}
}

// topLen counts a JSON array.
func topLen(raw string) (int, error) {
	var list []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return 0, err
	}
	return len(list), nil
}

// TestCLI_IndexSelectsOneOrFails verifies, for every subcommand that lists
// devices, that --index 0 prints exactly one when the host has any, and that
// an index past the end fails naming what was not found rather than printing
// an empty section.
func TestCLI_IndexSelectsOneOrFails(t *testing.T) {
	for _, tt := range indexed {
		t.Run(tt.command, func(t *testing.T) {
			all := harness.Run(t, harness.Bins.Sysmon, nil, tt.command, "--json")
			if all.Code != 0 {
				t.Fatalf("sysmon %s --json exited %d:\n%s", tt.command, all.Code, all.Output())
			}
			n, err := tt.list(all.Stdout)
			if err != nil {
				t.Fatalf("decode sysmon %s --json: %v\n%s", tt.command, err, all.Stdout)
			}

			if n > 0 {
				one := harness.Run(t, harness.Bins.Sysmon, nil, tt.command, "--json", "--index", "0")
				if got, err := tt.list(one.Stdout); one.Code != 0 || err != nil || got != 1 {
					t.Errorf("sysmon %s --index 0 = %d entries (exit %d, %v), want 1", tt.command, got, one.Code, err)
				}
				text := harness.Run(t, harness.Bins.Sysmon, nil, tt.command, "--index", "0")
				if text.Code != 0 {
					t.Errorf("sysmon %s --index 0 exited %d:\n%s", tt.command, text.Code, text.Output())
				}
			}

			missing := harness.Run(t, harness.Bins.Sysmon, nil, tt.command, "--index", strconv.Itoa(n+7))
			if missing.Code == 0 || !strings.Contains(missing.Output(), "not found") {
				t.Errorf("sysmon %s --index %d exited %d:\n%s\nwant a not-found failure", tt.command, n+7, missing.Code, missing.Output())
			}
		})
	}
}

// TestCLI_TextReports verifies every subcommand's human-readable report
// renders on this host without error and carries its section's heading.
func TestCLI_TextReports(t *testing.T) {
	headings := map[string]string{
		"overview": "System Monitor", "host": "Host", "cpu": "CPU", "memory": "Memory",
		"storage": "Storage", "network": "Network", "gpu": "GPU", "npu": "NPU",
		"containers": "Containers", "vms": "Virtual Machines", "version": "sysmon",
	}
	for command, heading := range headings {
		t.Run(command, func(t *testing.T) {
			r := harness.Run(t, harness.Bins.Sysmon, nil, command)
			if r.Code != 0 || !strings.Contains(r.Stdout, heading) {
				t.Errorf("sysmon %s exited %d without %q:\n%s", command, r.Code, heading, r.Output())
			}
		})
	}
}

// TestCLI_JSONReports verifies every subcommand's --json output is one valid
// JSON document on stdout.
func TestCLI_JSONReports(t *testing.T) {
	for _, command := range []string{"overview", "host", "cpu", "memory", "storage", "network", "gpu", "npu", "containers", "vms", "version"} {
		t.Run(command, func(t *testing.T) {
			r := harness.Run(t, harness.Bins.Sysmon, nil, command, "--json")
			if r.Code != 0 {
				t.Fatalf("sysmon %s --json exited %d:\n%s", command, r.Code, r.Output())
			}
			var doc json.RawMessage
			harness.MustParseJSON(t, r.Stdout, &doc)
		})
	}
}

// TestCLI_NoSubcommandWithADisplayExplainsTheMissingGUI verifies that with a
// display available, the server build says it has no GUI and how to get the
// dashboard, then shows the overview instead of failing.
func TestCLI_NoSubcommandWithADisplayExplainsTheMissingGUI(t *testing.T) {
	r := harness.Run(t, harness.Bins.Sysmon, []string{"DISPLAY=:99"})

	if r.Code != 0 {
		t.Fatalf("sysmon with a display exited %d:\n%s", r.Code, r.Output())
	}
	if !strings.Contains(r.Stderr, "No GUI in this build") || !strings.Contains(r.Stderr, "sysmon serve --addr :8080") {
		t.Errorf("stderr does not explain the missing GUI:\n%s", r.Stderr)
	}
	if !strings.Contains(r.Stdout, "System Monitor") {
		t.Errorf("stdout is not the overview:\n%s", r.Stdout)
	}
}

// clearScreen is what --refresh writes before every frame.
const clearScreen = "\033[H\033[2J"

// TestCLI_RefreshRedrawsUntilInterrupted runs a subcommand with --refresh,
// waits for it to draw more than one frame, and interrupts it the way a user
// at the terminal does: it must stop at once and exit 0.
func TestCLI_RefreshRedrawsUntilInterrupted(t *testing.T) {
	for _, command := range []string{"cpu", "memory", "network", "storage"} {
		t.Run(command, func(t *testing.T) {
			cmd := exec.Command(harness.Bins.Sysmon, command, "--refresh", "0.2")
			cmd.Env = append(os.Environ(), "DISPLAY=", "WAYLAND_DISPLAY=")
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatalf("capture stdout: %v", err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatalf("start sysmon %s --refresh: %v", command, err)
			}

			frames := make(chan int, 16)
			drained := make(chan error, 1)
			go func() { drained <- countFrames(stdout, frames) }()
			for want := 1; want <= 2; want++ {
				if got := await.Recv(t, "frame "+strconv.Itoa(want), frames); got != want {
					t.Fatalf("frame %d reported as %d", want, got)
				}
			}

			if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
				t.Fatalf("interrupt: %v", err)
			}
			if err := await.Recv(t, "the output to end", drained); err != nil {
				t.Errorf("read the output: %v", err)
			}
			if err := cmd.Wait(); err != nil {
				t.Errorf("sysmon %s --refresh after SIGINT: %v, want exit 0", command, err)
			}
		})
	}
}

// countFrames reads r to its end, sending the running count of frames each
// time another clear-screen sequence begins one.
func countFrames(r io.Reader, frames chan<- int) error {
	reader := bufio.NewReader(r)
	var window []byte
	count := 0
	for {
		b, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		window = append(window, b)
		if len(window) > len(clearScreen) {
			window = window[1:]
		}
		if bytes.Equal(window, []byte(clearScreen)) {
			count++
			select {
			case frames <- count:
			default:
			}
		}
	}
}

// TestCLI_AFullOutputIsReported runs every rendering subcommand with its
// standard output on /dev/full, where every write fails with ENOSPC -- a full
// disk under `sysmon cpu > report.txt`. Each must exit non-zero and say which
// report could not be written and why, rather than exiting 0 having written
// nothing.
func TestCLI_AFullOutputIsReported(t *testing.T) {
	reports := map[string]string{"doctor": "cli render doctor"}
	for _, command := range []string{"overview", "host", "cpu", "memory", "storage", "network", "gpu", "npu", "containers", "vms"} {
		reports[command] = "cli render " + command
	}
	for command, names := range reports {
		t.Run(command, func(t *testing.T) {
			full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
			if err != nil {
				t.Fatalf("open /dev/full: %v", err)
			}
			harness.CloseOnCleanup(t, "/dev/full", full)
			var stderr bytes.Buffer
			cmd := exec.Command(harness.Bins.Sysmon, command)
			cmd.Env = append(os.Environ(), "DISPLAY=", "WAYLAND_DISPLAY=")
			cmd.Stdout = full
			cmd.Stderr = &stderr

			var exited *exec.ExitError
			if err := cmd.Run(); !errors.As(err, &exited) || exited.ExitCode() == 0 {
				t.Fatalf("sysmon %s > /dev/full = %v, want a non-zero exit", command, err)
			}
			for _, want := range []string{names, "no space left on device"} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("sysmon %s > /dev/full said %q, want it to include %q", command, stderr.String(), want)
				}
			}
		})
	}
}
