package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/server"
	"github.com/elk-work/ark/internal/server/repodb"
	"github.com/elk-work/ark/internal/servertest"
	"github.com/elk-work/ark/pkg/api"
)

const (
	testApprovalURL = "https://idp.example.invalid/ark-auth"
	testIDPKey      = "test-idp-key"
)

// stubApprover is the identity provider, in process. It wraps the real
// service: when a client asks for a device code it approves that code before
// the response goes back, so the first poll succeeds and no test waits out a
// five-second interval.
//
// Nothing here reaches a network. That is the line that keeps this slice
// landable on its own — the approval page is Elk's, and none of it has to
// exist for `ark login` to be tested end to end.
type stubApprover struct {
	t     *testing.T
	inner http.Handler
	key   string
	req   api.DeviceApproveRequest
	// approved records what the approver was asked to approve, so a test can
	// assert the code the client printed is the code that was approved.
	approved []string
}

func (s *stubApprover) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/device/code" {
		s.inner.ServeHTTP(w, r)
		return
	}
	rec := httptest.NewRecorder()
	s.inner.ServeHTTP(rec, r)
	if rec.Code == 200 {
		var issued api.DeviceCodeResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &issued); err != nil {
			s.t.Errorf("device code response: %v", err)
		} else {
			s.approve(issued.UserCode)
		}
	}
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	w.Write(rec.Body.Bytes())
}

// approve is the server-to-server call Elk's edge function will make: the
// user code the person read off their terminal, the identity the provider
// verified, and ARK_IDP_KEY.
func (s *stubApprover) approve(userCode string) {
	s.approved = append(s.approved, userCode)
	req := s.req
	req.UserCode = userCode
	body, err := json.Marshal(req)
	if err != nil {
		s.t.Fatal(err)
	}
	call := httptest.NewRequest(http.MethodPost, "/v1/device/approve", bytes.NewReader(body))
	call.Header.Set("Authorization", "Bearer "+s.key)
	call.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.inner.ServeHTTP(rec, call)
	if rec.Code != 200 {
		s.t.Errorf("stub approver refused: %d %s", rec.Code, rec.Body.String())
	}
}

// startDeviceService boots the real service with an identity provider
// configured, behind the stub approver.
func startDeviceService(t *testing.T, assertion api.DeviceApproveRequest) (string, *stubApprover) {
	t.Helper()
	s := &server.Server{
		Repos:          repodb.NewManager(&repodb.LocalBackend{Dir: t.TempDir()}, t.TempDir()),
		Token:          servertest.Token,
		IDPApprovalURL: testApprovalURL,
		IDPKey:         testIDPKey,
		Blobs:          &server.LocalBlobStore{Dir: t.TempDir()},
	}
	approver := &stubApprover{t: t, inner: s.Handler(), key: testIDPKey, req: assertion}
	ts := httptest.NewServer(approver)
	t.Cleanup(ts.Close)
	return ts.URL, approver
}

// arkStdin runs the CLI with something piped in, which is the path a script
// uses and the one that must not change.
func arkStdin(t *testing.T, dir, stdin string, args ...string) (string, error) {
	t.Helper()
	root := New("test")
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"-C", dir}, args...))
	err := root.Execute()
	return buf.String(), err
}

// arkStreams runs the CLI with stdout and stderr kept apart, which is the only
// way to assert that --json leaves one JSON document on stdout.
func arkStreams(t *testing.T, dir, stdin string, args ...string) (string, string, error) {
	t.Helper()
	root := New("test")
	var out, errs bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errs)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"-C", dir}, args...))
	err := root.Execute()
	return out.String(), errs.String(), err
}

// userCodePattern is the shape a person reads off the screen.
var userCodePattern = regexp.MustCompile(`\b[0-9BCDFGHJKMNPQRSTVWXYZ]{4}-[0-9BCDFGHJKMNPQRSTVWXYZ]{4}\b`)

func storedToken(t *testing.T, home, host string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".ark", "credentials.toml"))
	if err != nil {
		t.Fatalf("no credentials file after a login: %v", err)
	}
	if !strings.Contains(string(data), host) {
		t.Fatalf("the credentials file holds nothing for %s:\n%s", host, data)
	}
	m := regexp.MustCompile(`arkc_[A-Za-z0-9_-]+`).FindString(string(data))
	if m == "" {
		t.Fatalf("no Ark credential in the credentials file:\n%s", data)
	}
	return m
}

