//go:build !desktop

package main

import (
	"errors"
	"testing"

	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

// withImageFlags sets the image selection flags for one test.
func withImageFlags(t *testing.T, limit int, filter, sortKey string, unused bool) {
	t.Helper()
	origLimit, origFilter, origSort, origUnused := imagesLimit, imagesFilter, imagesSort, imagesUnused
	imagesLimit, imagesFilter, imagesSort, imagesUnused = limit, filter, sortKey, unused
	t.Cleanup(func() {
		imagesLimit, imagesFilter, imagesSort, imagesUnused = origLimit, origFilter, origSort, origUnused
	})
}

// imageSet is a small inventory covering tagged, untagged, used and unused.
func imageSet() []types.ImageInfo {
	return []types.ImageInfo{
		{ShortID: "aaa", Tags: []string{"nginx:latest"}, SizeBytes: 100, SharedSizeBytes: 10, Containers: 2, InUse: true, CreatedUnix: 300},
		{ShortID: "bbb", Tags: []string{"redis:7"}, SizeBytes: 300, SharedSizeBytes: 30, Containers: 0, CreatedUnix: 100},
		{ShortID: "ccc", Tags: nil, SizeBytes: 200, SharedSizeBytes: 20, Containers: 0, Dangling: true, CreatedUnix: 200},
	}
}

func TestSelectImages_SortsLargestFirstByDefault(t *testing.T) {
	withImageFlags(t, 0, "", "size", false)

	got, err := selectImages(imageSet())
	if err != nil {
		t.Fatalf("selectImages() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d images, want 3", len(got))
	}
	if got[0].SizeBytes != 300 || got[2].SizeBytes != 100 {
		t.Errorf("not sorted largest-first: %v", []uint64{got[0].SizeBytes, got[1].SizeBytes, got[2].SizeBytes})
	}
}

func TestSelectImages_SortKeys(t *testing.T) {
	tests := map[string]func([]types.ImageInfo) bool{
		"tag":     func(g []types.ImageInfo) bool { return g[0].ShortID == "aaa" }, // nginx before redis, untagged last
		"id":      func(g []types.ImageInfo) bool { return g[0].ShortID == "aaa" },
		"created": func(g []types.ImageInfo) bool { return g[0].CreatedUnix == 300 }, // newest first
		"used":    func(g []types.ImageInfo) bool { return g[0].Containers == 2 },
		"shared":  func(g []types.ImageInfo) bool { return g[0].SharedSizeBytes == 30 },
	}

	for key, ok := range tests {
		t.Run(key, func(t *testing.T) {
			withImageFlags(t, 0, "", key, false)
			got, err := selectImages(imageSet())
			if err != nil {
				t.Fatalf("selectImages(%q) error = %v", key, err)
			}
			if !ok(got) {
				t.Errorf("sort by %q produced unexpected order: first = %+v", key, got[0])
			}
		})
	}
}

func TestSelectImages_RejectsUnknownSortKey(t *testing.T) {
	withImageFlags(t, 0, "", "bogus", false)

	_, err := selectImages(imageSet())
	if err == nil {
		t.Fatal("selectImages() = nil error for an unknown sort key")
	}
	var invalid *types.InvalidSortKeyError
	if !errors.As(err, &invalid) {
		t.Errorf("error = %T, want *types.InvalidSortKeyError", err)
	}
	if invalid.Valid == "" {
		t.Error("the error must list the valid keys")
	}
}

func TestSelectImages_FilterMatchesTagAndID(t *testing.T) {
	withImageFlags(t, 0, "redis", "size", false)
	got, err := selectImages(imageSet())
	if err != nil {
		t.Fatalf("selectImages() error = %v", err)
	}
	if len(got) != 1 || got[0].ShortID != "bbb" {
		t.Errorf("tag filter returned %+v", got)
	}

	withImageFlags(t, 0, "CCC", "size", false)
	got, err = selectImages(imageSet())
	if err != nil {
		t.Fatalf("selectImages() error = %v", err)
	}
	if len(got) != 1 || got[0].ShortID != "ccc" {
		t.Errorf("ID filter is not case-insensitive: %+v", got)
	}
}

// TestSelectImages_UnusedIsWhatPruneWouldRemove verifies the flag selects
// exactly the images no container references.
func TestSelectImages_UnusedIsWhatPruneWouldRemove(t *testing.T) {
	withImageFlags(t, 0, "", "size", true)

	got, err := selectImages(imageSet())
	if err != nil {
		t.Fatalf("selectImages() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d unused images, want 2", len(got))
	}
	for _, img := range got {
		if img.InUse {
			t.Errorf("in-use image %s survived --unused", img.ShortID)
		}
	}
}

func TestSelectImages_LimitTruncates(t *testing.T) {
	withImageFlags(t, 2, "", "size", false)

	got, err := selectImages(imageSet())
	if err != nil {
		t.Fatalf("selectImages() error = %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d images, want the limit of 2", len(got))
	}
	// The limit must keep the largest, not an arbitrary two.
	if got[0].SizeBytes != 300 || got[1].SizeBytes != 200 {
		t.Errorf("limit did not keep the top of the sort: %+v", got)
	}
}

func TestSelectImages_EmptyInput(t *testing.T) {
	withImageFlags(t, 5, "anything", "size", true)

	got, err := selectImages(nil)
	if err != nil {
		t.Fatalf("selectImages(nil) error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d images from empty input", len(got))
	}
}
