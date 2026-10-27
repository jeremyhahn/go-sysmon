package collector

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/jeremyhahn/go-sysmon/pkg/outbound"
	"github.com/jeremyhahn/go-sysmon/pkg/types"
)

const (
	// redfishRoot is the Redfish service root. Every link the walk follows
	// must stay under it.
	redfishRoot = "/redfish/v1"

	// redfishMaxBody bounds one response. A drive resource is a few KiB; a
	// BMC that streams more than this is not answering the question asked.
	redfishMaxBody = 4 << 20

	// redfishMaxRequests bounds one walk, so a BMC whose collections link in
	// a circle, or list thousands of members, cannot keep a poll running.
	redfishMaxRequests = 512
)

// redfishHealth maps Redfish Status.Health onto the shared scale.
var redfishHealth = map[string]types.DriveSlotHealth{
	"OK":       types.DriveSlotHealthOK,
	"Warning":  types.DriveSlotHealthWarning,
	"Critical": types.DriveSlotHealthCritical,
}

// redfishClient reads drives from a BMC's Redfish service. It has one verb,
// GET: nothing in it can change the BMC.
type redfishClient struct {
	base         *url.URL
	username     string
	passwordFile string
	http         *http.Client
}

func newRedfishClient(cfg DriveSlotConfig) (*redfishClient, error) {
	// The URL is never quoted in an error: url.Parse's own message repeats
	// the input, and a mistyped one may carry a password.
	u, err := url.Parse(cfg.RedfishURL)
	switch {
	case err != nil:
		return nil, &RedfishConfigError{Reason: "bmc.redfish_url does not parse"}
	case u.User != nil:
		return nil, &RedfishConfigError{Reason: "bmc.redfish_url must not carry credentials; set bmc.username and bmc.password_file"}
	case u.Scheme != "https":
		return nil, &RedfishConfigError{Reason: "bmc.redfish_url must be https: the BMC password travels with every request"}
	case u.Host == "":
		return nil, &RedfishConfigError{Reason: "bmc.redfish_url has no host"}
	case cfg.Username == "" || cfg.PasswordFile == "":
		return nil, &RedfishConfigError{Reason: "bmc.redfish_url needs bmc.username and bmc.password_file"}
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultBMCTimeout
	}
	client, err := outbound.NewTLSClient(timeout, cfg.TLS)
	if err != nil {
		return nil, err
	}
	return &redfishClient{
		base:         &url.URL{Scheme: u.Scheme, Host: u.Host},
		username:     cfg.Username,
		passwordFile: cfg.PasswordFile,
		http:         client,
	}, nil
}

// driveSlots walks Systems/*/Storage/*/Drives, or the Chassis drive links when
// the systems list none, and returns one slot per drive.
func (c *redfishClient) driveSlots(ctx context.Context) ([]types.DriveSlot, error) {
	auth, err := c.authorization()
	if err != nil {
		return nil, err
	}
	w := &redfishWalk{client: c, auth: auth}

	refs, err := w.systemDrives(ctx)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		if refs, err = w.chassisDrives(ctx); err != nil {
			return nil, err
		}
	}
	if len(refs) == 0 {
		return nil, &NoDriveSlotsError{Source: types.DriveSlotSourceRedfish}
	}

	slots := make([]types.DriveSlot, 0, len(refs))
	for _, ref := range refs {
		var d redfishDrive
		if err := w.get(ctx, ref, &d); err != nil {
			return nil, err
		}
		slots = append(slots, d.slot(ref))
	}
	return slots, nil
}

// authorization builds the Basic credential for one poll from the password
// file. Every intermediate buffer is cleared before it returns; the header
// string itself is the one copy net/http needs, and it lives only as long as
// the poll's requests.
func (c *redfishClient) authorization() (string, error) {
	pw, err := readPasswordFile(c.passwordFile)
	if err != nil {
		return "", err
	}
	defer clear(pw)

	cred := make([]byte, 0, len(c.username)+1+len(pw))
	cred = append(cred, c.username...)
	cred = append(cred, ':')
	cred = append(cred, pw...)
	defer clear(cred)

	enc := make([]byte, base64.StdEncoding.EncodedLen(len(cred)))
	base64.StdEncoding.Encode(enc, cred)
	defer clear(enc)
	return "Basic " + string(enc), nil
}

