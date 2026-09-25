//go:build integration

package tools

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jeremyhahn/go-sysmon/test/integration/harness"
)

// generated are the files tsgen writes, beside the dashboard's other types.
var generated = []string{"types.generated.ts", "schema.generated.ts"}

// libDir is where the committed generated files live.
func libDir() string {
	return filepath.Join(harness.RepoRoot, "cmd", "sysmon", "frontend", "src", "lib")
}

// tsgen runs the generator from source into out.
func tsgen(t *testing.T, source, out string) harness.Result {
	t.Helper()
	return harness.Run(t, harness.Bins.Tsgen, nil, "-source", source, "-out", out)
}

// TestTsgen_RegeneratesTheCommittedFilesExactly runs `make generate` into a
// scratch directory from the real pkg/types/types.go and requires the
// committed files byte for byte, as check-generated does. Run a second time
// over its own output, it replaces the pair in place with the same bytes.
func TestTsgen_RegeneratesTheCommittedFilesExactly(t *testing.T) {
	out := t.TempDir()
	source := filepath.Join(harness.RepoRoot, "pkg", "types", "types.go")

	for pass := 1; pass <= 2; pass++ {
		r := tsgen(t, source, out)
		wantExit(t, r, 0)
		for _, name := range generated {
			got, err := os.ReadFile(filepath.Join(out, name))
			if err != nil {
				t.Fatalf("pass %d: %v", pass, err)
			}
			want, err := os.ReadFile(filepath.Join(libDir(), name))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("pass %d: %s differs from the committed file; run make generate", pass, name)
			}
		}
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(generated) {
		t.Errorf("the output directory holds %d entries, want only the %d generated files", len(entries), len(generated))
	}
}

// wireSource is a types file declaring every root tsgen emits, with body
// inside Snapshot and extra declarations after.
func wireSource(t *testing.T, body, extra string) string {
	t.Helper()
	src := "package types\n\nimport \"time\"\n\nvar _ time.Time\n\n" +
		"type Snapshot struct {\n" + body + "\n}\n\n" +
		"type Fleet struct{}\ntype PeerSnapshot struct{}\ntype ConfigView struct{}\n" +
		"type History struct{}\ntype HistoryCatalog struct{}\ntype HistorySeries struct{}\n\n" + extra
	return harness.WriteFile(t, "types.go", src)
}

// TestTsgen_RefusesWhatItCannotModel checks every construct the generator has
// no faithful TypeScript for stops it -- exit 1, the reason named, and no
// file written -- rather than emitting types that drift from the wire.
func TestTsgen_RefusesWhatItCannotModel(t *testing.T) {
	tests := map[string]struct {
		body, extra string
		want        string
	}{
		"a byte slice": {
			body: "Blob []byte `json:\"blob\"`",
			want: "tsgen: Snapshot.Blob: no TypeScript form for Go type []byte",
		},
		"a channel": {
			body: "Ch chan int `json:\"ch\"`",
			want: "tsgen: Snapshot.Ch: no TypeScript form for Go type chan int",
		},
		"a fixed-size array": {
			body: "Last [3]float64 `json:\"last\"`",
			want: "tsgen: Snapshot.Last: no TypeScript form for Go type [3]float64",
		},
		"two names sharing one json name": {
			body: "A, B string `json:\"x\"`",
			want: "several names share one json name tag",
		},
		"a value sent as a quoted string": {
			body: "N int `json:\"n,string\"`",
			want: `json ",string" sends the value as a quoted string`,
		},
		"an embedded qualified type": {
			body: "time.Duration",
			want: "embedded type time.Duration is not a struct declared on the wire",
		},
		"an embedded unexported struct": {
			body:  "inner",
			extra: "type inner struct{ X int }\n",
			want:  "embedded type inner is not an exported struct",
		},
		"an embedding cycle": {
			body:  "Loop",
			extra: "type Loop struct{ Back }\ntype Back struct{ Loop }\n",
			want:  "embedding cycle through",
		},
		"two embedded fields sharing a key": {
			body:  "Left\nRight",
			extra: "type Left struct{ X int }\ntype Right struct{ X int }\n",
			want:  "share the json key X at the same depth",
		},
		"an enum constant that calls a function": {
			body:  "K Kind `json:\"k\"`",
			extra: "type Kind string\n\nconst KindA Kind = Kind(compute())\n\nfunc compute() string { return \"a\" }\n",
			want:  "tsgen: const KindA of type Kind: call compute is not a conversion to a string type",
		},
		"an enum constant that is not a string": {
			body:  "K Kind `json:\"k\"`",
			extra: "type Kind string\n\nconst KindA Kind = 1\n",
			want:  "tsgen: const KindA of type Kind: value is not a string",
		},
		"an enum constant built with an operator other than +": {
			body:  "K Kind `json:\"k\"`",
			extra: "type Kind string\n\nconst KindA = Kind(\"a\") - \"b\"\n",
			want:  "tsgen: const KindA of type Kind: operator - is not string concatenation",
		},
		"an enum constant from a name declared nowhere": {
			body:  "K Kind `json:\"k\"`",
			extra: "type Kind string\n\nconst KindA Kind = other.Name\n",
			want:  "expression other.Name is not a constant the generator can compute",
		},
		"an enum constant from a later constant": {
			body:  "K Kind `json:\"k\"`",
			extra: "type Kind string\n\nconst KindA Kind = kindB\n\nconst kindB = \"b\"\n",
			want:  "kindB is not a string constant declared earlier in the file",
		},
		"two enums concatenated": {
			body:  "K Kind `json:\"k\"`",
			extra: "type Kind string\ntype Other string\n\nconst (\n\tOtherA Other = \"o\"\n\tKindA = Kind(\"k\") + OtherA\n)\n",
			want:  "concatenates Kind and Other",
		},
		"an enum constant from another and a name declared nowhere": {
			body:  "K Kind `json:\"k\"`",
			extra: "type Kind string\n\nconst (\n\tKindA Kind = \"a\"\n\tKindB = KindA + missing\n)\n",
			want:  "tsgen: const KindB of type Kind: missing is not a string constant declared earlier in the file",
		},
		"a slice of channels": {
			body: "Chs []chan int `json:\"chs\"`",
			want: "tsgen: Snapshot.Chs: no TypeScript form for Go type chan int",
		},
		"a pointer to a map": {
			body: "Labels *map[string]string `json:\"labels\"`",
			want: "tsgen: Snapshot.Labels: no TypeScript form for Go type map[string]string",
		},
		"an enum constant with an operand that cannot be computed": {
			body:  "K Kind `json:\"k\"`",
			extra: "type Kind string\n\nconst KindA = Kind(\"a\") + (missing)\n",
			want:  "missing is not a string constant declared earlier in the file",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			out := t.TempDir()
			r := tsgen(t, wireSource(t, tt.body, tt.extra), out)
			wantExit(t, r, 1)
			wantContains(t, "stderr", r.Stderr, tt.want)
			assertEmpty(t, out)
		})
	}
}

