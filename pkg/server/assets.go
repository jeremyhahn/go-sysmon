package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"strings"
)

// hashedAsset matches the filenames the bundler content-addresses: a base
// name, a dash, the build hash, and an extension. Only these may be cached
// indefinitely, because only these are guaranteed to change name when their
// bytes change.
//
// The application entry chunk and its stylesheet are deliberately no longer
// hashed. A new name on every build meant that any two branches touching the
// frontend collided on three paths at once -- add/add on their new bundles,
// delete/delete on the one they both replaced, and modify/modify on
// index.html -- and none of it was resolvable by merging (#55). They are
// served with an entity tag and revalidated instead, so a browser still
// cannot run a stale bundle against a new server.
var hashedAsset = regexp.MustCompile(`^assets/.+-[A-Za-z0-9_-]{8,}\.[A-Za-z0-9]+$`)

// assetPath maps a request path onto the name the asset filesystem knows it
// by. It mirrors what http.FileServerFS does: strip the leading slash, and
// resolve the root to the entry point.
func assetPath(urlPath string) string {
	name := strings.TrimPrefix(path.Clean("/"+urlPath), "/")
	if name == "" {
		return "index.html"
	}
	return name
}

// assetETags computes a strong entity tag for every regular file in assets.
//
// The bundle is embedded in the binary, so the table is computed once and
// stays valid for the life of the process. It is what makes revalidation
// cheap for the files that are no longer content-addressed: an embedded
// filesystem reports a zero modification time, so without an entity tag a
// conditional request carries no validator and every reload re-downloads the
// whole bundle instead of answering 304.
//
// A file that cannot be walked or read is logged and left out of the table
// rather than failing the server. It is then served with no validator, which
// is what every asset did before entity tags existed.
func assetETags(assets fs.FS) map[string]string {
	tags := make(map[string]string)
	if assets == nil {
		return tags
	}

	err := fs.WalkDir(assets, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			slog.Warn("asset etag walk", "err", err, "name", name)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		tag, err := fileETag(assets, name)
		if err != nil {
			slog.Warn("asset etag", "err", err, "name", name)
			return nil
		}
		tags[name] = tag
		return nil
	})
	if err != nil {
		slog.Warn("asset etag walk", "err", err)
	}
	return tags
}

// fileETag returns a quoted strong entity tag derived from the file's bytes.
// Half of a SHA-256 is used: 128 bits is far past the point where a collision
// between two builds of the same bundle is a real concern, and it keeps the
// header short.
func fileETag(assets fs.FS, name string) (string, error) {
	f, err := assets.Open(name)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := f.Close(); err != nil {
			slog.Debug("asset close", "err", err, "name", name)
		}
	}()

	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return `"` + hex.EncodeToString(sum.Sum(nil)[:16]) + `"`, nil
}

// assetHeaders sets the caching headers and entity tag appropriate to each
// kind of asset.
//
// Content-addressed bundles can be kept forever: a new build produces a new
// name, so a cached copy can never be the wrong one. Everything else --
// index.html, and now the application chunk and stylesheet -- must revalidate
// on every use, because a stale copy of index.html pins the browser to an old
// bundle permanently and a stale copy of the chunk runs old code against a new
// server. The entity tag is what keeps that revalidation to a 304 rather than
// a full re-download.
func assetHeaders(etags map[string]string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := assetPath(r.URL.Path)
		if tag, ok := etags[name]; ok {
			// Set before delegating: http.ServeContent reads the entity tag
			// back out of the response header to answer If-None-Match.
			w.Header().Set("ETag", tag)
		}
		if hashedAsset.MatchString(name) {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		}
		next.ServeHTTP(w, r)
	})
}