// readPasswordFile reads the BMC password. A file that other users can read
// is refused: the password is the BMC's, and the BMC is the machine.
func readPasswordFile(name string) ([]byte, error) {
	st, err := os.Stat(name)
	if err != nil {
		return nil, &BMCPasswordFileError{Path: name, Err: err}
	}
	if st.Mode().Perm()&0o007 != 0 {
		return nil, &BMCPasswordFileError{Path: name, Reason: "is readable by other users; chmod o-rwx it"}
	}
	b, err := os.ReadFile(name)
	if err != nil {
		return nil, &BMCPasswordFileError{Path: name, Err: err}
	}
	pw := bytes.TrimRight(b, "\r\n")
	if len(pw) == 0 {
		clear(b)
		return nil, &BMCPasswordFileError{Path: name, Reason: "is empty"}
	}
	return pw, nil
}

// get fetches one resource into dst. It is the only request the client makes.
func (c *redfishClient) get(ctx context.Context, auth, ref string, dst any) (err error) {
	u, err := c.resolve(ref)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return &RedfishRequestError{Path: u.Path, Err: err}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("OData-Version", "4.0")
	req.Header.Set("Authorization", auth)

	resp, err := c.http.Do(req)
	if err != nil {
		return &RedfishRequestError{Path: u.Path, Timeout: isTimeout(err), Err: err}
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil && err == nil {
			err = &RedfishRequestError{Path: u.Path, Err: closeErr}
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return &RedfishStatusError{Path: u.Path, StatusCode: resp.StatusCode}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, redfishMaxBody)).Decode(dst); err != nil {
		return &RedfishDecodeError{Path: u.Path, Err: err}
	}
	return nil
}

// resolve turns an @odata.id into a URL on the configured BMC. A link to any
// other host, or outside the service root, is refused: the request would
// carry the BMC credential to wherever the response pointed.
func (c *redfishClient) resolve(ref string) (*url.URL, error) {
	r, err := url.Parse(ref)
	if err != nil {
		return nil, &RedfishLinkError{Link: ref, Reason: "does not parse"}
	}
	u := c.base.ResolveReference(r)
	if u.Scheme != c.base.Scheme || u.Host != c.base.Host {
		return nil, &RedfishLinkError{Link: ref, Reason: "leaves the configured BMC"}
	}
	if u.Path != redfishRoot && !strings.HasPrefix(u.Path, redfishRoot+"/") {
		return nil, &RedfishLinkError{Link: ref, Reason: "is outside " + redfishRoot}
	}
	return u, nil
}

// isTimeout reports whether a request failed by running out of time.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// redfishWalk is one poll's traversal: the credential it carries and the
// requests it has spent.
type redfishWalk struct {
	client   *redfishClient
	auth     string
	requests int
}

func (w *redfishWalk) get(ctx context.Context, ref string, dst any) error {
	if w.requests >= redfishMaxRequests {
		return &RedfishWalkLimitError{Limit: redfishMaxRequests}
	}
	w.requests++
	return w.client.get(ctx, w.auth, ref, dst)
}

