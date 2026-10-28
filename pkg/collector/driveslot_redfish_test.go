package collector

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jeremyhahn/go-sysmon/pkg/outbound"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

const (
	fakeBMCUser     = "monitor"
	fakeBMCPassword = "s3cret-bmc-pw"

	// dellemcDrives is the Drives collection of redfish-dellemc02.json's RAID
	// controller. A drive's identity is its path under it.
	dellemcDrives = "/redfish/v1/Systems/System.Embedded.1/Storage/RAID.Integrated.1-1/Drives/"
)

// fakeBMC is a Redfish service serving recorded resources over TLS. It
// answers a request that lacks the account's credential with 401, and any
// verb but GET with 405.
type fakeBMC struct {
	srv       *httptest.Server
	resources map[string]json.RawMessage
	handlers  map[string]http.HandlerFunc

	mu       sync.Mutex
	requests []string
}

func newFakeBMC(t *testing.T, fixture string) *fakeBMC {
	t.Helper()
	f := &fakeBMC{resources: map[string]json.RawMessage{}, handlers: map[string]http.HandlerFunc{}}
	if fixture != "" {
		b, err := os.ReadFile(filepath.Join("testdata", "driveslot", fixture))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &f.resources); err != nil {
			t.Fatalf("%s: %v", fixture, err)
		}
	}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBMC) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	f.mu.Unlock()

	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if user, pw, ok := r.BasicAuth(); !ok || user != fakeBMCUser || pw != fakeBMCPassword {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if h, ok := f.handlers[r.URL.Path]; ok {
		h(w, r)
		return
	}
	// A collection's later pages differ from its first only in the query.
	body, ok := f.resources[r.URL.RequestURI()]
	if !ok {
		body, ok = f.resources[r.URL.Path]
	}
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(body); err != nil {
		panic(err)
	}
}

func (f *fakeBMC) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

// config is a configuration that reaches the fake: its CA pinned and the
// account's password in a private file.
func (f *fakeBMC) config(t *testing.T) DriveSlotConfig {
	t.Helper()
	dir := t.TempDir()
	ca := filepath.Join(dir, "bmc-ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	if err := os.WriteFile(ca, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return DriveSlotConfig{
		RedfishURL:   f.srv.URL,
		Username:     fakeBMCUser,
		PasswordFile: writePasswordFile(t, fakeBMCPassword+"\n", 0o600),
		TLS:          outbound.TLS{CAFile: ca},
		IPMIDevice:   filepath.Join(dir, "ipmi0"),
	}
}

func writePasswordFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bmc-password")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func redfishSlots(t *testing.T, cfg DriveSlotConfig) ([]types.DriveSlot, error) {
	t.Helper()
	c, err := newRedfishClient(cfg)
	if err != nil {
		t.Fatalf("newRedfishClient() error = %v", err)
	}
	return c.driveSlots(context.Background())
}

// assertNoCredential fails if err carries the account's password or name.
func assertNoCredential(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, secret := range []string{fakeBMCPassword, fakeBMCUser + ":"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error %q carries a credential", err)
		}
	}
}

