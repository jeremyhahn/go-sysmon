package pins

import (
	"context"
	"errors"
	"reflect"
	"syscall"
	"testing"
)

// fixed is a LatestFunc answering v, err.
func fixed(v string, err error) LatestFunc {
	return func(context.Context) (string, error) { return v, err }
}

var errUnknownModule = errors.New("fake: unknown module")

// modules is a ModuleLatestFunc answering from a table.
func modules(latest map[string]string) ModuleLatestFunc {
	return func(_ context.Context, module string) (string, error) {
		if v, ok := latest[module]; ok {
			return v, nil
		}
		return "", errUnknownModule
	}
}

var lintTool = Tool{Name: "golangci-lint", Module: "github.com/golangci/golangci-lint/v2", Version: "v2.14.0"}

// verdict runs one item and returns its result, failing on no verdict.
func verdict(t *testing.T, it Item) Result {
	t.Helper()
	report, err := CheckLatest(context.Background(), []Item{it})
	if err != nil {
		t.Fatalf("CheckLatest(%s) error = %v", it.Name, err)
	}
	return report.Results[0]
}

// unchecked runs one item and requires it to reach no verdict, with cause
// reachable through the error.
func unchecked(t *testing.T, it Item, cause error) {
	t.Helper()
	report, err := CheckLatest(context.Background(), []Item{it})
	var u *UncheckedError
	if !errors.As(err, &u) {
		t.Fatalf("CheckLatest(%s) error = %v, want *UncheckedError", it.Name, err)
	}
	if cause != nil && !errors.Is(err, cause) {
		t.Errorf("errors.Is(err, %v) = false; the cause must stay reachable", cause)
	}
	if len(report.Results) != 0 {
		t.Errorf("Results = %+v, want none: an unchecked item has no verdict", report.Results)
	}
}

func TestItems_CurrentPinsPass(t *testing.T) {
	items := []Item{
		GoItem("1.27.1", fixed("go1.27.1", nil)),
		ToolItem(lintTool, modules(map[string]string{lintTool.Module: "v2.14.0"})),
		NodeItem("24.21.0", fixed("24.21.0", nil)),
		UbuntuItem("26.04", fixed("26.04", nil)),
	}
	report, err := CheckLatest(context.Background(), items)
	if err != nil {
		t.Fatalf("CheckLatest() error = %v", err)
	}
	if err := report.Err(); err != nil {
		t.Errorf("Report.Err() = %v, want nil", err)
	}
	want := []Result{
		{Name: "go", Pinned: "1.27.1", Latest: "go1.27.1", Status: Current},
		{Name: "golangci-lint", Pinned: "v2.14.0", Latest: "v2.14.0", Status: Current},
		{Name: "node", Pinned: "24.21.0", Latest: "24.21.0", Status: Current},
		{Name: "ubuntu", Pinned: "26.04", Latest: "26.04", Status: Current},
	}
	if !reflect.DeepEqual(report.Results, want) {
		t.Errorf("Results = %+v, want %+v", report.Results, want)
	}
}

func TestItems_StalePinsFail(t *testing.T) {
	items := []Item{
		GoItem("1.27.0", fixed("go1.27.1", nil)),
		ToolItem(lintTool, modules(map[string]string{lintTool.Module: "v2.15.0"})),
		NodeItem("22.23.3", fixed("24.21.0", nil)),
		UbuntuItem("24.04", fixed("26.04", nil)),
	}
	report, err := CheckLatest(context.Background(), items)
	if err != nil {
		t.Fatalf("CheckLatest() error = %v", err)
	}
	var lag *LagError
	if !errors.As(report.Err(), &lag) || len(lag.Results) != 4 {
		t.Fatalf("Report.Err() = %v, want all four lagging", report.Err())
	}
	for _, r := range lag.Results {
		if r.Status != Behind {
			t.Errorf("%s = %q, want behind", r.Name, r.Status)
		}
	}
	want := "pins: 4 item(s) not at their latest stable release:\n" +
		"  go 1.27.0: behind (latest stable is go1.27.1)\n" +
		"  golangci-lint v2.14.0: behind (latest stable is v2.15.0)\n" +
		"  node 22.23.3: behind (latest stable is 24.21.0)\n" +
		"  ubuntu 24.04: behind (latest stable is 26.04)"
	if lag.Error() != want {
		t.Errorf("Error() =\n%s\nwant\n%s", lag.Error(), want)
	}
}

// An offline check must not look like a passing one: each kind of item has no
// verdict, the error says why, and the cause is still reachable.
func TestItems_NetworkErrorIsNotUpToDate(t *testing.T) {
	offline := syscall.ECONNREFUSED
	for _, it := range []Item{
		GoItem("1.27.1", fixed("", offline)),
		ToolItem(lintTool, modules(nil)),
		NodeItem("24.21.0", fixed("", offline)),
		UbuntuItem("26.04", fixed("", offline)),
	} {
		t.Run(it.Name, func(t *testing.T) {
			cause := error(offline)
			if it.Name == lintTool.Name {
				cause = errUnknownModule
			}
			unchecked(t, it, cause)
		})
	}
}