// The acceptance case: no arguments, against a service that offers a device
// login. Ark prints a code and a URL, polls, and ends up holding a credential
// that works — with nothing typed back into the terminal.
func TestLoginWithNoArgumentsRunsTheDeviceFlow(t *testing.T) {
	url, approver := startDeviceService(t, api.DeviceApproveRequest{
		Subject:     "idp-subject-1",
		Email:       "me@example.com",
		DisplayName: "Me",
	})
	dir := gitRepo(t)
	home := logoutHome(t)

	out, err := arkStdin(t, dir, "", "login", "--remote", url)
	if err != nil {
		t.Fatalf("ark login: %v\n%s", err, out)
	}

	code := userCodePattern.FindString(out)
	if code == "" {
		t.Fatalf("no XXXX-XXXX code in the output:\n%s", out)
	}
	if len(approver.approved) != 1 || approver.approved[0] != code {
		t.Errorf("the code approved was %v, the code printed was %q", approver.approved, code)
	}
	if !strings.Contains(out, testApprovalURL) {
		t.Errorf("the output does not say where to go:\n%s", out)
	}
	if !strings.Contains(out, "Logged in as Me <me@example.com>") {
		t.Errorf("the output does not say who you logged in as:\n%s", out)
	}
	if !strings.Contains(out, "Token stored in") {
		t.Errorf("the output does not say where the credential went:\n%s", out)
	}

	// The credential is stored, and it is a credential this service accepts:
	// storing one it would refuse is the failure `ark login` verifies against
	// on the paste path, and the device path must not be the way in for it.
	token := storedToken(t, home, strings.TrimPrefix(url, "http://"))
	if out := ark(t, dir, "login", "--remote", url, "--token", token); !strings.Contains(out, "Token stored in") {
		t.Errorf("the credential the flow stored was not accepted by the service: %s", out)
	}
}

// --json is a stable interface for agents, so the flow must not write its
// pairing instructions into it — and a person running with --json still has to
// be able to read the code.
func TestDeviceLoginKeepsJSONOnStdoutAndTheCodeOnStderr(t *testing.T) {
	url, _ := startDeviceService(t, api.DeviceApproveRequest{
		Email: "me@example.com", RepositoryIDs: []string{"01SEEDEDREPO00000000000000"},
	})
	dir := gitRepo(t)
	logoutHome(t)

	stdout, stderr, err := arkStreams(t, dir, "", "--json", "login", "--remote", url)
	if err != nil {
		t.Fatalf("ark login --json: %v\n%s\n%s", err, stdout, stderr)
	}
	var got loginReport
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if got.Method != loginMethodDevice {
		t.Errorf("method = %q, want %q", got.Method, loginMethodDevice)
	}
	if got.Principal == nil || got.Principal.Email != "me@example.com" {
		t.Errorf("principal = %+v", got.Principal)
	}
	if got.Storage == "" || got.StoredIn == "" || got.Host == "" {
		t.Errorf("the original keys did not survive: %+v", got)
	}
	if userCodePattern.FindString(stderr) == "" {
		t.Errorf("the code is nowhere a person can read it:\n%s", stderr)
	}
	if userCodePattern.FindString(stdout) != "" {
		t.Errorf("the pairing instructions leaked into the JSON:\n%s", stdout)
	}
}

// A service with no identity provider must say so. The failure this guards
// against is the one that costs the most: a login that prints a code nobody
// can approve and then waits fifteen minutes for it.
func TestLoginWithNoArgumentsSaysSoWhenThereIsNoDeviceFlow(t *testing.T) {
	url := startBootstrapServer(t) // no ARK_IDP_APPROVAL_URL
	dir := gitRepo(t)
	logoutHome(t)

	out, err := arkStdin(t, dir, "", "login", "--remote", url)
	if err == nil {
		t.Fatalf("ark login reported success against a service with no device flow:\n%s", out)
	}
	if code := records.ExitCode(err); code != 2 {
		t.Errorf("exit code = %d, want 2 (validation): %v", code, err)
	}
	if !strings.Contains(err.Error(), "--token") {
		t.Errorf("the error %q does not say how to log in instead", err)
	}
	if !strings.Contains(err.Error(), "ark principal create") {
		t.Errorf("the error %q does not say where a token comes from", err)
	}
	if userCodePattern.FindString(out) != "" {
		t.Errorf("a code was printed for a service that cannot approve one:\n%s", out)
	}
}

