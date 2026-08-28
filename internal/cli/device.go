package cli

// The client half of the device-authorization flow
// (docs/rfc-0003-elk-issued-credentials.md, Decision 3; docs/v1-spec.md
// §20.1). It is what `ark login` runs when it is given no token: ask the
// service for a code, print it, and poll until somebody approves it in a
// browser — which need not be a browser on this machine, and that is the whole
// reason the flow has this shape.
//
// None of it goes through internal/cloud's Client, deliberately, and for the
// same reason `ark principal create` does not: that client resolves a stored
// credential and maps every 401 to "run `ark login`", which is the command
// being run. These routes are unauthenticated because the caller has nothing
// to authenticate with yet.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/pkg/api"
)

// deviceHTTP bounds one request, not the flow. The flow's own bound is the
// code's expiry, which the service states and this client honours.
var deviceHTTP = &http.Client{Timeout: 30 * time.Second}

// deviceFallbackInterval is used when a service reports no polling interval.
// It matches the one RFC-0003 specifies, so a service that omits the field is
// polled at the rate every other service asks for rather than in a loop.
const deviceFallbackInterval = 5 * time.Second

// serviceBanner reads GET /, the unauthenticated banner. Its `auth` object is
// the whole of how this client discovers how to log in — there is no client
// configuration, and nothing here is written down on the machine.
func serviceBanner(ctx context.Context, remote string) (*api.ServiceBanner, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(remote, "/")+"/", nil)
	if err != nil {
		return nil, err
	}
	resp, err := deviceHTTP.Do(req)
	if err != nil {
		return nil, records.Offlinef("cannot reach %s: %v", remote, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, records.Offlinef("%s did not answer with a service banner (HTTP %d)",
			remote, resp.StatusCode)
	}
	var banner api.ServiceBanner
	if err := json.Unmarshal(data, &banner); err != nil {
		return nil, records.Offlinef("unreadable service banner from %s: %v", remote, err)
	}
	return &banner, nil
}

