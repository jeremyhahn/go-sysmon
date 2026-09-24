package pins

import (
	"io/fs"
	"os"
	"path"
	"strings"

	"go.yaml.in/yaml/v3"
)

// skipDirs are directories whose contents are not this module's build
// configuration: version control, third-party trees and test fixtures.
var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	"testdata":     true,
}

// scanFunc reads one file. file is slash-separated and relative to the root.
type scanFunc func(file string, data []byte) error

// walkFiles visits every file under root that pick returns a scanFunc for.
// The walk and every read go through an os.Root, so a symlink in the tree
// cannot lead a scan to read a file outside it.
func walkFiles(root string, pick func(p, name string) scanFunc) error {
	dir, err := os.OpenRoot(root)
	if err != nil {
		return &SourceError{Path: root, Cause: err}
	}
	err = fs.WalkDir(dir.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return &SourceError{Path: p, Cause: err}
		}
		if d.IsDir() {
			if p != "." && skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		scan := pick(p, d.Name())
		if scan == nil {
			return nil
		}
		data, err := dir.ReadFile(p)
		if err != nil {
			return &SourceError{Path: p, Cause: err}
		}
		return scan(p, data)
	})
	if closeErr := dir.Close(); err == nil && closeErr != nil {
		err = &SourceError{Path: root, Cause: closeErr}
	}
	return err
}

func isYAML(name string) bool {
	return strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")
}

// isWorkflow reports whether the file at p is an Actions workflow.
func isWorkflow(p, name string) bool {
	dir := path.Dir(p)
	return isYAML(strings.ToLower(name)) && (dir == ".github/workflows" || dir == ".gitea/workflows")
}

// parseYAML returns the top-level mapping of a YAML document, or nil for an
// empty one.
func parseYAML(file string, data []byte) (*yaml.Node, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, &SourceError{Path: file, Cause: err}
	}
	if len(root.Content) == 0 {
		return nil, nil
	}
	return root.Content[0], nil
}

// child returns the value under key in a mapping node, or nil.
func child(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// mapValues returns the values of a mapping node in document order.
func mapValues(n *yaml.Node) []*yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	values := make([]*yaml.Node, 0, len(n.Content)/2)
	for i := 1; i < len(n.Content); i += 2 {
		values = append(values, n.Content[i])
	}
	return values
}

// stringMap returns the scalar entries of a mapping node.
func stringMap(n *yaml.Node) map[string]string {
	m := map[string]string{}
	if n == nil || n.Kind != yaml.MappingNode {
		return m
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if v := n.Content[i+1]; v.Kind == yaml.ScalarNode {
			m[n.Content[i].Value] = v.Value
		}
	}
	return m
}

// workflowSteps calls visit for every step that uses an action or image, with
// the env maps a ${{ env.NAME }} in that step resolves from, innermost first.
// Job containers and service containers are passed to image.
func workflowSteps(doc *yaml.Node, image func(line int, ref string), visit func(step, uses *yaml.Node, env []map[string]string)) {
	workflowEnv := stringMap(child(doc, "env"))
	for _, job := range mapValues(child(doc, "jobs")) {
		jobEnv := stringMap(child(job, "env"))
		if c := child(job, "container"); c != nil {
			if c.Kind == yaml.ScalarNode {
				image(c.Line, c.Value)
			} else if img := child(c, "image"); img != nil {
				image(img.Line, img.Value)
			}
		}
		for _, svc := range mapValues(child(job, "services")) {
			if img := child(svc, "image"); img != nil {
				image(img.Line, img.Value)
			}
		}
		steps := child(job, "steps")
		if steps == nil || steps.Kind != yaml.SequenceNode {
			continue
		}
		for _, step := range steps.Content {
			uses := child(step, "uses")
			if uses == nil {
				continue
			}
			if ref, ok := strings.CutPrefix(uses.Value, "docker://"); ok {
				image(uses.Line, ref)
				continue
			}
			visit(step, uses, []map[string]string{stringMap(child(step, "env")), jobEnv, workflowEnv})
		}
	}
}
