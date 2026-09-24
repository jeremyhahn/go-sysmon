package pins

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeModFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "go.mod")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadModFile(t *testing.T) {
	p := writeModFile(t, "module example.com/m\n\ngo 1.27.1\n\ntoolchain go1.27.1\n")

	got, err := ReadModFile(p)
	if err != nil {
		t.Fatalf("ReadModFile() error = %v", err)
	}
	if want := (ModFile{Go: "1.27.1", Toolchain: "go1.27.1"}); got != want {
		t.Errorf("ReadModFile() = %+v, want %+v", got, want)
	}
}

func TestReadModFile_WithoutToolchain(t *testing.T) {
	got, err := ReadModFile(writeModFile(t, "module example.com/m\n\ngo 1.27.1\n"))
	if err != nil {
		t.Fatalf("ReadModFile() error = %v", err)
	}
	if got.Toolchain != "" || got.Go != "1.27.1" {
		t.Errorf("ReadModFile() = %+v, want go 1.27.1 and no toolchain", got)
	}
}

func TestReadModFile_Errors(t *testing.T) {
	tests := map[string]string{
		"missing file":    "",
		"no go directive": "module example.com/m\n",
		"unparsable":      "module example.com/m\ngo {\n",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "go.mod")
			if body != "" {
				p = writeModFile(t, body)
			}
			_, err := ReadModFile(p)
			var merr *PinFileError
			if !errors.As(err, &merr) {
				t.Fatalf("ReadModFile() error = %v, want *PinFileError", err)
			}
		})
	}
}
