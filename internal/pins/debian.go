package pins

import (
	"context"
	"strconv"
	"strings"
)

// DebianRelease is what a Debian suite's Release file says about it.
type DebianRelease struct {
	// Codename is the release codename, "trixie".
	Codename string
	// Version is the point release, "13.7". It is empty for testing and
	// unstable, which are not releases.
	Version string
}

// DebianReleaseFunc returns the Release information of a suite: "stable", or
// a codename such as "bookworm".
type DebianReleaseFunc func(ctx context.Context, suite string) (DebianRelease, error)

// DebianItem judges the pinned Debian codename against the current stable
// release. Codenames do not order, so a pin that is not stable is placed by
// its own Release file: a numbered release older than stable is Behind, one
// with no number (testing, unstable) is Prerelease.
func DebianItem(pinned string, release DebianReleaseFunc) Item {
	return Item{Name: DebianName, Pinned: pinned, Check: func(ctx context.Context) (string, Status, error) {
		stable, err := release(ctx, "stable")
		if err != nil {
			return "", "", err
		}
		latest := stable.Codename + " (" + stable.Version + ")"
		stableMajor, err := debianMajor(stable)
		if err != nil {
			return "", "", err
		}
		if pinned == stable.Codename {
			return latest, Current, nil
		}
		own, err := release(ctx, pinned)
		if err != nil {
			return "", "", err
		}
		if own.Version == "" {
			return latest, Prerelease, nil
		}
		ownMajor, err := debianMajor(own)
		if err != nil {
			return "", "", err
		}
		return latest, classify(ownMajor - stableMajor), nil
	}}
}

// debianMajor returns the major number of a release's version, 13 for "13.7".
func debianMajor(r DebianRelease) (int, error) {
	major, _, _ := strings.Cut(r.Version, ".")
	n, err := strconv.Atoi(major)
	if err != nil || n <= 0 {
		return 0, &VersionError{Kind: "Debian release", Value: r.Version}
	}
	return n, nil
}
