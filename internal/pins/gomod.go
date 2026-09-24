package pins

import (
	"os"

	"golang.org/x/mod/modfile"
)

// ModFile holds the toolchain lines of a go.mod.
type ModFile struct {
	// Go is the go directive, "1.27.1". It is the single source of the Go
	// version for everything that builds this module.
	Go string
	// Toolchain is the toolchain directive, "go1.27.1", or empty when absent.
	Toolchain string
}

// ReadModFile reads the go and toolchain directives from the go.mod at path.
// A go.mod without a go directive is an error: there is then nothing for any
// other pin to agree with.
func ReadModFile(path string) (ModFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ModFile{}, &PinFileError{Path: path, Reason: "cannot read", Cause: err}
	}
	f, err := modfile.Parse(path, data, nil)
	if err != nil {
		return ModFile{}, &PinFileError{Path: path, Reason: "cannot parse", Cause: err}
	}
	if f.Go == nil || f.Go.Version == "" {
		return ModFile{}, &PinFileError{Path: path, Reason: "no go directive"}
	}
	mod := ModFile{Go: f.Go.Version}
	if f.Toolchain != nil {
		mod.Toolchain = f.Toolchain.Name
	}
	return mod, nil
}
