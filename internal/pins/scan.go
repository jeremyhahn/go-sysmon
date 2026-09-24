package pins

import (
	"bufio"
	"bytes"
	"path"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Pins is every version the repository sets in exactly one place. Every other
// reference to one of them must repeat it exactly, or read it from its source.
type Pins struct {
	// Go is the go directive in go.mod, "1.27.1".
	Go string
	// Toolchain is go.mod's toolchain directive, "go1.27.1", or empty.
	Toolchain string
	// Node is the exact Node.js release, "24.21.0", read from NodeFile.
	Node string
	// NodeFile is the file Node is read from, slash-separated and relative to
	// the root, ".node-version". setup-node steps must read it.
	NodeFile string
	// Debian is the Debian release codename every Debian-based image is
	// built on, "trixie".
	Debian string
	// Ubuntu is the Ubuntu LTS release Ubuntu images use, "26.04".
	Ubuntu string
	// HostedRunner is the Ubuntu release of every versioned GitHub-hosted
	// runner label (ubuntu-26.04, ubuntu-26.04-arm), "26.04".
	HostedRunner string
}

// debianCodename matches a Debian release codename.
var debianCodename = regexp.MustCompile(`^[a-z]+$`)

// ubuntuRelease matches an Ubuntu release number, "26.04".
var ubuntuRelease = regexp.MustCompile(`^[0-9]{2}\.(04|10)$`)

// CheckAgreement reports whether every version reference under root agrees
// with pins. It returns the number of references it examined, and a
// *MismatchError naming each one that disagrees.
//
// References are official images (golang, node, debian, ubuntu) in
// Dockerfiles, compose files and workflows, actions/setup-go and
// actions/setup-node steps, and versioned GitHub-hosted runner labels. An image must name exactly the pinned release,
// and a golang or node image's base variant must be the pinned Debian
// release, so a floating tag such as golang:1.27 or node:24-slim is a
// mismatch. A setup step must read the pin's source file or name exactly the
// pinned release.
func CheckAgreement(root string, pins Pins) (int, error) {
	s := scanner{pins: pins}
	s.checkPins()
	err := walkFiles(root, func(p, name string) scanFunc {
		lower := strings.ToLower(name)
		switch {
		case lower == "dockerfile", lower == "containerfile",
			strings.HasPrefix(lower, "dockerfile."), strings.HasSuffix(lower, ".dockerfile"):
			return s.scanDockerfile
		case isWorkflow(p, name):
			return s.scanWorkflow
		case isYAML(lower) && (strings.HasPrefix(lower, "docker-compose") || strings.HasPrefix(lower, "compose")):
			return s.scanCompose
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if len(s.mismatches) > 0 {
		slices.SortStableFunc(s.mismatches, func(a, b Mismatch) int {
			if c := strings.Compare(a.File, b.File); c != 0 {
				return c
			}
			return a.Line - b.Line
		})
		return s.refs, &MismatchError{Mismatches: s.mismatches}
	}
	return s.refs, nil
}

// scanner accumulates what one CheckAgreement walk finds.
type scanner struct {
	pins       Pins
	refs       int
	mismatches []Mismatch
}

func (s *scanner) mismatch(file string, line int, found, reason string) {
	s.mismatches = append(s.mismatches, Mismatch{File: file, Line: line, Found: found, Reason: reason})
}

// checkPins checks the pins themselves: each must name one exact release,
// since every other reference is compared with it.
func (s *scanner) checkPins() {
	p := s.pins
	if !IsGoRelease(p.Go) {
		s.mismatch("go.mod", 0, p.Go, "the go directive must name an exact stable Go release")
	}
	if p.Toolchain != "" && !SameGo(p.Toolchain, p.Go) {
		s.mismatch("go.mod", 0, p.Toolchain, "the toolchain directive names a different release from the go directive")
	}
	if !IsExactRelease(p.Node) {
		s.mismatch(p.NodeFile, 0, p.Node, "must name an exact stable Node.js release, such as 24.21.0")
	}
	if !debianCodename.MatchString(p.Debian) {
		s.mismatch("Makefile", 0, p.Debian, "DEBIAN_RELEASE must be a Debian release codename, such as trixie")
	}
	if !ubuntuRelease.MatchString(p.Ubuntu) {
		s.mismatch("Makefile", 0, p.Ubuntu, "UBUNTU_RELEASE must be an Ubuntu release number, such as 26.04")
	}
	if !ubuntuLTS.MatchString(p.HostedRunner) {
		s.mismatch("Makefile", 0, p.HostedRunner, "HOSTED_RUNNER_RELEASE must be an Ubuntu LTS release number, such as 26.04")
	}
}

// imageRules checks the tag of each official image this repository pins. A
// rule returns why the tag disagrees with the pins, or "" when it agrees.
var imageRules = map[string]func(p Pins, tag string) string{
	"golang": func(p Pins, tag string) string {
		release, variant, _ := strings.Cut(tag, "-")
		if !IsGoRelease(release) {
			return "the tag does not name an exact stable Go release, so the image floats"
		}
		if !SameGo(release, p.Go) {
			return "the image is Go " + release + ", go.mod says " + p.Go
		}
		return debianVariant(p, variant)
	},
	"node": func(p Pins, tag string) string {
		release, variant, _ := strings.Cut(tag, "-")
		if !IsExactRelease(release) {
			return "the tag does not name an exact stable Node.js release, so the image floats"
		}
		if release != p.Node {
			return "the image is Node.js " + release + ", " + p.NodeFile + " says " + p.Node
		}
		return debianVariant(p, variant)
	},
	"debian": func(p Pins, tag string) string {
		if codename := strings.TrimSuffix(tag, "-slim"); codename != p.Debian {
			return "the image is Debian " + codename + ", DEBIAN_RELEASE is " + p.Debian
		}
		return ""
	},
	"ubuntu": func(p Pins, tag string) string {
		if tag != p.Ubuntu {
			return "the image is Ubuntu " + tag + ", UBUNTU_RELEASE is " + p.Ubuntu
		}
		return ""
	},
}

// debianVariant checks the base variant of a golang or node tag, "trixie" or
// "trixie-slim". A tag with no variant takes whatever Debian release the image
// currently defaults to, so the base OS floats.
func debianVariant(p Pins, variant string) string {
	if variant == "" {
		return "the tag names no base variant, so its Debian release floats; add -" + p.Debian
	}
	if codename := strings.TrimSuffix(variant, "-slim"); codename != p.Debian {
		return "the image is built on " + variant + ", DEBIAN_RELEASE is " + p.Debian
	}
	return ""
}

// officialImage returns the Docker Hub official image an image name refers to
// ("docker.io/library/golang" and "library/golang" are "golang"), or "" for
// any other repository.
func officialImage(name string) string {
	for _, prefix := range []string{"docker.io/", "index.docker.io/", "library/"} {
		name = strings.TrimPrefix(name, prefix)
	}
	if strings.ContainsAny(name, "/:.") {
		return ""
	}
	return name
}

// checkImage examines one image reference. An image with no rule is ignored;
// one with a rule counts as a reference and must be tagged in agreement with
// the pins.
func (s *scanner) checkImage(file string, line int, ref string) {
	name, tag := splitImage(ref)
	rule, ok := imageRules[officialImage(name)]
	if !ok {
		return
	}
	s.refs++
	if tag == "" || tag == "latest" {
		s.mismatch(file, line, ref, "an untagged or latest image floats to whatever release is newest")
		return
	}
	if reason := rule(s.pins, tag); reason != "" {
		s.mismatch(file, line, ref, reason)
	}
}

// splitImage returns an image reference's repository name and tag, with any
// digest dropped: "docker.io/library/golang:1.27.1-trixie@sha256:..." gives
// ("docker.io/library/golang", "1.27.1-trixie").
func splitImage(ref string) (name, tag string) {
	ref, _, _ = strings.Cut(ref, "@")
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon > slash {
		return ref[:colon], ref[colon+1:]
	}
	return ref, ""
}

// dockerArg matches a variable reference in a Dockerfile: $NAME or ${NAME}.
var dockerArg = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// scanDockerfile checks every FROM line. A build argument in the image is
// resolved from an earlier ARG default in the same file; one with no default
// cannot be checked and is a mismatch.
func (s *scanner) scanDockerfile(file string, data []byte) error {
	args := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	line := 0
	for sc.Scan() {
		line++
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "ARG":
			if name, value, ok := strings.Cut(fields[1], "="); ok {
				args[name] = strings.Trim(value, `"'`)
			}
		case "FROM":
			image := ""
			for _, f := range fields[1:] {
				if !strings.HasPrefix(f, "--") {
					image = f
					break
				}
			}
			resolved, ok := expandArgs(image, args)
			if !ok {
				name, _ := splitImage(image)
				if _, pinned := imageRules[officialImage(name)]; pinned {
					s.refs++
					s.mismatch(file, line, image, "the tag uses a build argument with no default, so it cannot be checked")
				}
				continue
			}
			s.checkImage(file, line, resolved)
		}
	}
	if err := sc.Err(); err != nil {
		return &SourceError{Path: file, Cause: err}
	}
	return nil
}

// expandArgs substitutes known build arguments into s. It reports false when a
// reference has no known value.
func expandArgs(s string, args map[string]string) (string, bool) {
	ok := true
	out := dockerArg.ReplaceAllStringFunc(s, func(m string) string {
		sub := dockerArg.FindStringSubmatch(m)
		v, known := args[sub[1]+sub[2]]
		if !known {
			ok = false
		}
		return v
	})
	return out, ok
}

// envExpr matches a workflow expression that reads one env variable.
var envExpr = regexp.MustCompile(`^\$\{\{\s*env\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}$`)

// setupRule describes a setup action that installs a pinned toolchain: the
// input naming a version file and the file it must name, and the input naming
// a version and the check of that version against the pins.
type setupRule struct {
	fileInput    string
	file         func(p Pins) string
	versionInput string
	version      func(p Pins, v string) string
}

// setupRules are the setup actions whose version is pinned, by action.
var setupRules = map[string]setupRule{
	"actions/setup-go": {
		fileInput:    "go-version-file",
		file:         func(Pins) string { return "go.mod" },
		versionInput: "go-version",
		version: func(p Pins, v string) string {
			switch {
			case !IsGoRelease(v):
				return "go-version does not name an exact stable release"
			case !SameGo(v, p.Go):
				return "go-version is Go " + v + ", go.mod says " + p.Go
			}
			return ""
		},
	},
	"actions/setup-node": {
		fileInput:    "node-version-file",
		file:         func(p Pins) string { return p.NodeFile },
		versionInput: "node-version",
		version: func(p Pins, v string) string {
			if v != p.Node {
				return "node-version is " + v + ", " + p.NodeFile + " says " + p.Node
			}
			return ""
		},
	},
}

// scanWorkflow checks setup steps, job containers, service containers and
// docker:// steps in one Actions workflow.
func (s *scanner) scanWorkflow(file string, data []byte) error {
	doc, err := parseYAML(file, data)
	if err != nil || doc == nil {
		return err
	}
	image := func(line int, ref string) { s.checkImage(file, line, ref) }
	workflowSteps(doc, image, func(step, uses *yaml.Node, env []map[string]string) {
		action, _, _ := strings.Cut(uses.Value, "@")
		if rule, ok := setupRules[action]; ok {
			s.checkSetup(file, uses.Line, action, rule, child(step, "with"), env)
		}
	})
	eachScalar(doc, func(n *yaml.Node) {
		for _, m := range hostedUbuntuLabel.FindAllStringSubmatch(n.Value, -1) {
			s.refs++
			if m[1] != s.pins.HostedRunner {
				s.mismatch(file, n.Line, m[0], "the hosted runner is Ubuntu "+m[1]+", HOSTED_RUNNER_RELEASE is "+s.pins.HostedRunner)
			}
		}
	})
	return nil
}

// checkSetup checks one setup step. Without either input the action installs
// whatever release it defaults to, which is a pin nothing controls.
func (s *scanner) checkSetup(file string, line int, action string, rule setupRule, with *yaml.Node, env []map[string]string) {
	s.refs++
	fileInput := child(with, rule.fileInput)
	versionInput := child(with, rule.versionInput)
	if fileInput == nil && versionInput == nil {
		s.mismatch(file, line, action, "no "+rule.fileInput+" or "+rule.versionInput+", so the action picks the release")
		return
	}
	if fileInput != nil {
		v, ok := resolveEnv(fileInput.Value, env)
		switch want := rule.file(s.pins); {
		case !ok:
			s.mismatch(file, fileInput.Line, fileInput.Value, rule.fileInput+" cannot be resolved offline")
		case path.Clean(v) != path.Clean(want):
			s.mismatch(file, fileInput.Line, v, rule.fileInput+" must be "+want)
		}
	}
	if versionInput != nil {
		v, ok := resolveEnv(versionInput.Value, env)
		if !ok {
			s.mismatch(file, versionInput.Line, versionInput.Value, rule.versionInput+" cannot be resolved offline")
		} else if reason := rule.version(s.pins, v); reason != "" {
			s.mismatch(file, versionInput.Line, v, reason)
		}
	}
}

// resolveEnv returns a workflow value with a whole-value ${{ env.NAME }}
// expression resolved from the innermost env that defines NAME. It reports
// false for any other expression, which only a runner can evaluate.
func resolveEnv(v string, envs []map[string]string) (string, bool) {
	if m := envExpr.FindStringSubmatch(v); m != nil {
		for _, env := range envs {
			if val, ok := env[m[1]]; ok {
				return val, true
			}
		}
		return "", false
	}
	if strings.Contains(v, "${{") {
		return "", false
	}
	return v, true
}

// scanCompose checks the image of every compose service.
func (s *scanner) scanCompose(file string, data []byte) error {
	doc, err := parseYAML(file, data)
	if err != nil || doc == nil {
		return err
	}
	for _, svc := range mapValues(child(doc, "services")) {
		if img := child(svc, "image"); img != nil {
			s.checkImage(file, img.Line, img.Value)
		}
	}
	return nil
}