// assertEmpty fails the test if dir holds anything: a refused run writes no
// file, not even half the pair.
func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a refused run left %d entries in %s", len(entries), dir)
	}
}

// richSource exercises every form the generator models: enum values spelled
// every way Go allows a constant (parentheses, concatenation, conversions,
// references, implicit repetition), a named string with no constants, time,
// pointers, slices of pointers and of slices, omitted and skipped fields, a
// key that is not an identifier, an interpreted-quoted tag, an untagged field,
// a tagged embedding, and promoted fields with encoding/json's dominance.
const richSource = `package types

import "time"

// Level is how bad an alert is.
type Level string

// The levels, spelled the ways Go allows a constant to be.
const (
	LevelInfo Level = "info"
	LevelWarn Level = ("warn")
	LevelCrit       = Level("crit" + "ical")
	levelPrefix     = "sev-"
	LevelSev        = Level(levelPrefix + "1")
	LevelAlias      = LevelInfo
	LevelPre        = "pre-" + LevelInfo
	levelBase       = string("base")
	LevelBase Level = Level(levelBase)
	notOnTheWire    = 42
)

// month is typed by another package; it is not a wire enum.
const month time.Month = 1

// Grouped repeats its type and value implicitly.
type Grouped string

const (
	GroupedA Grouped = "a"
	GroupedB
)

// Free is a named string with no constants.
type Free string

// Snapshot is the root.
type Snapshot struct {
	// Name is documented.
	Name     string    ` + "`json:\"name\"`" + `
	Taken    time.Time ` + "`json:\"taken\"`" + `
	Level    Level     ` + "`json:\"level\"`" + `
	Grouped  Grouped   ` + "`json:\"grouped,omitzero\"`" + `
	Free     Free      ` + "`json:\"free\"`" + `
	Display  string    ` + "`json:\"display-name\"`" + `
	Items    []*Item   ` + "`json:\"items\"`" + `
	Grid     [][]int   ` + "`json:\"grid\"`" + `
	Maybe    *Item     ` + "`json:\"maybe,omitempty\"`" + `
	Hidden   string    ` + "`json:\"-\"`" + `
	Quoted   string    "json:\"quoted\""
	Untagged bool
	Tagged   ` + "`json:\"tagged\"`" + `
	*Promoted
	Shadow
	Level2 float64 ` + "`json:\"level2\"`" + `
	Dash   string  ` + "`json:\"-,\"`" + `
	Other  string  ` + "`yaml:\"other\"`" + `
	Skipped ` + "`json:\"-\"`" + `
	Plain
	Tagger
	private int
}

type Skipped struct{ S string }

type Plain struct{ Z int }

type Tagger struct {
	Z string ` + "`json:\"Z\"`" + `
}

type Item struct {
	ID int64 ` + "`json:\"id\"`" + `
}

type Tagged struct {
	T string ` + "`json:\"t\"`" + `
}

type Promoted struct {
	P uint8 ` + "`json:\"p\"`" + `
}

type Shadow struct {
	Name   string ` + "`json:\"name\"`" + `
	Level2 string ` + "`json:\"level2\"`" + `
	Only   string ` + "`json:\"only\"`" + `
}

type Unreached struct{ X chan int }

type Fleet struct{}
type PeerSnapshot struct{}
type ConfigView struct{}
type History struct{}
type HistoryCatalog struct{}
type HistorySeries struct{}
`

