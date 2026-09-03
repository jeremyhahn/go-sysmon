//go:build !linux || !(amd64 || arm64) || nomegaraid

package collector

import "github.com/jeremyhahn/go-sysmon/pkg/types"

// Hardware RAID members are not read on this target.
//
// The MFI passthrough in megaraid_linux.go encodes the 64-bit Linux ABI: a
// 16-byte struct iovec, a 404-byte packet, and an ioctl number that carries
// that size. All three differ on a 32-bit target, and a packet laid out for the
// wrong ABI is not a subtle error -- the controller acts on whatever it finds
// at each offset.
//
// So the implementation is built only where its layout is the real one, and
// this stands in elsewhere. A host that would have had members simply reports
// none, which is the same thing every host without a RAID controller reports.
//
// THE `nomegaraid` TAG EXISTS TO MAKE THIS FILE COMPILABLE. pkg/collector
// cannot currently be built for a 32-bit target at all -- gpu.go imports NVML,
// which does not compile there (#8) -- so "the unsupported path builds" was a
// claim nothing could check. It was wrong when it was written: DiskCollector
// carries a megaraidSessions field whose type lived only in the 64-bit file,
// so the stub target failed with `undefined: megaraidSession`.
//
// Selecting a file is not the same as compiling it, and `go list` only proved
// the former. `go build -tags nomegaraid ./...` proves the latter, on any host.

// megaraidSession is the type DiskCollector's field refers to. It is declared
// here as well because that field is in disk.go, which has no build tag and is
// therefore compiled on every target.
type megaraidSession struct{}

func (c *DiskCollector) applyHardwareRAID(string, *types.DiskInfo) {}

// beginMegaRAID and endMegaRAID bracket a collect cycle's controller sessions
// on the supported targets, and do nothing here.
func (c *DiskCollector) beginMegaRAID() {}
func (c *DiskCollector) endMegaRAID()   {}
