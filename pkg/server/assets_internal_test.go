package server

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// errFS is an fs.FS whose files exist as far as WalkDir is concerned but
// cannot be opened. It stands in for the unreadable-asset case, which cannot
// happen with a real embed.FS but must not take the server down if it ever
// does.
type errFS struct {
	fs.FS
	bad string
}

func (e errFS) Open(name string) (fs.File, error) {
	if name == e.bad {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
	}
	return e.FS.Open(name)
}

func TestAssetPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"root resolves to the entry point", "/", "index.html"},
		{"empty resolves to the entry point", "", "index.html"},
		{"leading slash is stripped", "/assets/index.js", "assets/index.js"},
		{"already relative", "assets/index.js", "assets/index.js"},
		{"redundant separators collapse", "//assets//index.js", "assets/index.js"},
		{"dot segments collapse", "/assets/./index.js", "assets/index.js"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := assetPath(c.in); got != c.want {
				t.Errorf("assetPath(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestAssetPath_EscapeAttemptStaysInsideRoot checks that a path trying to
// climb out of the asset root yields a name still relative to that root, so it
// can only ever miss the entity-tag table rather than name a file outside it.
// Rooting the path before cleaning is what discards the leading "..", and it
// is the same normalisation http.FileServerFS applies to the lookup itself.
func TestAssetPath_EscapeAttemptStaysInsideRoot(t *testing.T) {
	for _, in := range []string{"/../etc/passwd", "/assets/../../etc/passwd", "/..", "../secret"} {
		got := assetPath(in)
		if strings.HasPrefix(got, "..") || strings.HasPrefix(got, "/") {
			t.Errorf("assetPath(%q) = %q, want a name confined to the asset root", in, got)
		}
	}
}

func TestHashedAsset(t *testing.T) {
	hashed := []string{
		"assets/echarts-CJXIpdJj.js",
		"assets/index-DRxpB94K.css",
		"assets/some_font-AbCd1234_-.woff2",
	}
	for _, name := range hashed {
		if !hashedAsset.MatchString(name) {
			t.Errorf("hashedAsset.MatchString(%q) = false, want true", name)
		}
	}

	// The fixed-name bundle, the entry point, and anything outside assets/
	// must not be treated as content-addressed: they are rewritten in place.
	plain := []string{
		"assets/index.js",
		"assets/index.css",
		"index.html",
		"favicon.ico",
		"assets/short-abc.js",
	}
	for _, name := range plain {
		if hashedAsset.MatchString(name) {
			t.Errorf("hashedAsset.MatchString(%q) = true, want false", name)
		}
	}
}

func TestAssetETags(t *testing.T) {
	assets := fstest.MapFS{
		"index.html":       &fstest.MapFile{Data: []byte("<html>app</html>")},
		"assets/index.js":  &fstest.MapFile{Data: []byte("console.log(1)")},
		"assets/index.css": &fstest.MapFile{Data: []byte("body{}")},
	}

	tags := assetETags(assets)
	if len(tags) != len(assets) {
		t.Fatalf("len(tags) = %d, want %d: %v", len(tags), len(assets), tags)
	}
	for name, tag := range tags {
		if len(tag) != 34 || tag[0] != '"' || tag[len(tag)-1] != '"' {
			t.Errorf("tag for %q = %q, want a quoted 32-hex-digit tag", name, tag)
		}
	}
	if tags["assets/index.js"] == tags["assets/index.css"] {
		t.Error("different files share an entity tag")
	}

	// The tag must be a function of the bytes alone, so that a rebuild that
	// changes nothing does not invalidate a client's cache.
	if again := assetETags(assets); again["assets/index.js"] != tags["assets/index.js"] {
		t.Error("entity tag is not stable across calls")
	}

	// ...and must change when the bytes do, which is the whole point now that
	// the filename no longer does.
	changed := fstest.MapFS{"assets/index.js": &fstest.MapFile{Data: []byte("console.log(2)")}}
	if assetETags(changed)["assets/index.js"] == tags["assets/index.js"] {
		t.Error("entity tag unchanged after the file's bytes changed")
	}
}

// TestAssetETags_BadInput covers the two ways the table can come up short: no
// filesystem at all, and a file that walks but will not open. Neither may
// panic, and neither may take down a server that is otherwise able to serve.
func TestAssetETags_BadInput(t *testing.T) {
	if tags := assetETags(nil); len(tags) != 0 {
		t.Errorf("assetETags(nil) = %v, want an empty table", tags)
	}

	assets := fstest.MapFS{
		"index.html":      &fstest.MapFile{Data: []byte("<html>app</html>")},
		"assets/index.js": &fstest.MapFile{Data: []byte("console.log(1)")},
	}
	tags := assetETags(errFS{FS: assets, bad: "assets/index.js"})
	if _, ok := tags["assets/index.js"]; ok {
		t.Error("unreadable file got an entity tag")
	}
	if _, ok := tags["index.html"]; !ok {
		t.Error("readable file lost its entity tag because a sibling failed")
	}
}

func TestFileETag(t *testing.T) {
	assets := fstest.MapFS{"a.js": &fstest.MapFile{Data: []byte("x")}}
	tag, err := fileETag(assets, "a.js")
	if err != nil {
		t.Fatalf("fileETag: %v", err)
	}
	// SHA-256("x") truncated to 16 bytes.
	const want = `"2d711642b726b04401627ca9fbac32f5"`
	if tag != want {
		t.Errorf("fileETag = %q, want %q", tag, want)
	}
}

func TestFileETag_MissingFile(t *testing.T) {
	if _, err := fileETag(fstest.MapFS{}, "nope.js"); err == nil {
		t.Fatal("fileETag on a missing file returned no error")
	}
}

func TestAssetHeaders(t *testing.T) {
	assets := fstest.MapFS{
		"index.html":                 &fstest.MapFile{Data: []byte("<html>app</html>")},
		"assets/index.js":            &fstest.MapFile{Data: []byte("console.log(1)")},
		"assets/echarts-CJXIpdJj.js": &fstest.MapFile{Data: []byte("charts")},
	}
	h := assetHeaders(assetETags(assets), http.FileServerFS(assets))

	cases := []struct {
		name      string
		path      string
		wantCache string
	}{
		{"entry point revalidates", "/", "no-cache, must-revalidate"},
		{"fixed-name bundle revalidates", "/assets/index.js", "no-cache, must-revalidate"},
		{"hashed chunk is immutable", "/assets/echarts-CJXIpdJj.js", "public, max-age=31536000, immutable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))

			if got := rec.Header().Get("Cache-Control"); got != c.wantCache {
				t.Errorf("Cache-Control = %q, want %q", got, c.wantCache)
			}
			if rec.Header().Get("ETag") == "" {
				t.Error("ETag is empty, so a revalidating client has no validator to send")
			}
		})
	}
}

// TestAssetHeaders_ConditionalRequestIs304 is the property that makes the
// fixed bundle name affordable: without a validator every reload would
// re-download the whole 226 kB chunk, because an embedded filesystem reports
// no modification time.
func TestAssetHeaders_ConditionalRequestIs304(t *testing.T) {
	assets := fstest.MapFS{"assets/index.js": &fstest.MapFile{Data: []byte("console.log(1)")}}
	tags := assetETags(assets)
	h := assetHeaders(tags, http.FileServerFS(assets))

	req := httptest.NewRequest(http.MethodGet, "/assets/index.js", nil)
	req.Header.Set("If-None-Match", tags["assets/index.js"])
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotModified)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %d bytes, want an empty 304", rec.Body.Len())
	}
}

