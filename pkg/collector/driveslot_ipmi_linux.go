//go:build linux && (amd64 || arm64)

package collector

import (
	"context"
	"errors"
	"os"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The ipmi_devintf ABI (include/uapi/linux/ipmi.h) on a 64-bit kernel.
const (
	ipmiSystemInterfaceAddrType = 0x0c
	ipmiBMCChannel              = 0x0f
	ipmiResponseRecvType        = 1
	ipmiMaxMsgLength            = 272

	// ipmiResponseTimeout bounds one exchange when the context sets no
	// earlier deadline. The kernel itself gives up on the BMC after about
	// five seconds and delivers an error response.
	ipmiResponseTimeout = 10 * time.Second
)

// ipmiSystemInterfaceAddr is struct ipmi_system_interface_addr.
type ipmiSystemInterfaceAddr struct {
	addrType int32
	channel  int16
	lun      uint8
}

// ipmiMsg is struct ipmi_msg.
type ipmiMsg struct {
	netfn   uint8
	cmd     uint8
	dataLen uint16
	data    *byte
}

// ipmiReq is struct ipmi_req.
type ipmiReq struct {
	addr    *byte
	addrLen uint32
	msgid   int64
	msg     ipmiMsg
}

// ipmiRecv is struct ipmi_recv.
type ipmiRecv struct {
	recvType int32
	addr     *byte
	addrLen  uint32
	msgid    int64
	msg      ipmiMsg
}

// ipmiIOC builds an ioctl number for the 'i' magic, as _IOC does.
func ipmiIOC(dir, nr, size uintptr) uintptr {
	return dir<<30 | size<<16 | uintptr('i')<<8 | nr
}

var (
	// IPMICTL_SEND_COMMAND is _IOR('i', 13, struct ipmi_req), and
	// IPMICTL_RECEIVE_MSG_TRUNC is _IOWR('i', 11, struct ipmi_recv).
	ipmictlSendCommand     = ipmiIOC(2, 13, unsafe.Sizeof(ipmiReq{}))
	ipmictlReceiveMsgTrunc = ipmiIOC(3, 11, unsafe.Sizeof(ipmiRecv{}))
)

// ipmiDevice is the kernel IPMI device, open for one poll.
type ipmiDevice struct {
	f     *os.File
	msgid int64
}

// openIPMIDevice opens the device read-only. ipmi_devintf does not check the
// open mode for its ioctls, and nothing this collector sends needs more:
// read-only is the least the service can be granted.
func openIPMIDevice(path string) (ipmiTransport, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &IPMIDeviceError{Path: path, Err: err}
	}
	return &ipmiDevice{f: f}, nil
}

func (d *ipmiDevice) Close() error { return d.f.Close() }

// exchange sends one request to the BMC on the system interface and waits for
// its response, which it returns completion code first.
func (d *ipmiDevice) exchange(ctx context.Context, lun, netfn, cmd uint8, data []byte) ([]byte, error) {
	d.msgid++
	if err := d.send(lun, netfn, cmd, data); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(ipmiResponseTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, &IPMIIoctlError{Op: "wait for response", Err: err}
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			return nil, &IPMITimeoutError{NetFn: netfn, Cmd: cmd}
		}
		fds := []unix.PollFd{{Fd: int32(d.f.Fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(wait.Milliseconds())+1)
		if errors.Is(err, unix.EINTR) || n == 0 {
			continue
		}
		if err != nil {
			return nil, &IPMIIoctlError{Op: "poll", Err: err}
		}
		resp, msgid, ok, err := d.receive()
		if err != nil {
			return nil, err
		}
		// A response to an earlier request that timed out is discarded.
		if ok && msgid == d.msgid {
			return resp, nil
		}
	}
}

// send issues IPMICTL_SEND_COMMAND.
func (d *ipmiDevice) send(lun, netfn, cmd uint8, data []byte) error {
	addr := ipmiSystemInterfaceAddr{addrType: ipmiSystemInterfaceAddrType, channel: ipmiBMCChannel, lun: lun}
	req := ipmiReq{
		addr:    (*byte)(unsafe.Pointer(&addr)),
		addrLen: uint32(unsafe.Sizeof(addr)),
		msgid:   d.msgid,
		msg:     ipmiMsg{netfn: netfn, cmd: cmd, dataLen: uint16(len(data))},
	}
	if len(data) > 0 {
		req.msg.data = &data[0]
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, d.f.Fd(), ipmictlSendCommand, uintptr(unsafe.Pointer(&req)))
	runtime.KeepAlive(&addr)
	runtime.KeepAlive(data)
	if errno != 0 {
		return &IPMIIoctlError{Op: "IPMICTL_SEND_COMMAND", Err: errno}
	}
	return nil
}

// receive issues IPMICTL_RECEIVE_MSG_TRUNC. ok is false for a message that is
// not a command response, such as an event the BMC queued.
func (d *ipmiDevice) receive() (resp []byte, msgid int64, ok bool, err error) {
	var addr ipmiSystemInterfaceAddr
	buf := make([]byte, ipmiMaxMsgLength)
	recv := ipmiRecv{
		addr:    (*byte)(unsafe.Pointer(&addr)),
		addrLen: uint32(unsafe.Sizeof(addr)),
		msg:     ipmiMsg{dataLen: uint16(len(buf)), data: &buf[0]},
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, d.f.Fd(), ipmictlReceiveMsgTrunc, uintptr(unsafe.Pointer(&recv)))
	runtime.KeepAlive(&addr)
	runtime.KeepAlive(buf)
	// EMSGSIZE means the message was truncated to the buffer; a drive-slot
	// response is never that long, so it is not the one being waited for.
	if errno != 0 && errno != unix.EMSGSIZE {
		return nil, 0, false, &IPMIIoctlError{Op: "IPMICTL_RECEIVE_MSG_TRUNC", Err: errno}
	}
	if errno != 0 || recv.recvType != ipmiResponseRecvType {
		return nil, recv.msgid, false, nil
	}
	return buf[:min(int(recv.msg.dataLen), len(buf))], recv.msgid, true, nil
}
