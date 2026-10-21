package collector

import (
	"bufio"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// idDatabase is a parsed hwdata identifier database: pci.ids or usb.ids. Both
// share one format: an unindented "vvvv  Vendor" line, tab-indented
// "\tdddd  Device" lines under it, doubly-indented subsystem or interface
// lines, and "C cc  Class" sections. Keys are lower-case hex without "0x".
type idDatabase struct {
	vendors map[string]string
	// devices is keyed "vendor:device".
	devices map[string]string
	classes map[string]string
}

// loadIDDatabase parses the first of paths that opens. A host without the
// database yields empty maps, so every lookup resolves to no name.
func loadIDDatabase(paths []string) idDatabase {
	db := idDatabase{
		vendors: make(map[string]string),
		devices: make(map[string]string),
		classes: make(map[string]string),
	}

	var f *os.File
	var path string
	for _, candidate := range paths {
		opened, err := os.Open(candidate)
		if err == nil {
			f, path = opened, candidate
			break
		}
	}
	if f == nil {
		return db
	}
	// Read-only: a close error loses nothing that was read, and there is no
	// caller to hand it to, since each parse runs once behind a sync.Once.
	defer func() {
		if err := f.Close(); err != nil {
			slog.Debug("hwids: close identifier database", "path", path, "error", err)
		}
	}()

	scanner := bufio.NewScanner(f)
	var vendor string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch {
		case strings.HasPrefix(line, "C "):
			// "C 12  Processing accelerators"
			if id, name, ok := splitIDEntry(strings.TrimPrefix(line, "C ")); ok {
				db.classes[id] = name
			}
			vendor = ""
		case strings.HasPrefix(line, "\t\t"):
			// Subsystem or interface entry; not used.
		case strings.HasPrefix(line, "\t"):
			if vendor == "" {
				continue
			}
			if id, name, ok := splitIDEntry(strings.TrimPrefix(line, "\t")); ok {
				db.devices[vendor+":"+id] = name
			}
		default:
			// Anything unindented that is not a vendor -- usb.ids' later
			// "HID", "L" and "VT" sections, or a malformed line -- ends the
			// current vendor, so the lines under it are not taken as its
			// devices.
			vendor = ""
			if id, name, ok := splitIDEntry(line); ok {
				db.vendors[id] = name
				vendor = id
			}
		}
	}
	// What was parsed before the failure is correct and is kept; names past
	// it resolve to nothing, the same as an identifier the database lacks.
	if err := scanner.Err(); err != nil {
		slog.Warn("hwids: "+filepath.Base(path)+" read stopped early; later names are unresolved", "path", path, "error", err)
	}
	return db
}

// splitIDEntry splits an identifier database entry into its hex identifier and
// name.
func splitIDEntry(line string) (id, name string, ok bool) {
	id, name, found := strings.Cut(strings.TrimSpace(line), " ")
	if !found {
		return "", "", false
	}
	if _, err := strconv.ParseUint(id, 16, 32); err != nil {
		return "", "", false
	}
	return strings.ToLower(id), strings.TrimSpace(name), true
}

// normalizeHexID lower-cases a sysfs identifier and strips its "0x" prefix,
// the form idDatabase keys use.
func normalizeHexID(id string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(id)), "0x")
}
