// Package cloud is the HTTP client for the Ark sync service.
package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ijroth/ark/internal/records"
	"github.com/ijroth/ark/pkg/api"
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
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return &records.Error{Kind: records.KindPermission, Message: msg}
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
