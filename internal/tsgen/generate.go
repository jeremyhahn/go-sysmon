package tsgen

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// Roots are the wire types the dashboard receives, and everything reachable
// from them is what gets emitted.
//
// Snapshot is the stream. The rest are REST payloads the same TypeScript
// declares, and leaving them hand-written beside a generated file would invite
// exactly the assumption this change removes -- that everything next to a
// generated file is generated.
var Roots = []string{
	"Snapshot",
	"Fleet",
	"PeerSnapshot",
	"ConfigView",
	"History",
	"HistoryCatalog",
	"HistorySeries",
}

// Output names the files a run writes, relative to the frontend's src/lib.
const (
	TypesFile  = "types.generated.ts"
	SchemaFile = "schema.generated.ts"
	// UnionsModule is the specifier the generated types import their
	// hand-written union aliases from.
	UnionsModule = "./unions"
)

// Generate parses source and writes both emitted files into libDir.
//
// The pair is replaced together or not at all: a schema regenerated beside
// interfaces that were not is worse than neither, because the two would
// disagree while both looked current. Both files are staged in libDir first,
// so every failure that depends on content, space or permissions happens
// before either committed file changes; only the final renames remain.
func Generate(source, libDir string) (err error) {
	model, err := Parse(source, Roots)
	if err != nil {
		return err
	}

	outputs := []struct{ path, content string }{
		{filepath.Join(libDir, TypesFile), RenderTypes(model, UnionsModule)},
		{filepath.Join(libDir, SchemaFile), RenderSchema(model)},
	}

	staged := make([]string, 0, len(outputs))
	defer func() {
		// A staged file that was renamed no longer exists; one that was not is
		// removed, and a failure to remove it is reported with the rest.
		for _, tmp := range staged {
			if rmErr := os.Remove(tmp); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
				err = errors.Join(err, &WriteError{Path: tmp, Cause: rmErr})
			}
		}
	}()
	for _, out := range outputs {
		tmp, err := stageFile(out.path, out.content)
		if err != nil {
			return err
		}
		staged = append(staged, tmp)
	}
	targets := make([]string, len(outputs))
	for i, out := range outputs {
		targets[i] = out.path
	}
	return replaceAll(staged, targets)
}

// rename is os.Rename, held in a variable so a test can make one rename fail
// and prove the pair is restored.
var rename = os.Rename

// replaceAll moves each staged file onto its target, all or nothing. Every
// existing target is first moved aside to a backup beside it; if any later
// replacement fails, the targets already replaced are put back from their
// backups, and a target that did not exist before is removed again. Backups
// are deleted once the outcome is settled.
func replaceAll(staged, targets []string) (err error) {
	backups := make([]string, len(targets))
	defer func() {
		// Only a settled success discards the backups. After a failure, a
		// backup that was restored has already been renamed away, and one that
		// could not be restored is the sole copy of the original: it stays,
		// and the error names it.
		if err != nil {
			return
		}
		for _, b := range backups {
			if b == "" {
				continue
			}
			if rmErr := os.Remove(b); rmErr != nil {
				err = errors.Join(err, &WriteError{Path: b, Cause: rmErr})
			}
		}
	}()

	for i, target := range targets {
		backup, backupErr := moveAside(target)
		if backupErr != nil {
			return errors.Join(backupErr, restore(targets[:i], backups[:i]))
		}
		backups[i] = backup
		if renameErr := rename(staged[i], target); renameErr != nil {
			return errors.Join(&WriteError{Path: target, Cause: renameErr}, restore(targets[:i+1], backups[:i+1]))
		}
	}
	return nil
}

// moveAside renames an existing target to a fresh backup name beside it and
// returns that name, or "" when there was no target to keep.
func moveAside(target string) (string, error) {
	if _, err := os.Lstat(target); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	f, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".bak.*")
	if err != nil {
		return "", &WriteError{Path: target, Cause: err}
	}
	backup := f.Name()
	if err := f.Close(); err != nil {
		return "", errors.Join(&WriteError{Path: target, Cause: err}, os.Remove(backup))
	}
	if err := rename(target, backup); err != nil {
		return "", errors.Join(&WriteError{Path: target, Cause: err}, os.Remove(backup))
	}
	return backup, nil
}

// restore undoes replacements: each target goes back to its backup, or is
// removed when it had none. It attempts every one and reports all failures.
func restore(targets, backups []string) error {
	var errs []error
	for i := len(targets) - 1; i >= 0; i-- {
		if backups[i] == "" {
			if err := os.Remove(targets[i]); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, &WriteError{Path: targets[i], Cause: err})
			}
			continue
		}
		if err := rename(backups[i], targets[i]); err != nil {
			errs = append(errs, &WriteError{
				Path:  targets[i],
				Cause: errors.Join(err, &fs.PathError{Op: "restore: original kept at", Path: backups[i], Err: fs.ErrExist}),
			})
		}
	}
	return errors.Join(errs...)
}

// stageFile writes content to a new temporary file beside path and returns
// its name. The target itself is checked first: a directory in its place
// would otherwise only fail at the rename, after the other file of the pair
// had been replaced.
//
// 0644 rather than 0600: this is source, it is committed, and it has to be
// readable by whatever runs the build next.
func stageFile(path, content string) (string, error) {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return "", &WriteError{Path: path, Cause: &fs.PathError{Op: "write", Path: path, Err: syscall.EISDIR}}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return "", &WriteError{Path: path, Cause: err}
	}
	name := tmp.Name()
	if _, err := tmp.WriteString(content); err != nil {
		return "", closeAndWrap(tmp, name, path, err)
	}
	// #nosec G302 -- generated TypeScript, checked into the repository
	if err := tmp.Chmod(0o644); err != nil {
		return "", closeAndWrap(tmp, name, path, err)
	}
	if err := tmp.Close(); err != nil {
		return "", closeAndWrap(nil, name, path, err)
	}
	return name, nil
}

// closeAndWrap abandons a staged file after a failed write and reports the
// failure against the file it was meant to replace.
func closeAndWrap(tmp *os.File, name, path string, cause error) error {
	if tmp != nil {
		if err := tmp.Close(); err != nil {
			cause = errors.Join(cause, err)
		}
	}
	if err := os.Remove(name); err != nil {
		cause = errors.Join(cause, err)
	}
	return &WriteError{Path: path, Cause: cause}
}