func TestRedfishDriveSlots_ReadsEveryBayAndTheFault(t *testing.T) {
	f := newFakeBMC(t, "redfish-dellemc02.json")
	slots, err := redfishSlots(t, f.config(t))
	if err != nil {
		t.Fatalf("driveSlots() error = %v", err)
	}
	got := map[string]types.DriveSlot{}
	for _, s := range slots {
		got[s.Slot] = s
	}
	if len(slots) != 4 || len(got) != 4 {
		t.Fatalf("driveSlots() = %d slots, want bays 0-3 once each (bay 3 is listed twice)", len(slots))
	}

	bay3 := got["Bay 3"]
	if !bay3.Present || !bay3.Fault || bay3.Health != types.DriveSlotHealthCritical || bay3.MediaLifeReported {
		t.Errorf("Bay 3 = %+v, want present, faulted, critical, no media life", bay3)
	}
	if bay3.ID != dellemcDrives+"Disk.Bay.3:Enclosure.Internal.0-1:RAID.Integrated.1-1" || bay3.SerialNumber != "S45NNA0R100004" {
		t.Errorf("Bay 3 identity = %q %q", bay3.ID, bay3.SerialNumber)
	}

	bay2 := got["Bay 2"]
	if !bay2.FailurePredicted || bay2.Fault || bay2.Health != types.DriveSlotHealthWarning || bay2.MediaLifeLeftPercent != 3 {
		t.Errorf("Bay 2 = %+v, want a predicted failure at warning with 3%% life left", bay2)
	}
	if bay1 := got["Bay 1"]; !bay1.Rebuilding || bay1.Health != types.DriveSlotHealthOK || bay1.State != "Updating" {
		t.Errorf("Bay 1 = %+v, want rebuilding and healthy", bay1)
	}
	want0 := types.DriveSlot{
		ID: dellemcDrives + "Disk.Bay.0:Enclosure.Internal.0-1:RAID.Integrated.1-1", Slot: "Bay 0", Present: true,
		Health: types.DriveSlotHealthOK, State: "Enabled", MediaLifeLeftPercent: 98, MediaLifeReported: true,
		Manufacturer: "SAMSUNG", Model: "MZ7LH960HAJR0D3", SerialNumber: "S45NNA0R100001",
		MediaType: "SSD", Protocol: "SATA", CapacityBytes: 960197124096,
	}
	if got["Bay 0"] != want0 {
		t.Errorf("Bay 0 = %+v, want %+v", got["Bay 0"], want0)
	}
}

// A drive's Id is unique only within its collection: here an NVMe and a SAS
// controller each have a drive "0", and only the SAS one is faulted. Keyed by
// Id they were one UI row, one metric label set and one alert, so the fault
// could be shadowed by the healthy drive.
func TestRedfishDriveSlots_SameIdOnTwoControllersStaysTwoBays(t *testing.T) {
	f := newFakeBMC(t, "redfish-two-controllers.json")
	slots, err := redfishSlots(t, f.config(t))
	if err != nil {
		t.Fatalf("driveSlots() error = %v", err)
	}
	got := map[string]types.DriveSlot{}
	for _, s := range slots {
		got[s.ID] = s
	}
	nvme, sas := got["/redfish/v1/Systems/1/Storage/NVMe/Drives/0"], got["/redfish/v1/Systems/1/Storage/SAS/Drives/0"]
	if len(slots) != 2 || len(got) != 2 {
		t.Fatalf("driveSlots() IDs = %v, want the two drives' canonical paths", got)
	}
	if nvme.Slot != "NVMe drive 0" || nvme.Fault {
		t.Errorf("NVMe drive 0 = %+v, want healthy", nvme)
	}
	if sas.Slot != "SAS drive 0" || !sas.Fault {
		t.Errorf("SAS drive 0 = %+v, want faulted", sas)
	}
}

// The faulted drive is on the Storage collection's second page and the drive
// predicting its failure on the Systems collection's second page. Reading
// first pages only reported one healthy bay as the whole host.
func TestRedfishDriveSlots_FollowsEveryCollectionPage(t *testing.T) {
	f := newFakeBMC(t, "redfish-paged.json")
	slots, err := redfishSlots(t, f.config(t))
	if err != nil {
		t.Fatalf("driveSlots() error = %v", err)
	}
	got := map[string]types.DriveSlot{}
	for _, s := range slots {
		got[s.Slot] = s
	}
	if len(got) != 3 {
		t.Fatalf("driveSlots() = %v, want the drives of both pages of both collections", got)
	}
	if !got["B drive 0"].Fault {
		t.Errorf("B drive 0 = %+v, want the second Storage page's fault", got["B drive 0"])
	}
	if !got["C drive 0"].FailurePredicted {
		t.Errorf("C drive 0 = %+v, want the second Systems page's predicted failure", got["C drive 0"])
	}
}