// The client gives up at the TTL with a message that says what to do, not with
// a bare timeout. The stub never approves anything, which is the case a person
// hits when they close the browser tab.
func TestLoginGivesUpWhenTheCodeExpires(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.ServiceBanner{
			Service: "ark-sync", API: "v1",
			Auth: &api.ServiceAuth{DeviceFlow: true, ApprovalURL: testApprovalURL},
		})
	})
	mux.HandleFunc("POST /v1/device/code", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(api.DeviceCodeResponse{
			DeviceCode: "device-code", UserCode: "BCDF-GHJK",
			VerificationURI: testApprovalURL,
			// One second to live and five between polls: the client must give
			// up on this side of the deadline rather than sleep past it.
			ExpiresIn: 1, Interval: 5,
		})
	})
	mux.HandleFunc("POST /v1/device/token", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPreconditionRequired)
		json.NewEncoder(w).Encode(api.Error{Code: api.DeviceCodePending, Message: "not approved yet"})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	dir := gitRepo(t)
	home := logoutHome(t)
	out, err := arkStdin(t, dir, "", "login", "--remote", ts.URL)
	if err == nil {
		t.Fatalf("ark login reported success without an approval:\n%s", out)
	}
	if code := records.ExitCode(err); code != 5 {
		t.Errorf("exit code = %d, want 5 (permission): %v", code, err)
	}
	if !strings.Contains(err.Error(), "BCDF-GHJK") {
		t.Errorf("the error %q does not name the code that expired", err)
	}
	if !strings.Contains(err.Error(), "ark login") {
		t.Errorf("the error %q does not say to start over", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, ".ark", "credentials.toml")); statErr == nil {
		t.Error("a login that never completed stored something")
	}
}

// The paste path is unchanged, and this is the acceptance criterion that says
// so: a token on stdin is verified against the live service and stored, with
// no device flow anywhere near it.
func TestLoginStillReadsATokenFromStdin(t *testing.T) {
	url := startBootstrapServer(t)
	dir := gitRepo(t)
	home := logoutHome(t)

	out, err := arkStdin(t, dir, servertest.Token+"\n", "login", "--remote", url)
	if err != nil {
		t.Fatalf("ark login with a piped token: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Token stored in") {
		t.Errorf("output = %q", out)
	}
	if userCodePattern.FindString(out) != "" {
		t.Errorf("a piped token started a device flow:\n%s", out)
	}
	data, err := os.ReadFile(filepath.Join(home, ".ark", "credentials.toml"))
	if err != nil || !strings.Contains(string(data), servertest.Token) {
		t.Fatalf("the piped token was not stored: %v\n%s", err, data)
	}
}

// --token, --no-verify and --remote keep the shape they had, including the
// --json keys an agent reads.
func TestLoginWithATokenIsUnchanged(t *testing.T) {
	dir := gitRepo(t)
	home := logoutHome(t)

	var got loginReport
	arkJSON(t, dir, &got, "login", "--no-verify", "--remote", "https://ark-token-path.invalid", "--token", "tok-pasted")
	if got.Storage != "file" || got.Host != "ark-token-path.invalid" || got.Remote != "https://ark-token-path.invalid" {
		t.Errorf("report = %+v", got)
	}
	if got.Method != loginMethodToken {
		t.Errorf("method = %q, want %q", got.Method, loginMethodToken)
	}
	if got.Principal != nil {
		t.Errorf("a pasted token resolved a principal from nowhere: %+v", got.Principal)
	}
	data, err := os.ReadFile(filepath.Join(home, ".ark", "credentials.toml"))
	if err != nil || !strings.Contains(string(data), "tok-pasted") {
		t.Fatalf("the pasted token was not stored: %v\n%s", err, data)
	}
}

// A terminal is not a token source. Before the device flow, `ark login` with
// no arguments blocked on a read here; now that no arguments means "log in
// through a browser", reading would be indistinguishable from a hang.
func TestStdinIsNotReadWhenItIsATerminal(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Skipf("no %s on this platform: %v", os.DevNull, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		t.Skipf("%s does not stat as a character device here", os.DevNull)
	}

	cmd := &cobra.Command{}
	cmd.SetIn(f)
	if got := stdinToken(cmd); got != "" {
		t.Errorf("stdinToken read %q from a character device", got)
	}

	// And a pipe still is read, which is every scripted login.
	cmd.SetIn(strings.NewReader("  tok-piped \n"))
	if got := stdinToken(cmd); got != "tok-piped" {
		t.Errorf("stdinToken(pipe) = %q, want the trimmed token", got)
	}
}

// The device flow is per service, exactly as the paste path is: --remote lets
// it run from outside a repository, and the credential covers every repository
// pointing at that host.
func TestDeviceLoginWorksOutsideARepository(t *testing.T) {
	url, _ := startDeviceService(t, api.DeviceApproveRequest{Email: "me@example.com"})
	dir := t.TempDir() // not a Git repository, let alone an Ark one
	logoutHome(t)

	out, err := arkStdin(t, dir, "", "login", "--remote", url)
	if err != nil {
		t.Fatalf("ark login --remote outside a repository: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Logged in as me@example.com") {
		t.Errorf("output = %q", out)
	}
	if !strings.Contains(out, fmt.Sprintf("Covers every repository on this machine whose remote is %s",
		strings.TrimPrefix(url, "http://"))) {
		t.Errorf("the output does not state the credential's scope:\n%s", out)
	}
}
