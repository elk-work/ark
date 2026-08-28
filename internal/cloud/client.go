// Package cloud is the HTTP client for the Ark sync service.
package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/pkg/api"
)

// Client talks to one Ark remote.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New builds a client for the remote, resolving the token from the
// environment, the OS keychain, or the credentials file.
func New(remote string) (*Client, error) {
	token, err := ResolveToken(remote)
	if err != nil {
		return nil, err
	}
	return &Client{
		BaseURL: strings.TrimSuffix(remote, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}, nil
}

// VerifyToken reports whether the server accepts a token, without storing it.
//
// It probes an authenticated route with a repository id that cannot exist. A
// live token earns a not-found; a dead one earns a 401. That difference is the
// whole signal, and it is why `ark login` can tell you the credential is wrong
// while you are still holding it, rather than at the next sync.
func VerifyToken(ctx context.Context, remote, token string) error {
	c := &Client{
		BaseURL: strings.TrimSuffix(remote, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
	err := c.do(ctx, http.MethodPost, "/v1/sync/pull",
		api.PullRequest{RepositoryID: "00000000000000000000000000", AfterRevision: 0}, nil)
	if err == nil {
		return nil
	}
	var arkErr *records.Error
	if errors.As(err, &arkErr) {
		switch arkErr.Kind {
		case records.KindPermission:
			return records.Permissionf("the server rejected this token")
		case records.KindNotFound:
			return nil // authenticated; the throwaway repository simply is not there
		case records.KindOffline:
			return err
		}
	}
	// Anything else means we reached the server and it did not object to who we
	// are, which is all this check claims to establish.
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return records.Offlinef("cannot reach %s: %v", c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var apiErr api.Error
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if json.Unmarshal(data, &apiErr) != nil || apiErr.Message == "" {
			apiErr.Message = strings.TrimSpace(string(data))
		}
		return statusError(resp.StatusCode, apiErr)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func statusError(status int, e api.Error) error {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(status)
	}
	// The service's code outranks its status for the one condition the status
	// cannot express. A 500 normally means "the service is having a moment",
	// which is what offline (exit 6) tells a caller: wait and try again. A
	// repository whose stored database will not open is a 500 that is going to
	// keep being a 500 until a person restores it, and a scripted caller
	// treating exit 6 as retryable retries forever (elk-work/ark#65). Exit 8
	// says so instead; the two must not share a code, because 6 is the one
	// retry loops key on.
	if e.Code == api.ErrorCodeRepositoryCorrupt {
		return records.RemoteCorruptf("%s", msg)
	}
	switch status {
	case http.StatusUnauthorized:
		// 401 is a verdict on the bearer itself: the service does not
		// recognise it. The server's own wording is "invalid or missing
		// token", which cannot distinguish a stale credential from an absent
		// one — and the client always has one by this point, or it would not
		// have got here.
		return &records.Error{Kind: records.KindPermission,
			Message: msg + " — the stored credential was not accepted; it may have been rotated. Run `ark login`"}
	case http.StatusForbidden:
		// 403 is the other half of `permission`, and it says the opposite
		// about the credential. Both stay exit 5: the two mean the same thing
		// to a program — you may not do this, and retrying will not change it
		// — and only the sentence a person reads differs.
		return &records.Error{Kind: records.KindPermission, Message: forbidden(e.Message)}
	case http.StatusNotFound:
		return records.NotFoundf("%s", msg)
	case http.StatusConflict:
		return records.Conflictf("%s", msg)
	case http.StatusBadRequest:
		return records.Validationf("%s", msg)
	default:
		if status >= 500 {
			return records.Offlinef("server error: %s", msg)
		}
		return fmt.Errorf("%s", msg)
	}
}

// forbidden words a 403 for the person who hit one.
//
// Until elk-work/ark#52 the two `permission` statuses had one cause between
// them, so one sentence covered both. Grants are enforced now, and a 403 has
// a second meaning: the service recognised the credential and is refusing the
// request anyway, because the principal holds no grant — or too low a one —
// on this repository (spec §19.2). Telling that reader to run `ark login`
// sends them to re-authenticate a credential that is already working, which
// cannot help and leaves them believing the credential is the problem
// (elk-work/ark#95).
//
// **The status is the whole signal**, so no message text is guessed at. The
// service refuses an unrecognised bearer with 401 and an unauthorized
// principal with 403 — that is the HTTP distinction between "authenticate"
// and "authorization will not help", and `faultPermission` in
// internal/server/grants.go picks it deliberately.
//
// The refusal itself passes through unchanged, because the service is the
// only side that knows which authority was missing and it already names the
// principal, the repository, the level required, and the `ark repo grant`
// that issues one (`refusal`, same file). The client adds the one thing the
// service cannot know: that the credential in this machine's store is not
// what needs repairing.
func forbidden(msg string) string {
	if msg == "" {
		// Nothing named what was refused, so there is no refusal to pass
		// through — a 403 from something sitting in front of the service, or
		// from one too old to say. Inventing a diagnosis here would repeat
		// the mistake this function exists to fix, so it claims only what the
		// status itself establishes.
		return "the service refused this request without saying what authority was missing" +
			" — it refused a credential it accepted rather than rejecting one, so logging in" +
			" again will not help; ask an admin of this repository whether you hold a grant on it"
	}
	return msg + " — the stored credential was accepted, so logging in again will not help;" +
		" the grant is the missing part"
}

func (c *Client) RegisterRepo(ctx context.Context, req api.RegisterRepositoryRequest) error {
	return c.do(ctx, http.MethodPost, "/v1/repositories", req, nil)
}

func (c *Client) Push(ctx context.Context, req api.PushRequest) (*api.PushResponse, error) {
	var resp api.PushResponse
	if err := c.do(ctx, http.MethodPost, "/v1/sync/push", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) Pull(ctx context.Context, req api.PullRequest) (*api.PullResponse, error) {
	var resp api.PullResponse
	if err := c.do(ctx, http.MethodPost, "/v1/sync/pull", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) GetRecord(ctx context.Context, repoID, recordType, recordID string) (*api.Record, error) {
	var rec api.Record
	path := fmt.Sprintf("/v1/repositories/%s/records/%s/%s", repoID, recordType, recordID)
	if err := c.do(ctx, http.MethodGet, path, nil, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// Dangling lists the references the service accepted in this repository
// while holding nothing at the other end (spec §9.1).
//
// `all` asks for every entry ever recorded rather than only the ones that
// still resolve to nothing; `limit` bounds the listing, zero taking the
// service's default. The counts in the response describe the repository
// either way, so neither argument can make it look healthier than it is.
func (c *Client) Dangling(ctx context.Context, repoID string, all bool, limit int) (*api.DanglingResponse, error) {
	q := url.Values{}
	if all {
		q.Set("all", "true")
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/repositories/" + repoID + "/dangling"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var resp api.DanglingResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) Merge(ctx context.Context, prID string, req api.MergeRequest) (*api.MergeResponse, error) {
	var resp api.MergeResponse
	if err := c.do(ctx, http.MethodPost, "/v1/pull-requests/"+prID+"/merge", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) UploadURL(ctx context.Context, req api.UploadURLRequest) (*api.UploadURLResponse, error) {
	var resp api.UploadURLResponse
	if err := c.do(ctx, http.MethodPost, "/v1/artifacts/upload-url", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) ConfirmUpload(ctx context.Context, req api.UploadURLRequest) error {
	return c.do(ctx, http.MethodPost, "/v1/artifacts/confirm", req, nil)
}

func (c *Client) DownloadURL(ctx context.Context, req api.DownloadURLRequest) (*api.DownloadURLResponse, error) {
	var resp api.DownloadURLResponse
	if err := c.do(ctx, http.MethodPost, "/v1/artifacts/download-url", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// PutBlob uploads content to a signed URL.
func (c *Client) PutBlob(ctx context.Context, url, mediaType string, content io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, content)
	if err != nil {
		return err
	}
	req.ContentLength = size
	if mediaType != "" {
		req.Header.Set("Content-Type", mediaType)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return records.Offlinef("blob upload failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("blob upload rejected (%d): %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}

// GetBlob downloads content from a signed URL to w.
func (c *Client) GetBlob(ctx context.Context, url string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return records.Offlinef("blob download failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("blob download rejected (%d)", resp.StatusCode)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}