// TestAssetHeaders_StaleTagRefetches is the safety property the content hash
// used to provide: a client holding the previous bundle must not be allowed to
// keep it once the server's copy changes.
func TestAssetHeaders_StaleTagRefetches(t *testing.T) {
	assets := fstest.MapFS{"assets/index.js": &fstest.MapFile{Data: []byte("console.log(2)")}}
	h := assetHeaders(assetETags(assets), http.FileServerFS(assets))

	req := httptest.NewRequest(http.MethodGet, "/assets/index.js", nil)
	req.Header.Set("If-None-Match", `"00000000000000000000000000000000"`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec.Body.String() != "console.log(2)" {
		t.Errorf("body = %q, want the current bundle", rec.Body.String())
	}
}

// TestAssetHeaders_UnknownHashedPathIsNotCacheable covers a path that looks
// content-addressed but has nothing behind it. net/http strips the caching
// headers off the 404 it writes, and that is the outcome we want: what must
// never happen is a miss being served back as immutable for a year.
func TestAssetHeaders_UnknownHashedPathIsNotCacheable(t *testing.T) {
	assets := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html>app</html>")}}
	h := assetHeaders(assetETags(assets), http.FileServerFS(assets))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/gone-Cs6KRCp8.js", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if got := rec.Header().Get("Cache-Control"); strings.Contains(got, "immutable") {
		t.Errorf("Cache-Control = %q, want a miss never to be cacheable", got)
	}
	if got := rec.Header().Get("ETag"); got != "" {
		t.Errorf("ETag = %q, want none for a path with no asset behind it", got)
	}
}
