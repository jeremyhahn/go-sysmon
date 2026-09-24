package pins

import (
	"go/version"
	"regexp"
	"strings"

	"golang.org/x/mod/semver"
)

// Status says how a pin relates to the latest stable release of what it pins.
// Only Current passes: the owner's rule is the latest stable release, exactly.
type Status string

const (
	// Current means the pin names the latest stable release.
	Current Status = "current"
	// Behind means a newer stable release exists.
	Behind Status = "behind"
	// Ahead means the pin is newer than the latest stable release the source
	// reports: the pinned release was retracted, or is not published where
	// the toolchain resolves it.
	Ahead Status = "ahead"
	// Prerelease means the pin names a release candidate, beta or
	// pseudo-version rather than a stable release.
	Prerelease Status = "prerelease"
)

// goPrerelease matches the prerelease forms the Go toolchain publishes,
// "1.28rc1" and "1.28beta1", after the "go" prefix and any "-suffix" are gone.
var goPrerelease = regexp.MustCompile(`(rc|beta)[0-9]+$`)

// goRelease matches an exact Go release, "1.27.1". A language version such as
// "1.27" names no single toolchain, so it is not one.
var goRelease = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// canonicalGo returns v in the "go1.27.1" form go/version compares, accepting
// the "1.27.1" form go.mod and image tags use. A "-suffix" (a vendor build such
// as "go1.27.1-bigcorp") is kept: go/version ignores it when comparing.
func canonicalGo(v string) (string, error) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "go") {
		v = "go" + v
	}
	if !version.IsValid(v) {
		return "", &VersionError{Kind: "Go", Value: v}
	}
	return v, nil
}

// goNumber returns the bare release number of a canonical Go version:
// "go1.27.1-bigcorp" becomes "1.27.1".
func goNumber(v string) string {
	v = strings.TrimPrefix(v, "go")
	v, _, _ = strings.Cut(v, "-")
	return v
}

// IsGoRelease reports whether v names one exact, stable Go release. It accepts
// both "1.27.1" and "go1.27.1", and rejects release candidates, betas and bare
// language versions such as "1.27".
func IsGoRelease(v string) bool {
	c, err := canonicalGo(v)
	if err != nil {
		return false
	}
	return goRelease.MatchString(goNumber(c))
}

// SameGo reports whether a and b name the same Go release. Invalid versions
// are never the same as anything.
func SameGo(a, b string) bool {
	ca, err := canonicalGo(a)
	if err != nil {
		return false
	}
	cb, err := canonicalGo(b)
	if err != nil {
		return false
	}
	return version.Compare(ca, cb) == 0
}

// CompareGo classifies a pinned Go version against the latest stable Go
// release. The latest must itself be an exact stable release: a source that
// reports a release candidate or a bare language version as the latest is not
// answering the question, and the answer is refused rather than trusted.
func CompareGo(pinned, latest string) (Status, error) {
	p, err := canonicalGo(pinned)
	if err != nil {
		return "", err
	}
	l, err := canonicalGo(latest)
	if err != nil {
		return "", err
	}
	if !goRelease.MatchString(goNumber(l)) {
		return "", &VersionError{Kind: "stable Go release", Value: latest}
	}
	if goPrerelease.MatchString(goNumber(p)) {
		return Prerelease, nil
	}
	return classify(version.Compare(p, l)), nil
}

// CompareModule classifies a pinned module version against the version the
// module proxy reports as latest. Both must be canonical semantic versions
// ("v1.2.3"). A pinned prerelease or pseudo-version is Prerelease whatever the
// latest is. Build metadata ("+incompatible") does not affect the order.
func CompareModule(pinned, latest string) (Status, error) {
	if !semver.IsValid(pinned) {
		return "", &VersionError{Kind: "module", Value: pinned}
	}
	if !semver.IsValid(latest) {
		return "", &VersionError{Kind: "module", Value: latest}
	}
	if semver.Prerelease(pinned) != "" {
		return Prerelease, nil
	}
	return classify(semver.Compare(pinned, latest)), nil
}

// IsExactRelease reports whether v is one exact stable release in the
// unprefixed MAJOR.MINOR.PATCH form Node.js versions and image tags use,
// "24.21.0". Prereleases, build metadata and partial versions are not.
func IsExactRelease(v string) bool {
	return !strings.HasPrefix(v, "v") && semver.Canonical("v"+v) == "v"+v && semver.Prerelease("v"+v) == ""
}

// CompareRelease classifies a pinned MAJOR.MINOR.PATCH release ("24.21.0")
// against the latest ("24.22.0"). Both are unprefixed, as Node.js versions
// are written in version files and image tags.
func CompareRelease(pinned, latest string) (Status, error) {
	if !IsExactRelease(pinned) {
		return "", &VersionError{Kind: "exact release", Value: pinned}
	}
	if !IsExactRelease(latest) {
		return "", &VersionError{Kind: "exact release", Value: latest}
	}
	return classify(semver.Compare("v"+pinned, "v"+latest)), nil
}

// CompareUbuntu classifies a pinned Ubuntu release ("24.04") against the
// latest LTS ("26.04"). Release numbers are fixed-width YY.MM (the pattern
// admits nothing else), so they order as strings; they are not semantic
// versions, whose rules reject the leading zero in "04".
func CompareUbuntu(pinned, latest string) (Status, error) {
	if !ubuntuRelease.MatchString(pinned) {
		return "", &VersionError{Kind: "Ubuntu release", Value: pinned}
	}
	if !ubuntuRelease.MatchString(latest) {
		return "", &VersionError{Kind: "Ubuntu release", Value: latest}
	}
	return classify(strings.Compare(pinned, latest)), nil
}

// classify maps a three-way comparison of pin against latest to a Status.
func classify(cmp int) Status {
	switch {
	case cmp < 0:
		return Behind
	case cmp > 0:
		return Ahead
	default:
		return Current
	}
}