func TestRedfishDriveSlots_FollowsChassisDrivesPages(t *testing.T) {
	f := newFakeBMC(t, "redfish-chassis.json")
	f.resources["/redfish/v1/Chassis/1/Drives"] = json.RawMessage(`{
		"Members": [{"@odata.id": "/redfish/v1/Chassis/1/Drives/NVMe0"}],
		"Members@odata.nextLink": "/redfish/v1/Chassis/1/Drives?$skip=1"}`)
	f.resources["/redfish/v1/Chassis/1/Drives?$skip=1"] = json.RawMessage(`{
		"Members": [{"@odata.id": "/redfish/v1/Chassis/1/Drives/NVMe1"}]}`)
	slots, err := redfishSlots(t, f.config(t))
	if err != nil {
		t.Fatalf("driveSlots() error = %v", err)
	}
	if len(slots) != 2 {
		t.Errorf("driveSlots() = %+v, want NVMe0 and the second page's NVMe1", slots)
	}
}

// TestRedfishDriveSlots_NoSystemsFallsBackToChassis verifies a service that
// does not implement /redfish/v1/Systems, and answers it 404, still has its
// chassis drives read.
func TestRedfishDriveSlots_NoSystemsFallsBackToChassis(t *testing.T) {
	f := newFakeBMC(t, "redfish-chassis-only.json")
	slots, err := redfishSlots(t, f.config(t))
	if err != nil {
		t.Fatalf("driveSlots() error = %v", err)
	}
	if len(slots) != 2 || !slots[1].Fault || slots[1].ID != "/redfish/v1/Chassis/Enclosure/Drives/Bay1" {
		t.Errorf("driveSlots() = %+v, want Bay0 and the faulted Bay1", slots)
	}
}

// TestRedfishDriveSlots_OnlyAMissingSystemsFallsBack verifies that only a 404
// on the Systems collection itself means "no Systems". A refused account, or a
// 404 deeper in the Systems walk, still ends the poll with its error even
// though the chassis would have answered.
func TestRedfishDriveSlots_OnlyAMissingSystemsFallsBack(t *testing.T) {
	cases := map[string]struct {
		path   string
		status int
	}{
		"401 on Systems":  {"/redfish/v1/Systems", http.StatusUnauthorized},
		"403 on Systems":  {"/redfish/v1/Systems", http.StatusForbidden},
		"404 on a system": {"/redfish/v1/Systems/1", http.StatusNotFound},
		"500 on Systems":  {"/redfish/v1/Systems", http.StatusInternalServerError},
	}
	for name, tc := range cases {
		f := newFakeBMC(t, "redfish-chassis.json")
		f.handlers[tc.path] = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) }
		_, err := redfishSlots(t, f.config(t))
		var status *RedfishStatusError
		if !errors.As(err, &status) || status.StatusCode != tc.status || status.Path != tc.path {
			t.Errorf("%s: error = %v, want RedfishStatusError %d at %s", name, err, tc.status, tc.path)
		}
	}
}

// A BMC whose last page links back to its first must end the walk, not hold
// the poll until its timeout.
func TestRedfishDriveSlots_LoopingNextLinkEnds(t *testing.T) {
	f := newFakeBMC(t, "redfish-looping.json")
	_, err := redfishSlots(t, f.config(t))
	var link *RedfishLinkError
	if !errors.As(err, &link) || link.Link != "/redfish/v1/Systems" || !strings.Contains(err.Error(), "loops") {
		t.Fatalf("driveSlots() error = %v, want a RedfishLinkError for the repeated page", err)
	}
	if n := len(f.seen()); n != 2 {
		t.Errorf("the walk sent %d requests, want the two pages once each", n)
	}
}