func TestCheckLatest_KeepsVerdictsBesideUncheckedItems(t *testing.T) {
	report, err := CheckLatest(context.Background(), []Item{
		GoItem("1.27.1", fixed("go1.27.2", nil)),
		ToolItem(lintTool, modules(nil)),
	})

	var u *UncheckedError
	if !errors.As(err, &u) || len(u.Failures) != 1 || u.Failures[0].Item != "golangci-lint" {
		t.Fatalf("CheckLatest() error = %v, want golangci-lint unchecked", err)
	}
	if lag := report.Lagging(); len(lag) != 1 || lag[0].Name != "go" {
		t.Errorf("Lagging() = %+v, want go", lag)
	}
}

func TestItems_InvalidAnswerHasNoVerdict(t *testing.T) {
	for _, it := range []Item{
		GoItem("1.27.1", fixed("go1.28rc1", nil)),
		ToolItem(lintTool, modules(map[string]string{lintTool.Module: "latest"})),
		NodeItem("24.21.0", fixed("v24.21.0", nil)),
		UbuntuItem("26.04", fixed("resolute", nil)),
	} {
		t.Run(it.Name, func(t *testing.T) {
			_, err := CheckLatest(context.Background(), []Item{it})
			var verr *VersionError
			if !errors.As(err, &verr) {
				t.Fatalf("CheckLatest() error = %v, want a *VersionError inside", err)
			}
		})
	}
}

// debianArchive is a DebianReleaseFunc answering from a table of suites.
func debianArchive(suites map[string]DebianRelease, err error) DebianReleaseFunc {
	return func(_ context.Context, suite string) (DebianRelease, error) {
		if err != nil {
			return DebianRelease{}, err
		}
		r, ok := suites[suite]
		if !ok {
			return DebianRelease{}, &FetchResponseError{Source: suite, Status: 404}
		}
		return r, nil
	}
}

var archive = map[string]DebianRelease{
	"stable":   {Codename: "trixie", Version: "13.7"},
	"trixie":   {Codename: "trixie", Version: "13.7"},
	"bookworm": {Codename: "bookworm", Version: "12.13"},
	"forky":    {Codename: "forky"},
}

func TestDebianItem(t *testing.T) {
	tests := map[string]Status{
		"trixie":   Current,
		"bookworm": Behind,
		"forky":    Prerelease, // testing has no version: not a release
	}
	for pinned, want := range tests {
		t.Run(pinned, func(t *testing.T) {
			r := verdict(t, DebianItem(pinned, debianArchive(archive, nil)))
			if r.Status != want || r.Latest != "trixie (13.7)" {
				t.Errorf("DebianItem(%s) = %+v, want %q against trixie (13.7)", pinned, r, want)
			}
		})
	}
}

func TestDebianItem_Unchecked(t *testing.T) {
	offline := syscall.ENETUNREACH
	unchecked(t, DebianItem("trixie", debianArchive(nil, offline)), offline)
	// A pin the archive does not know cannot be placed.
	unchecked(t, DebianItem("sarge", debianArchive(archive, nil)), nil)
	// A stable release with no usable version cannot be compared.
	unchecked(t, DebianItem("bookworm", debianArchive(map[string]DebianRelease{"stable": {Codename: "trixie", Version: "x"}}, nil)), nil)
	unchecked(t, DebianItem("bookworm", debianArchive(map[string]DebianRelease{
		"stable": {Codename: "trixie", Version: "13.7"}, "bookworm": {Codename: "bookworm", Version: "twelve"},
	}, nil)), nil)
}

func TestParseTool(t *testing.T) {
	got, err := ParseTool("gosec=github.com/securego/gosec/v2@v2.29.0")
	if err != nil {
		t.Fatalf("ParseTool() error = %v", err)
	}
	if want := (Tool{Name: "gosec", Module: "github.com/securego/gosec/v2", Version: "v2.29.0"}); got != want {
		t.Errorf("ParseTool() = %+v, want %+v", got, want)
	}
}

func TestParseTool_Malformed(t *testing.T) {
	for _, s := range []string{"", "gosec", "=mod@v1", "gosec=mod", "gosec=@v1", "gosec=mod@"} {
		_, err := ParseTool(s)
		var serr *ToolSpecError
		if !errors.As(err, &serr) {
			t.Errorf("ParseTool(%q) error = %v, want *ToolSpecError", s, err)
		}
	}
}

func TestResultString(t *testing.T) {
	if got := (Result{Name: "go", Pinned: "1.27.1", Latest: "go1.27.1", Status: Current}).String(); got != "go 1.27.1: current" {
		t.Errorf("String() = %q", got)
	}
	got := (Result{Name: "gosec", Pinned: "v2.30.0-rc.1", Latest: "v2.29.0", Status: Prerelease}).String()
	if want := "gosec v2.30.0-rc.1: prerelease (latest stable is v2.29.0)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