// devicePost calls one device route and reports the status alongside the
// service's error body, because on this flow the status *is* the answer:
// 428 pending, 429 slow_down, 410 expired are all ordinary states of a login
// in progress rather than failures to report.
func devicePost(ctx context.Context, remote, path string, in, out any) (int, api.Error, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return 0, api.Error{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimSuffix(remote, "/")+path, bytes.NewReader(body))
	if err != nil {
		return 0, api.Error{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := deviceHTTP.Do(req)
	if err != nil {
		return 0, api.Error{}, records.Offlinef("cannot reach %s: %v", remote, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var apiErr api.Error
		if json.Unmarshal(data, &apiErr) != nil || apiErr.Message == "" {
			apiErr.Message = strings.TrimSpace(string(data))
		}
		return resp.StatusCode, apiErr, nil
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, api.Error{},
				records.Offlinef("unreadable response from %s: %v", remote, err)
		}
	}
	return resp.StatusCode, api.Error{}, nil
}

// noDeviceFlow is the answer for a service that cannot log anybody in without
// a token. It is a validation error — exit 2 — because the invocation is what
// has to change: this is the same state as `ark login` with nothing to store,
// and it says where a token comes from rather than leaving the user to guess.
func noDeviceFlow(remote, why string) error {
	return records.Validationf(
		"%s %s, so `ark login` cannot log you in through a browser — pass --token or pipe a token on stdin. "+
			"`ark principal create --remote %s` mints one if the service has a bootstrap token; "+
			"otherwise ask whoever runs it for a credential", remote, why, remote)
}

// deviceLogin runs the flow and returns the credential the service minted.
//
// notify prints the code and the URL as the flow reaches them. It is separate
// from the command's result because a person has to act on it *during* the
// call, and in --json mode the result has not been written yet.
func deviceLogin(ctx context.Context, remote string, notify func(string, ...any)) (*api.DeviceTokenResponse, error) {
	banner, err := serviceBanner(ctx, remote)
	if err != nil {
		return nil, err
	}
	switch {
	case banner.Auth == nil:
		// No `auth` object at all: this service predates the device flow, so
		// its answer is not "no identity provider" but "no such question".
		return nil, noDeviceFlow(remote, "is older than the device login")
	case !banner.Auth.DeviceFlow:
		return nil, noDeviceFlow(remote, "has no identity provider configured")
	}

	var code api.DeviceCodeResponse
	status, apiErr, err := devicePost(ctx, remote, "/v1/device/code", struct{}{}, &code)
	if err != nil {
		return nil, err
	}
	if status >= 300 {
		// The banner said yes and the route says no, which is a service that
		// changed configuration between two requests. Say what happened
		// rather than reporting the banner's answer as the truth.
		return nil, records.Offlinef("%s would not issue a device code: %s", remote, apiErr.Message)
	}

	deadline := time.Now().Add(time.Duration(code.ExpiresIn) * time.Second)
	interval := time.Duration(code.Interval) * time.Second
	if interval <= 0 {
		interval = deviceFallbackInterval
	}

	notify("")
	notify("To finish logging in, open this URL in a browser — any browser, on any device:")
	notify("")
	notify("    %s", code.VerificationURI)
	notify("")
	notify("and enter this code:")
	notify("")
	notify("    %s", code.UserCode)
	notify("")
	notify("Waiting for approval. The code expires in %s.", expiryWords(code.ExpiresIn))

	for {
		var out api.DeviceTokenResponse
		status, apiErr, err := devicePost(ctx, remote, "/v1/device/token",
			api.DeviceTokenRequest{DeviceCode: code.DeviceCode}, &out)
		if err != nil {
			return nil, err
		}
		switch {
		case status < 300:
			return &out, nil
		case apiErr.Code == api.DeviceCodeExpired:
			return nil, deviceExpired(code.UserCode)
		case apiErr.Code == api.DeviceCodeSlowDown:
			// RFC 8628's rule: a slow_down means lengthen the interval, not
			// merely retry on the same one.
			interval += deviceFallbackInterval
		case apiErr.Code == api.DeviceCodePending:
			// The ordinary state of a login in progress.
		default:
			return nil, records.Offlinef("%s answered the login poll with: %s", remote, apiErr.Message)
		}

		// Give up on this side of the deadline rather than sleeping past it
		// and asking a question the service has already stopped answering.
		if time.Now().Add(interval).After(deadline) {
			return nil, deviceExpired(code.UserCode)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// deviceExpired is the give-up message, and it is deliberately not a bare
// timeout: nothing is wrong, nobody approved the code in time, and the repair
// is one command.
func deviceExpired(userCode string) error {
	return records.Permissionf(
		"the login code %s expired before it was approved — run `ark login` again to start over", userCode)
}

// expiryWords renders a code's lifetime the way the message reads it.
func expiryWords(seconds int) string {
	if seconds >= 120 {
		return fmt.Sprintf("%d minutes", seconds/60)
	}
	return fmt.Sprintf("%d seconds", seconds)
}

// deviceNotifier is where the pairing instructions go. In --json mode that is
// stderr: stdout carries one JSON document, and a person still has to read the
// code off the screen and type it into a browser.
func deviceNotifier(g *globals, cmd *cobra.Command) func(string, ...any) {
	if g.json {
		return func(format string, args ...any) {
			fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", args...)
		}
	}
	return g.printer(cmd).Line
}

// principalLabel names who you logged in as, preferring what a person would
// recognise.
func principalLabel(p *api.Principal) string {
	switch {
	case p.DisplayName != "" && p.Email != "":
		return fmt.Sprintf("%s <%s>", p.DisplayName, p.Email)
	case p.Email != "":
		return p.Email
	case p.DisplayName != "":
		return p.DisplayName
	default:
		return p.ID
	}
}