// Links that never repeat a spelling still end, at the request budget.
func TestRedfishDriveSlots_EndlessNextLinksStopAtTheBudget(t *testing.T) {
	f := newFakeBMC(t, "")
	f.handlers["/redfish/v1/Systems"] = func(w http.ResponseWriter, r *http.Request) {
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil {
			page = 0
		}
		if _, err := fmt.Fprintf(w, `{"Members": [], "Members@odata.nextLink": "/redfish/v1/Systems?page=%d"}`, page+1); err != nil {
			panic(err)
		}
	}
	_, err := redfishSlots(t, f.config(t))
	var limit *RedfishWalkLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("driveSlots() error = %v, want RedfishWalkLimitError", err)
	}
	if n := len(f.seen()); n != redfishMaxRequests {
		t.Errorf("the walk sent %d requests, want the budget of %d", n, redfishMaxRequests)
	}
}

func TestRedfishDriveSlots_SendsOnlyGETs(t *testing.T) {
	f := newFakeBMC(t, "redfish-dellemc02.json")
	if _, err := redfishSlots(t, f.config(t)); err != nil {
		t.Fatal(err)
	}
	seen := f.seen()
	if len(seen) == 0 {
		t.Fatal("the fake BMC saw no requests")
	}
	for _, r := range seen {
		if !strings.HasPrefix(r, "GET /redfish/v1") {
			t.Errorf("request %q is not a GET under the service root", r)
		}
	}
}

func TestRedfishDriveSlots_FallsBackToChassisDrives(t *testing.T) {
	f := newFakeBMC(t, "redfish-chassis.json")
	slots, err := redfishSlots(t, f.config(t))
	if err != nil {
		t.Fatalf("driveSlots() error = %v", err)
	}
	if len(slots) != 2 {
		t.Fatalf("driveSlots() = %+v, want NVMe0 once and NVMe1", slots)
	}
	nvme0, nvme1 := slots[0], slots[1]
	if nvme0.ID != "/redfish/v1/Chassis/1/Drives/NVMe0" || nvme0.Slot != "NVMe slot 0" || nvme0.Health != types.DriveSlotHealthWarning || nvme0.CapacityBytes != 0 {
		t.Errorf("NVMe0 = %+v, want its ID from the link, Name as the slot, warning, no capacity", nvme0)
	}
	if nvme1.Present || nvme1.Health != types.DriveSlotHealthUnknown || nvme1.Slot != "NVMe1" {
		t.Errorf("NVMe1 = %+v, want an empty bay with no health", nvme1)
	}
}

func TestRedfishDriveSlots_NoDrives(t *testing.T) {
	f := newFakeBMC(t, "")
	f.resources["/redfish/v1/Systems"] = json.RawMessage(`{"Members":[]}`)
	f.resources["/redfish/v1/Chassis"] = json.RawMessage(`{"Members":[]}`)
	_, err := redfishSlots(t, f.config(t))
	var none *NoDriveSlotsError
	if !errors.As(err, &none) || none.Source != types.DriveSlotSourceRedfish {
		t.Errorf("driveSlots() error = %v, want NoDriveSlotsError from Redfish", err)
	}
}

func TestRedfishDriveSlots_RefusedAccount(t *testing.T) {
	f := newFakeBMC(t, "redfish-dellemc02.json")
	cfg := f.config(t)
	cfg.PasswordFile = writePasswordFile(t, "wrong-password", 0o600)
	_, err := redfishSlots(t, cfg)
	var status *RedfishStatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusUnauthorized {
		t.Fatalf("driveSlots() error = %v, want RedfishStatusError 401", err)
	}
	if !strings.Contains(err.Error(), "refused bmc.username") {
		t.Errorf("error %q does not say the account was refused", err)
	}
	assertNoCredential(t, err)
	if strings.Contains(err.Error(), "wrong-password") {
		t.Errorf("error %q carries the password", err)
	}
}

func TestRedfishDriveSlots_BadJSON(t *testing.T) {
	f := newFakeBMC(t, "redfish-dellemc02.json")
	f.resources["/redfish/v1/Systems"] = json.RawMessage(`{"Members": [`)
	_, err := redfishSlots(t, f.config(t))
	var decode *RedfishDecodeError
	if !errors.As(err, &decode) || decode.Path != "/redfish/v1/Systems" {
		t.Errorf("driveSlots() error = %v, want RedfishDecodeError for /redfish/v1/Systems", err)
	}
}

