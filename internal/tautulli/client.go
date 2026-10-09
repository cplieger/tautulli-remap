// Package tautulli provides a client for the Tautulli API v2.
package tautulli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/cplieger/httpx/v5"
	"github.com/cplieger/tautulli-remap/internal/remap"
)

// Response body size limit.
const maxTautulliBody = 30 << 20

// resultSuccess is the expected value of Response.Result on success.
const resultSuccess = "success"

// defaultRetryDelayUnit is the base delay multiplied by attempt number.
const defaultRetryDelayUnit = 5 * time.Second

// ClientTimeout is the total budget for one Tautulli HTTP request including
// retries; main.go installs it on the http.Client it injects. It must
// accommodate APIWithRetry's whole backoff schedule — a lower client timeout
// cuts the retry chain short before the attempt cap applies.
const ClientTimeout = 2 * time.Minute

// Client is the concrete Tautulli API client.
type Client struct {
	httpClient *http.Client
	url        string
	apiKey     string

	// RetryDelayUnit is the base delay multiplied by attempt number. Zero uses the default.
	RetryDelayUnit time.Duration
}

// historyResponse represents the JSON envelope for get_history.
type historyResponse struct {
	Response struct {
		Result  string `json:"result"`
		Message string `json:"message"`
		Data    struct {
			Data            []remap.HistoryItem `json:"data"`
			RecordsFiltered int                 `json:"recordsFiltered"`
		} `json:"data"`
	} `json:"response"`
}

// resultEnvelope represents a generic Tautulli API result envelope.
type resultEnvelope struct {
	Response struct {
		Result  string `json:"result"`
		Message string `json:"message"`
	} `json:"response"`
}

// HistoryPage holds a parsed page of history data.
type HistoryPage struct {
	Rows            []remap.HistoryItem
	RecordsFiltered int
}

// New creates a new Tautulli client.
func New(tautulliURL, apiKey string, httpClient *http.Client) *Client {
	return &Client{
		httpClient: httpClient,
		url:        tautulliURL,
		apiKey:     apiKey,
	}
}

// requestURL builds the Tautulli API v2 URL for cmd with the api key and any
// extra query params. The api key rides in the query string: httpx redacts it
// from APIWithRetry's logs and errors, and httpx.RedactSecret strips it from
// bare-API transport errors. cmd and apikey are applied after extra is
// merged, so a caller-supplied extra can never override them.
//
// extra is deep-copied via Values.Clone (nil for a nil extra) so no value
// slice is shared with the caller.
func (c *Client) requestURL(cmd string, extra url.Values) string {
	params := extra.Clone()
	if params == nil {
		params = url.Values{}
	}
	params.Set("cmd", cmd)
	params.Set("apikey", c.apiKey)
	return c.url + "/api/v2?" + params.Encode()
}

// call executes a single Tautulli API v2 GET command and returns the raw
// response body. The API key is embedded in the query string and redacted
// from any transport error. Callers that need retry logic should use
// APIWithRetry instead.
func (c *Client) call(ctx context.Context, cmd string, extra url.Values) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.requestURL(cmd, extra), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("tautulli %s: %w", cmd, httpx.RedactSecret(err, c.apiKey))
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tautulli %s: %w", cmd, httpx.RedactSecret(err, c.apiKey))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		httpx.DrainClose(resp.Body)
		return nil, fmt.Errorf("tautulli %s: HTTP %d", cmd, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTautulliBody))
	if err != nil {
		return nil, fmt.Errorf("tautulli %s: %w", cmd, httpx.RedactSecret(err, c.apiKey))
	}
	return body, nil
}

// APIWithRetry performs a GET against the Tautulli API with bounded
// exponential-backoff retry on 429/5xx (honoring Retry-After) via
// github.com/cplieger/httpx. 4xx-other-than-429 and non-transient transport
// errors are returned immediately. The api key (carried in the query string)
// is redacted from any returned error. Used for read commands; mutating
// commands use call directly so they are never retried.
func (c *Client) APIWithRetry(ctx context.Context, cmd string, extra url.Values) ([]byte, error) {
	body, err := httpx.GetBytes(ctx, c.httpClient, c.requestURL(cmd, extra),
		httpx.WithMaxAttempts(3),
		httpx.WithBaseDelay(c.retryDelayUnit()),
		httpx.WithMaxBodyBytes(maxTautulliBody),
	)
	if err != nil {
		return nil, fmt.Errorf("tautulli %s: %w", cmd, err)
	}
	return body, nil
}

// GetHistory fetches one page of Tautulli watch history using the supplied
// query parameters (start, length, media_type, etc.) and returns a parsed
// HistoryPage. It retries on transient errors via APIWithRetry.
func (c *Client) GetHistory(ctx context.Context, params url.Values) (*HistoryPage, error) {
	body, err := c.APIWithRetry(ctx, "get_history", params)
	if err != nil {
		return nil, err
	}
	var resp historyResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing history: %w", err)
	}
	if resp.Response.Result != resultSuccess {
		return nil, fmt.Errorf("get_history: result=%q message=%q", resp.Response.Result, resp.Response.Message)
	}
	return &HistoryPage{
		Rows:            resp.Response.Data.Data,
		RecordsFiltered: resp.Response.Data.RecordsFiltered,
	}, nil
}

// checkResult unmarshals a generic Tautulli result envelope and returns an
// error if the API reported anything other than success.
func checkResult(body []byte, cmd string) error {
	var resp resultEnvelope
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parsing %s: %w", cmd, err)
	}
	if resp.Response.Result != resultSuccess {
		return fmt.Errorf("%s: result=%q message=%q", cmd, resp.Response.Result, resp.Response.Message)
	}
	return nil
}

// UpdateMetadata updates the Tautulli history records for a renamed Plex item,
// replacing all occurrences of oldKey with newKey for the given media type.
func (c *Client) UpdateMetadata(ctx context.Context, oldKey, newKey string, mediaType remap.MediaType) error {
	params := url.Values{
		"old_rating_key": {oldKey},
		"new_rating_key": {newKey},
		"media_type":     {string(mediaType)},
	}
	body, err := c.call(ctx, "update_metadata_details", params)
	if err != nil {
		return err
	}
	return checkResult(body, "update_metadata_details")
}

// DeleteRecentlyAdded clears all entries from Tautulli's recently-added
// table. Deliberately table-wide: the API's delete_recently_added has no
// per-item variant, so clearing everything is the only way to evict the
// stale entries a remap leaves behind. Tautulli repopulates it from Plex
// activity.
func (c *Client) DeleteRecentlyAdded(ctx context.Context) error {
	body, err := c.call(ctx, "delete_recently_added", nil)
	if err != nil {
		return err
	}
	return checkResult(body, "delete_recently_added")
}

// retryDelayUnit returns the configured retry delay unit or the default.
func (c *Client) retryDelayUnit() time.Duration {
	if c.RetryDelayUnit > 0 {
		return c.RetryDelayUnit
	}
	return defaultRetryDelayUnit
}
