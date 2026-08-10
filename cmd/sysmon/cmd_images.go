package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jeremyhahn/go-sysmon/pkg/cli"
	"github.com/jeremyhahn/go-sysmon/pkg/collector"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

var imagesCmd = &cobra.Command{
	Use:     "images",
	Aliases: []string{"image"},
	Short:   "Display container images and runtime storage usage",
	Long: "Display container images, their sizes and how much space can be reclaimed, " +
		"along with where the runtime stores them on disk.\n\n" +
		"This is the one command that talks to the container runtime's API socket: " +
		"image inventory exists only in the runtime's database and its storage " +
		"directory is root-owned, so there is no unprivileged filesystem route to it. " +
		"Running this command is the opt-in; no other command contacts the daemon.",
	RunE: runImagesCmd,
}

func init() {
	imagesCmd.Flags().IntVar(&imagesLimit, "limit", 0,
		"Show at most this many images (0 = all)")
	imagesCmd.Flags().StringVar(&imagesFilter, "filter", "",
		"Show only images whose tag or ID contains this text")
	imagesCmd.Flags().StringVar(&imagesSort, "sort", "size",
		"Sort by: size, shared, tag, id, created or used")
	imagesCmd.Flags().BoolVar(&imagesUnused, "unused", false,
		"Show only images no container is using, which are what a prune would remove")
	rootCmd.AddCommand(imagesCmd)
}

// Filtering and ordering flags for the image list.
var (
	imagesLimit  int
	imagesFilter string
	imagesSort   string
	imagesUnused bool
)

func runImagesCmd(cmd *cobra.Command, _ []string) error {
	// Invoking this command is the explicit consent to query the daemon.
	snap, err := collectSnapshotWithRuntime()
	if err != nil {
		return err
	}

	filtered, err := selectImages(snap.Virt.Runtime.Images)
	if err != nil {
		return err
	}
	// Report how many the flags removed so a shortened list is never mistaken
	// for the whole inventory.
	snap.Virt.Runtime.ImagesFiltered = len(snap.Virt.Runtime.Images) - len(filtered)
	snap.Virt.Runtime.Images = filtered

	if jsonOutput {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(snap.Virt.Runtime)
	}
	if err := cli.RenderImages(cmd.OutOrStdout(), snap); err != nil {
		return fmt.Errorf("render images: %w", err)
	}
	return nil
}

// collectSnapshotWithRuntime takes a snapshot with runtime API queries turned
// on. It is a variable so tests can supply a synthetic snapshot.
var collectSnapshotWithRuntime = func() (*types.Snapshot, error) {
	sc := collector.NewSystemCollector(nil)
	sc.SetTiering(false)
	sc.EnableRuntimeAPI(true)

	ctx, cancel := context.WithTimeout(cmdContext(), imagesTimeout)
	defer cancel()

	// The first snapshot starts the background disk usage query; wait for it
	// so a one-shot command reports sizes rather than zeros.
	if _, err := sc.Snapshot(ctx); err != nil {
		return nil, err
	}
	sc.WaitForRuntimeDiskUsage(ctx)

	snap, err := sc.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

// imagesTimeout bounds the whole command. Querying a runtime's disk usage
// walks every layer and volume, which takes many seconds on a large host.
const imagesTimeout = 90 * time.Second

// selectImages applies the filter, sort and limit flags.
func selectImages(images []types.ImageInfo) ([]types.ImageInfo, error) {
	out := make([]types.ImageInfo, 0, len(images))
	needle := strings.ToLower(strings.TrimSpace(imagesFilter))

	for _, img := range images {
		if imagesUnused && img.InUse {
			continue
		}
		if needle != "" {
			haystack := strings.ToLower(img.ShortID + " " + strings.Join(img.Tags, " "))
			if !strings.Contains(haystack, needle) {
				continue
			}
		}
		out = append(out, img)
	}

	less, err := imageLess(imagesSort, out)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, less)

	if imagesLimit > 0 && len(out) > imagesLimit {
		out = out[:imagesLimit]
	}
	return out, nil
}

// imageLess returns the comparison for the requested sort key. Numeric keys
// order largest-first, which is the useful end when reclaiming space.
func imageLess(key string, images []types.ImageInfo) (func(i, j int) bool, error) {
	switch strings.ToLower(key) {
	case "size", "":
		return func(i, j int) bool { return images[i].SizeBytes > images[j].SizeBytes }, nil
	case "shared":
		return func(i, j int) bool { return images[i].SharedSizeBytes > images[j].SharedSizeBytes }, nil
	case "created":
		return func(i, j int) bool { return images[i].CreatedUnix > images[j].CreatedUnix }, nil
	case "used":
		return func(i, j int) bool { return images[i].Containers > images[j].Containers }, nil
	case "id":
		return func(i, j int) bool { return images[i].ShortID < images[j].ShortID }, nil
	case "tag":
		return func(i, j int) bool { return firstTag(images[i]) < firstTag(images[j]) }, nil
	default:
		return nil, &types.InvalidSortKeyError{Key: key, Valid: "size, shared, tag, id, created, used"}
	}
}

// firstTag returns an image's first tag, sorting untagged images last.
func firstTag(img types.ImageInfo) string {
	if len(img.Tags) == 0 {
		return "\uffff"
	}
	return img.Tags[0]
}