func TestRedfishDriveSlots_ErrorsAtEveryLevelStopTheWalk(t *testing.T) {
	paths := []string{
		"/redfish/v1/Systems/System.Embedded.1",
		"/redfish/v1/Systems/System.Embedded.1/Storage",
		"/redfish/v1/Systems/System.Embedded.1/Storage/RAID.Integrated.1-1",
		"/redfish/v1/Systems/System.Embedded.1/Storage/RAID.Integrated.1-1/Drives/Disk.Bay.0:Enclosure.Internal.0-1:RAID.Integrated.1-1",
	}
	for _, p := range paths {
		f := newFakeBMC(t, "redfish-dellemc02.json")
		f.handlers[p] = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }
		_, err := redfishSlots(t, f.config(t))
		var status *RedfishStatusError
		if !errors.As(err, &status) || status.Path != p || !strings.HasSuffix(err.Error(), "HTTP 500") {
			t.Errorf("500 at %s: error = %v", p, err)
		}
	}

	for _, p := range []string{"/redfish/v1/Chassis", "/redfish/v1/Chassis/1", "/redfish/v1/Chassis/1/Drives"} {
		f := newFakeBMC(t, "redfish-chassis.json")
		delete(f.resources, p)
		_, err := redfishSlots(t, f.config(t))
		var status *RedfishStatusError
		if !errors.As(err, &status) || status.StatusCode != http.StatusNotFound {
			t.Errorf("404 at %s: error = %v", p, err)
		}
	}
}

func TestRedfishDriveSlots_Timeout(t *testing.T) {
	f := newFakeBMC(t, "redfish-dellemc02.json")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	// The handler waits for release only. Returning when its context ends
	// would let net/http write an empty 200 that can reach the client after
	// its deadline (red main, run 69908).
	f.handlers["/redfish/v1/Systems"] = func(http.ResponseWriter, *http.Request) {
		<-release
	}
	cfg := f.config(t)
	cfg.Timeout = 50 * time.Millisecond
	_, err := redfishSlots(t, cfg)
	var req *RedfishRequestError
	if !errors.As(err, &req) || !req.Timeout {
		t.Fatalf("driveSlots() error = %v, want a timed-out RedfishRequestError", err)
	}
	if err.Error() != "GET /redfish/v1/Systems timed out" {
		t.Errorf("error = %q", err)
	}
}

// TestRedfishDriveSlots_TimeoutReadingBody covers a deadline that fires
// after the status line arrives but while the body is still being read: it
// is a timeout, not a body the client failed to decode.
func TestRedfishDriveSlots_TimeoutReadingBody(t *testing.T) {
	f := newFakeBMC(t, "redfish-dellemc02.json")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.handlers["/redfish/v1/Systems"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, `{"Members": [`); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		<-release
	}
	cfg := f.config(t)
	cfg.Timeout = 50 * time.Millisecond
	_, err := redfishSlots(t, cfg)
	var req *RedfishRequestError
	if !errors.As(err, &req) || !req.Timeout {
		t.Fatalf("driveSlots() error = %v, want a timed-out RedfishRequestError", err)
	}
	if err.Error() != "GET /redfish/v1/Systems timed out" {
		t.Errorf("error = %q", err)
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestRedfishDriveSlots_ResponseAfterDeadline covers the race behind red
// main run 69908: the transport hands back an empty response only once the
// request's deadline has passed. The failure that follows is still a
// timeout.
func TestRedfishDriveSlots_ResponseAfterDeadline(t *testing.T) {
	f := newFakeBMC(t, "redfish-dellemc02.json")
	cfg := f.config(t)
	cfg.Timeout = 50 * time.Millisecond
	c, err := newRedfishClient(cfg)
	if err != nil {
		t.Fatalf("newRedfishClient() error = %v", err)
	}
	next := c.http.Transport
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/redfish/v1/Systems" {
			return next.RoundTrip(r)
		}
		<-r.Context().Done()
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
	})
	_, err = c.driveSlots(context.Background())
	var req *RedfishRequestError
	if !errors.As(err, &req) || !req.Timeout {
		t.Fatalf("driveSlots() error = %v, want a timed-out RedfishRequestError", err)
	}
	if err.Error() != "GET /redfish/v1/Systems timed out" {
		t.Errorf("error = %q", err)
	}
}

