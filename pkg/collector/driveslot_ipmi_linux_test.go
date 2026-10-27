//go:build linux && (amd64 || arm64)

package collector

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestIPMIIoctlNumbers_MatchTheKernelABI(t *testing.T) {
	// The values <linux/ipmi.h> yields on a 64-bit kernel, as ipmitool and
	// freeipmi compile them.
	if ipmictlSendCommand != 0x8028690d {
		t.Errorf("IPMICTL_SEND_COMMAND = %#x, want 0x8028690d", ipmictlSendCommand)
	}
	if ipmictlReceiveMsgTrunc != 0xc030690b {
		t.Errorf("IPMICTL_RECEIVE_MSG_TRUNC = %#x, want 0xc030690b", ipmictlReceiveMsgTrunc)
	}
}

func TestOpenIPMIDevice_MissingDeviceIsNotExist(t *testing.T) {
	_, err := openIPMIDevice(filepath.Join(t.TempDir(), "ipmi0"))
	var devErr *IPMIDeviceError
	if !errors.As(err, &devErr) || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("openIPMIDevice(missing) error = %v, want IPMIDeviceError wrapping fs.ErrNotExist", err)
	}
}

func TestIPMIDevice_NotAnIPMIDeviceRefusesTheIoctl(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ipmi0")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	dev, err := openIPMIDevice(path)
	if err != nil {
		t.Fatalf("openIPMIDevice() error = %v", err)
	}
	_, err = dev.exchange(context.Background(), 0, ipmiNetFnStorage, ipmiCmdReserveSDR, []byte{0x01})
	var ioctlErr *IPMIIoctlError
	if !errors.As(err, &ioctlErr) || !errors.Is(err, unix.ENOTTY) {
		t.Errorf("exchange() on a regular file error = %v, want IPMIIoctlError wrapping ENOTTY", err)
	}
	if err := dev.Close(); err != nil {
		t.Errorf("Close() error = %v", err)
	}
	if err := dev.Close(); err == nil {
		t.Error("a second Close() succeeded")
	}
}