// systemDrives lists the drive links of every storage subsystem of every
// computer system.
func (w *redfishWalk) systemDrives(ctx context.Context) ([]string, error) {
	systems, err := w.members(ctx, redfishRoot+"/Systems")
	if err != nil {
		// A service that does not implement Systems answers the walk's first
		// request 404; its drives, if it has any, are under Chassis. Any other
		// failure, or a 404 after that first request, still ends the poll.
		if w.requests == 1 && isRedfishNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	refs := newRefSet()
	for _, m := range systems {
		var sys redfishSystem
		if err := w.get(ctx, m.ID, &sys); err != nil {
			return nil, err
		}
		if sys.Storage.ID == "" {
			continue
		}
		storages, err := w.members(ctx, sys.Storage.ID)
		if err != nil {
			return nil, err
		}
		for _, sm := range storages {
			var st redfishStorage
			if err := w.get(ctx, sm.ID, &st); err != nil {
				return nil, err
			}
			refs.add(st.Drives)
		}
	}
	return refs.list, nil
}

// isRedfishNotFound reports whether err is the BMC answering 404.
func isRedfishNotFound(err error) bool {
	var status *RedfishStatusError
	return errors.As(err, &status) && status.StatusCode == http.StatusNotFound
}

// chassisDrives lists the drives every chassis links to, from its Drives
// collection (Redfish 2020.1 and later) or its Links.Drives (earlier).
func (w *redfishWalk) chassisDrives(ctx context.Context) ([]string, error) {
	chassis, err := w.members(ctx, redfishRoot+"/Chassis")
	if err != nil {
		return nil, err
	}
	refs := newRefSet()
	for _, m := range chassis {
		var ch redfishChassis
		if err := w.get(ctx, m.ID, &ch); err != nil {
			return nil, err
		}
		refs.add(ch.Links.Drives)
		if ch.Drives.ID == "" {
			continue
		}
		drives, err := w.members(ctx, ch.Drives.ID)
		if err != nil {
			return nil, err
		}
		refs.add(drives)
	}
	return refs.list, nil
}

// members reads a collection's members across all of its pages. A BMC may
// split a large collection into pages chained by Members@odata.nextLink, and
// reading only the first would miss bays, and their faults, while the
// reading still reported as available.
//
// Every page goes through get, so each link is validated and counted against
// the request budget. A link back to a page already read is an error rather
// than a loop. A loop that reaches the same page under other spellings still
// ends, at the request budget.
func (w *redfishWalk) members(ctx context.Context, ref string) ([]odataRef, error) {
	var all []odataRef
	read := map[string]bool{}
	for ref != "" {
		if read[ref] {
			return nil, &RedfishLinkError{Link: ref, Reason: "loops back to a collection page already read"}
		}
		read[ref] = true
		var page redfishCollection
		if err := w.get(ctx, ref, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Members...)
		ref = page.NextLink
	}
	return all, nil
}

// refSet is drive links in first-seen order, each once: a drive listed by
// both a storage subsystem and a chassis is one bay.
type refSet struct {
	seen map[string]bool
	list []string
}

func newRefSet() *refSet { return &refSet{seen: map[string]bool{}} }

func (s *refSet) add(refs []odataRef) {
	for _, r := range refs {
		if r.ID == "" || s.seen[r.ID] {
			continue
		}
		s.seen[r.ID] = true
		s.list = append(s.list, r.ID)
	}
}

// The Redfish resources the walk reads, cut down to the properties it uses.
type (
	odataRef struct {
		ID string `json:"@odata.id"`
	}
	redfishCollection struct {
		Members  []odataRef `json:"Members"`
		NextLink string     `json:"Members@odata.nextLink"`
	}
	redfishSystem struct {
		Storage odataRef `json:"Storage"`
	}
	redfishStorage struct {
		Drives []odataRef `json:"Drives"`
	}
	redfishChassis struct {
		Drives odataRef `json:"Drives"`
		Links  struct {
			Drives []odataRef `json:"Drives"`
		} `json:"Links"`
	}
	redfishDrive struct {
		ODataID string `json:"@odata.id"`
		ID      string `json:"Id"`
		Name    string `json:"Name"`
		Status  struct {
			State  string `json:"State"`
			Health string `json:"Health"`
		} `json:"Status"`
		PredictedMediaLifeLeftPercent *float64 `json:"PredictedMediaLifeLeftPercent"`
		FailurePredicted              bool     `json:"FailurePredicted"`
		Manufacturer                  string   `json:"Manufacturer"`
		Model                         string   `json:"Model"`
		SerialNumber                  string   `json:"SerialNumber"`
		MediaType                     string   `json:"MediaType"`
		Protocol                      string   `json:"Protocol"`
		CapacityBytes                 *int64   `json:"CapacityBytes"`
		PhysicalLocation              struct {
			PartLocation struct {
				ServiceLabel string `json:"ServiceLabel"`
			} `json:"PartLocation"`
		} `json:"PhysicalLocation"`
		Operations []struct {
			OperationName string `json:"OperationName"`
			Operation     string `json:"Operation"`
		} `json:"Operations"`
	}
)

// slot maps a Redfish Drive onto the shared record. ref is the link it was
// read from.
//
// The identity is the drive's canonical @odata.id path, or ref when the BMC
// omits it, and not its Id: an Id is unique only within its collection, so
// two storage controllers can each have a drive "0". Those two would share a
// UI row, a metric label set and an alert.
func (d *redfishDrive) slot(ref string) types.DriveSlot {
	s := types.DriveSlot{
		ID:               strings.TrimSuffix(firstNonEmpty(d.ODataID, ref), "/"),
		State:            d.Status.State,
		Present:          d.Status.State != "Absent",
		FailurePredicted: d.FailurePredicted,
		Manufacturer:     d.Manufacturer,
		Model:            d.Model,
		SerialNumber:     d.SerialNumber,
		MediaType:        d.MediaType,
		Protocol:         d.Protocol,
	}
	s.Slot = firstNonEmpty(d.PhysicalLocation.PartLocation.ServiceLabel, d.Name, d.ID, path.Base(s.ID))
	if s.Present {
		s.Health = redfishHealth[d.Status.Health]
	}
	s.Fault = s.Health == types.DriveSlotHealthCritical
	if s.FailurePredicted && s.Health != types.DriveSlotHealthCritical {
		s.Health = types.DriveSlotHealthWarning
	}
	if d.PredictedMediaLifeLeftPercent != nil {
		s.MediaLifeLeftPercent = *d.PredictedMediaLifeLeftPercent
		s.MediaLifeReported = true
	}
	if d.CapacityBytes != nil && *d.CapacityBytes > 0 {
		s.CapacityBytes = uint64(*d.CapacityBytes)
	}
	for _, op := range d.Operations {
		if strings.Contains(strings.ToLower(op.OperationName+op.Operation), "rebuild") {
			s.Rebuilding = true
		}
	}
	return s
}

// firstNonEmpty returns the first of vals that is not empty.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// RedfishConfigError reports a Redfish configuration that can never work. It
// never quotes the URL, which may carry a credential.
type RedfishConfigError struct {
	Reason string
}

func (e *RedfishConfigError) Error() string { return "drive-slot: " + e.Reason }

// BMCPasswordFileError reports the BMC password file being unusable. It names
// the file and never its contents.
type BMCPasswordFileError struct {
	Path   string
	Reason string
	Err    error
}

func (e *BMCPasswordFileError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("bmc.password_file %s: %v", e.Path, e.Err)
	}
	return fmt.Sprintf("bmc.password_file %s %s", e.Path, e.Reason)
}