func TestTimedOut(t *testing.T) {
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	tests := map[string]struct {
		ctx  context.Context
		err  error
		want bool
	}{
		"deadline passed":       {expired, io.EOF, true},
		"timeout error":         {context.Background(), context.DeadlineExceeded, true},
		"live request":          {context.Background(), io.EOF, false},
		"canceled, not expired": {canceled, io.ErrUnexpectedEOF, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := timedOut(tt.ctx, tt.err); got != tt.want {
				t.Errorf("timedOut() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRedfishDriveSlots_VerifiesTheBMCCertificateByDefault(t *testing.T) {
	f := newFakeBMC(t, "redfish-dellemc02.json")
	cfg := f.config(t)
	cfg.TLS = outbound.TLS{}
	_, err := redfishSlots(t, cfg)
	var req *RedfishRequestError
	if !errors.As(err, &req) || req.Timeout {
		t.Fatalf("driveSlots() against an unpinned self-signed BMC error = %v, want a verification failure", err)
	}
	if len(f.seen()) != 0 {
		t.Errorf("requests reached a BMC whose certificate did not verify: %v", f.seen())
	}

	cfg.TLS = outbound.TLS{InsecureSkipVerify: true}
	if _, err := redfishSlots(t, cfg); err != nil {
		t.Errorf("driveSlots() with the explicit opt-out error = %v", err)
	}
}

func TestRedfishClient_RefusesLinksOffTheBMC(t *testing.T) {
	c, err := newRedfishClient(DriveSlotConfig{RedfishURL: "https://bmc.example:8443/ignored", Username: "u", PasswordFile: "p"})
	if err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{
		"https://elsewhere.example/redfish/v1/Systems",
		"http://bmc.example:8443/redfish/v1/Systems",
		"//elsewhere.example/redfish/v1",
		"/redfish/v10/Systems",
		"/redfish/v1/../../etc",
		"/other",
		"%zz",
	} {
		_, err := c.resolve(link)
		var linkErr *RedfishLinkError
		if !errors.As(err, &linkErr) {
			t.Errorf("resolve(%q) error = %v, want RedfishLinkError", link, err)
		}
	}
	for _, link := range []string{"/redfish/v1", "/redfish/v1/Systems/1", "https://bmc.example:8443/redfish/v1/Chassis"} {
		if u, err := c.resolve(link); err != nil || u.Host != "bmc.example:8443" {
			t.Errorf("resolve(%q) = %v, %v", link, u, err)
		}
	}
}

func TestRedfishDriveSlots_OffHostLinkIsNotFollowed(t *testing.T) {
	f := newFakeBMC(t, "redfish-dellemc02.json")
	f.resources["/redfish/v1/Systems"] = json.RawMessage(`{"Members":[{"@odata.id":"https://attacker.example/redfish/v1/Systems/1"}]}`)
	_, err := redfishSlots(t, f.config(t))
	var linkErr *RedfishLinkError
	if !errors.As(err, &linkErr) || !strings.Contains(err.Error(), "leaves the configured BMC") {
		t.Errorf("driveSlots() error = %v, want RedfishLinkError", err)
	}
}

func TestRedfishWalk_StopsAtTheRequestLimit(t *testing.T) {
	w := &redfishWalk{client: &redfishClient{}, requests: redfishMaxRequests}
	var dst redfishCollection
	err := w.get(context.Background(), redfishRoot, &dst)
	var limit *RedfishWalkLimitError
	if !errors.As(err, &limit) || !strings.Contains(err.Error(), "512 requests") {
		t.Errorf("get() past the limit error = %v, want RedfishWalkLimitError", err)
	}
}

func TestNewRedfishClient_RefusesUnusableConfiguration(t *testing.T) {
	cases := map[string]DriveSlotConfig{
		"unparsable":      {RedfishURL: "https://bmc:pw@[::1", Username: "u", PasswordFile: "p"},
		"credentials":     {RedfishURL: "https://root:calvin@bmc.example", Username: "u", PasswordFile: "p"},
		"plain http":      {RedfishURL: "http://bmc.example", Username: "u", PasswordFile: "p"},
		"no host":         {RedfishURL: "https://", Username: "u", PasswordFile: "p"},
		"no username":     {RedfishURL: "https://bmc.example", PasswordFile: "p"},
		"no password":     {RedfishURL: "https://bmc.example", Username: "u"},
		"missing CA file": {RedfishURL: "https://bmc.example", Username: "u", PasswordFile: "p", TLS: outbound.TLS{CAFile: "/nonexistent/ca.pem"}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := newRedfishClient(cfg)
			if err == nil {
				t.Fatal("newRedfishClient() accepted it")
			}
			for _, secret := range []string{"calvin", "pw@"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error %q quotes the URL's credential", err)
				}
			}
		})
	}
}

func TestReadPasswordFile(t *testing.T) {
	pw, err := readPasswordFile(writePasswordFile(t, "hunter2\r\n", 0o640))
	if err != nil || string(pw) != "hunter2" {
		t.Errorf("readPasswordFile() = %q, %v, want hunter2 without the line ending", pw, err)
	}

	cases := map[string]struct {
		path string
		want error
	}{
		"world readable": {writePasswordFile(t, "hunter2", 0o644), nil},
		"empty":          {writePasswordFile(t, "\n", 0o600), nil},
		"missing":        {filepath.Join(t.TempDir(), "none"), fs.ErrNotExist},
	}
	for name, tc := range cases {
		_, err := readPasswordFile(tc.path)
		var pfErr *BMCPasswordFileError
		if !errors.As(err, &pfErr) || (tc.want != nil && !errors.Is(err, tc.want)) {
			t.Errorf("%s: readPasswordFile() error = %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: error %q carries the password", name, err)
		}
	}
}

func TestRedfishDriveSlots_UnreadablePasswordFileSendsNothing(t *testing.T) {
	f := newFakeBMC(t, "redfish-dellemc02.json")
	cfg := f.config(t)
	cfg.PasswordFile = writePasswordFile(t, fakeBMCPassword, 0o604)
	_, err := redfishSlots(t, cfg)
	var pfErr *BMCPasswordFileError
	if !errors.As(err, &pfErr) || !strings.Contains(err.Error(), "readable by other users") {
		t.Errorf("driveSlots() error = %v, want BMCPasswordFileError", err)
	}
	if len(f.seen()) != 0 {
		t.Errorf("requests went out without a usable credential: %v", f.seen())
	}
}

func TestRedfishErrors_SayWhatFailed(t *testing.T) {
	cause := errors.New("connection refused")
	cases := map[string]struct {
		err  error
		want string
	}{
		"request":   {&RedfishRequestError{Path: "/redfish/v1", Err: cause}, "GET /redfish/v1: connection refused"},
		"forbidden": {&RedfishStatusError{Path: "/redfish/v1", StatusCode: 403}, "GET /redfish/v1: the BMC refused bmc.username (HTTP 403)"},
		"link":      {&RedfishLinkError{Link: "/x", Reason: "is outside /redfish/v1"}, `redfish link "/x" is outside /redfish/v1`},
		"config":    {&RedfishConfigError{Reason: "bmc.redfish_url has no host"}, "drive-slot: bmc.redfish_url has no host"},
	}
	for name, tc := range cases {
		if tc.err.Error() != tc.want {
			t.Errorf("%s: %q, want %q", name, tc.err, tc.want)
		}
	}
	if !errors.Is(&RedfishRequestError{Err: cause}, cause) || !errors.Is(&RedfishDecodeError{Err: cause}, cause) {
		t.Error("Redfish errors do not unwrap to their cause")
	}
}