// TestTsgen_ModelsEveryWireForm generates from richSource and checks the
// TypeScript says what encoding/json sends: each enum's values once, in order;
// optional keys where the server may omit them; nulls where a pointer can
// be nil; the shallowest field winning a promoted key; nothing for skipped,
// unexported or unreachable declarations.
func TestTsgen_ModelsEveryWireForm(t *testing.T) {
	out := t.TempDir()

	r := tsgen(t, harness.WriteFile(t, "types.go", richSource), out)

	wantExit(t, r, 0)
	types, err := os.ReadFile(filepath.Join(out, generated[0]))
	if err != nil {
		t.Fatal(err)
	}
	ts := string(types)
	for _, want := range []string{
		"/** Free is a named string with no constants. */\nexport type Free = string;",
		"export type Grouped =\n  | \"a\";",
		"export type Level =\n  | \"info\"\n  | \"warn\"\n  | \"critical\"\n  | \"sev-1\"\n  | \"pre-info\"\n  | \"base\";",
		`export interface Snapshot {
  /** Name is documented. */
  name: string;
  taken: string;
  level: Level;
  grouped?: Grouped;
  free: Free;
  "display-name": string;
  items: (Item | null)[];
  grid: number[][];
  maybe?: Item | null;
  quoted: string;
  Untagged: boolean;
  tagged: Tagged;
  p?: number;
  level2: number;
  only: string;
  "-": string;
  Other: string;
  Z: string;
}`,
	} {
		wantContains(t, "types", ts, want)
	}
	for _, absent := range []string{"Hidden", "private", "Unreached", "notOnTheWire", "Promoted", "Shadow", "Skipped", "month"} {
		if strings.Contains(ts, absent) {
			t.Errorf("types mention %s, which is not on the wire", absent)
		}
	}

	schema, err := os.ReadFile(filepath.Join(out, generated[1]))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`{ key: "items", type: { kind: "array", elem: { kind: "object", ref: "Item", nullable: true }, nullable: true } },`,
		`{ key: "maybe", optional: true, type: { kind: "object", ref: "Item", nullable: true } },`,
		`{ key: "p", optional: true, type: { kind: "number" } },`,
	} {
		wantContains(t, "schema", string(schema), want)
	}
}

// TestTsgen_ReportsASourceItCannotUse covers a source that is missing, does
// not parse, or does not declare a root.
func TestTsgen_ReportsASourceItCannotUse(t *testing.T) {
	tests := map[string]struct {
		source func(t *testing.T) string
		want   string
	}{
		"missing": {
			source: func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.go") },
			want:   "tsgen: read ",
		},
		"not Go": {
			source: func(t *testing.T) string { return harness.WriteFile(t, "types.go", "package types\n\ntype {\n") },
			want:   "tsgen: read ",
		},
		"without a root": {
			source: func(t *testing.T) string {
				return harness.WriteFile(t, "types.go", "package types\n\ntype Snapshot struct{}\n")
			},
			want: "tsgen: root type Fleet is not declared in the source",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			out := t.TempDir()
			r := tsgen(t, tt.source(t), out)
			wantExit(t, r, 1)
			wantContains(t, "stderr", r.Stderr, tt.want)
			assertEmpty(t, out)
		})
	}
}

// TestTsgen_ReportsWhereItCannotWrite covers an output directory that does
// not exist, and a directory standing where a generated file goes: the pair is
// written together or not at all.
func TestTsgen_ReportsWhereItCannotWrite(t *testing.T) {
	source := filepath.Join(harness.RepoRoot, "pkg", "types", "types.go")

	missing := filepath.Join(t.TempDir(), "absent")
	r := tsgen(t, source, missing)
	wantExit(t, r, 1)
	wantContains(t, "stderr", r.Stderr, missing)

	blocked := t.TempDir()
	if err := os.Mkdir(filepath.Join(blocked, generated[1]), 0o755); err != nil {
		t.Fatal(err)
	}
	r = tsgen(t, source, blocked)
	wantExit(t, r, 1)
	wantContains(t, "stderr", r.Stderr, "is a directory")
	entries, err := os.ReadDir(blocked)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != generated[1] {
			t.Errorf("%s was written although its pair could not be", e.Name())
		}
	}
}

func TestTsgen_AMalformedCommandLineExitsTwo(t *testing.T) {
	r := harness.Run(t, harness.Bins.Tsgen, nil, "-no-such-flag")
	wantExit(t, r, 2)
	if !strings.Contains(r.Stderr, "Usage of tsgen") {
		t.Errorf("stderr = %q, want the usage", r.Stderr)
	}
}