func (e *BMCPasswordFileError) Unwrap() error { return e.Err }

// RedfishRequestError reports a request that got no response: a refused
// connection, a TLS verification failure, or a timeout.
type RedfishRequestError struct {
	Path    string
	Timeout bool
	Err     error
}

func (e *RedfishRequestError) Error() string {
	if e.Timeout {
		return fmt.Sprintf("GET %s timed out", e.Path)
	}
	return fmt.Sprintf("GET %s: %v", e.Path, e.Err)
}

func (e *RedfishRequestError) Unwrap() error { return e.Err }

// RedfishStatusError reports a response other than 200. A 401 or 403 means
// the account was refused; the error says so without repeating it.
type RedfishStatusError struct {
	Path       string
	StatusCode int
}

func (e *RedfishStatusError) Error() string {
	if e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden {
		return fmt.Sprintf("GET %s: the BMC refused bmc.username (HTTP %d)", e.Path, e.StatusCode)
	}
	return fmt.Sprintf("GET %s: HTTP %d", e.Path, e.StatusCode)
}

// RedfishDecodeError reports a response that is not the JSON resource asked
// for.
type RedfishDecodeError struct {
	Path string
	Err  error
}

func (e *RedfishDecodeError) Error() string {
	return fmt.Sprintf("GET %s: response is not a Redfish resource: %v", e.Path, e.Err)
}

func (e *RedfishDecodeError) Unwrap() error { return e.Err }

// RedfishLinkError reports an @odata.id the walk refused to follow.
type RedfishLinkError struct {
	Link   string
	Reason string
}

func (e *RedfishLinkError) Error() string {
	return fmt.Sprintf("redfish link %q %s", e.Link, e.Reason)
}

// RedfishWalkLimitError reports a walk that needed more requests than one
// poll allows.
type RedfishWalkLimitError struct {
	Limit int
}

func (e *RedfishWalkLimitError) Error() string {
	return fmt.Sprintf("redfish walk stopped after %d requests", e.Limit)
}
