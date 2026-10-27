//go:build !linux || !(amd64 || arm64)

package collector

// In-band IPMI is not read on this target; Redfish still is.
func openIPMIDevice(path string) (ipmiTransport, error) {
	return nil, &IPMIDeviceError{Path: path, Err: &IPMIUnsupportedError{}}
}
