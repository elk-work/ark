package cli

import (
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/pkg/api"
)

// The CLI half of elk-work/ark#94: the two verbs that make a credential id
// recoverable and a credential retirable, end to end against a real service.

// mintPrincipal runs the bootstrap route through the CLI and returns what it
// printed. The first one on a service with no operator is the operator.
func mintPrincipal(t *testing.T, dir, url, email string, args ...string) principalResult {
	t.Helper()
	var got principalResult
	arkJSON(t, dir, &got, append([]string{"principal", "create", "--remote", url,
		"--email", email, "--bootstrap", testBootstrap}, args...)...)
	if got.Token == "" {
		t.Fatalf("no credential minted for %s: %+v", email, got)
	}
	return got
}

// The path the issue said was missing: a credential id nobody wrote down,
// found again and then used to retire the credential.
func TestCredentialListThenRevoke(t *testing.T) {
	url := startBootstrapServer(t)
	dir := gitRepo(t)

	operator := mintPrincipal(t, dir, url, "operator@example.com")
	member := mintPrincipal(t, dir, url, "member@example.com")
	// A second credential for the same person — the laptop, in the case this
	// is for. Reissuing for a known email is what `create` does.
	laptop := mintPrincipal(t, dir, url, "member@example.com")

	t.Setenv("ARK_TOKEN", member.Token)
	var mine api.CredentialListResponse
	arkJSON(t, dir, &mine, "credential", "list", "--remote", url)
	if len(mine.Credentials) != 2 {
		t.Fatalf("own credentials = %+v, want 2", mine.Credentials)
	}
	ids := map[string]bool{}
	for _, c := range mine.Credentials {
		ids[c.ID] = true
	}
	if !ids[member.CredentialID] || !ids[laptop.CredentialID] {
		t.Fatalf("the list does not carry the ids that were minted: %+v", mine.Credentials)
	}

	// Retire the lost one, and keep working with the other.
	out := ark(t, dir, "credential", "revoke", laptop.CredentialID, "--remote", url)
	if !strings.Contains(out, "revoked") {
		t.Errorf("revoke did not say what it did:\n%s", out)
	}
	if !strings.Contains(out, "60 seconds") {
		// The bound is the useful half — without it a still-served request
		// looks like the revocation having failed.
		t.Errorf("revoke did not state the propagation bound:\n%s", out)
	}

	// The human table has to carry the state, or the list is unreadable at
	// exactly the moment somebody needs it.
	human := ark(t, dir, "credential", "list", "--remote", url)
	if !strings.Contains(human, laptop.CredentialID) || !strings.Contains(human, "revoked") {
		t.Errorf("the list does not show the revoked credential:\n%s", human)
	}

	// And the roster, which is the operator's view of the same thing.
	t.Setenv("ARK_TOKEN", operator.Token)
	roster := ark(t, dir, "principal", "list", "--remote", url)
	for _, want := range []string{"operator@example.com", "member@example.com",
		"operator since", laptop.CredentialID} {
		if !strings.Contains(roster, want) {
			t.Errorf("the roster does not carry %q:\n%s", want, roster)
		}
	}
}

// Refusals have to land on the right exit code and leave the reader somewhere
// to go — an agent scripting this sees only the code (spec §22).
func TestPrincipalListIsOperatorOnly(t *testing.T) {
	url := startBootstrapServer(t)
	dir := gitRepo(t)

	mintPrincipal(t, dir, url, "operator@example.com")
	member := mintPrincipal(t, dir, url, "member@example.com")

	t.Setenv("ARK_TOKEN", member.Token)
	_, err := arkErr(t, dir, "principal", "list", "--remote", url)
	if err == nil {
		t.Fatal("a non-operator listed every principal")
	}
	if code := records.ExitCode(err); code != 5 {
		t.Errorf("exit code = %d, want 5 (permission): %v", code, err)
	}
	if !strings.Contains(err.Error(), "operator") {
		t.Errorf("error %q does not name what is required", err)
	}

	// Revoking somebody else's is refused the same way it is on the wire: a
	// non-operator cannot tell "not yours" from "not there".
	_, err = arkErr(t, dir, "credential", "revoke", "01NOSUCHCREDENTIAL00000000", "--remote", url)
	if err == nil {
		t.Fatal("a non-operator revoked a credential that is not theirs")
	}
	if code := records.ExitCode(err); code != 3 {
		t.Errorf("exit code = %d, want 3 (not found): %v", code, err)
	}
}

// `--operator` is how the second operator comes into existence, and the only
// way: the bootstrap token seeds the first and then stops being an authority.
func TestPrincipalCreateOperator(t *testing.T) {
	url := startBootstrapServer(t)
	dir := gitRepo(t)

	first := mintPrincipal(t, dir, url, "operator@example.com")
	if first.Principal.OperatorSince == "" {
		t.Fatalf("the first principal is not the operator: %+v", first.Principal)
	}

	// The bootstrap token cannot appoint one.
	_, err := arkErr(t, dir, "principal", "create", "--remote", url,
		"--email", "nope@example.com", "--operator", "--bootstrap", testBootstrap)
	if err == nil {
		t.Fatal("the bootstrap token appointed an operator")
	}
	if code := records.ExitCode(err); code != 5 {
		t.Errorf("exit code = %d, want 5 (permission): %v", code, err)
	}

	// An operator authenticating as themselves can, with no bootstrap token
	// anywhere in the command.
	t.Setenv("ARK_TOKEN", first.Token)
	var second principalResult
	arkJSON(t, dir, &second, "principal", "create", "--remote", url,
		"--email", "second@example.com", "--operator")
	if second.Principal.OperatorSince == "" {
		t.Fatalf("--operator did not make one: %+v", second.Principal)
	}
	if second.Principal.Kind != "human" {
		// Authority is its own column precisely so promoting somebody does
		// not overwrite what is holding the credential.
		t.Errorf("promotion overwrote kind: %+v", second.Principal)
	}
}
