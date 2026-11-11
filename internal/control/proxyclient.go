package control

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
	"time"

	"faultline/internal/fault"
)

// ErrLeaseLost means the proxy no longer holds our rule set: the lease
// expired or another rule set replaced it.
var ErrLeaseLost = errors.New("lease lost")

// ProxyClient talks to the proxy admin API.
type ProxyClient struct {
	base  string
	token string
	http  *http.Client
}

func NewProxyClient(base, token string) *ProxyClient {
	return &ProxyClient{base: base, token: token, http: &http.Client{Timeout: 5 * time.Second}}
}

func (c *ProxyClient) do(ctx context.Context, method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("proxy admin API unreachable: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("proxy admin API %s %s: status %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(data))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("proxy admin API %s %s: %w", method, path, err)
		}
	}
	return resp.StatusCode, nil
}

func (c *ProxyClient) Install(ctx context.Context, rs fault.RuleSet) error {
	_, err := c.do(ctx, http.MethodPut, "/admin/v1/faults", rs, nil)
	return err
}

func (c *ProxyClient) Clear(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodDelete, "/admin/v1/faults", nil, nil)
	return err
}

func (c *ProxyClient) Renew(ctx context.Context, leaseID string) error {
	status, err := c.do(ctx, http.MethodPost, "/admin/v1/lease/renew", map[string]string{"lease_id": leaseID}, nil)
	if status == http.StatusNotFound || status == http.StatusConflict {
		return ErrLeaseLost
	}
	return err
}

func (c *ProxyClient) Active(ctx context.Context) (json.RawMessage, error) {
	var raw json.RawMessage
	_, err := c.do(ctx, http.MethodGet, "/admin/v1/faults/active", nil, &raw)
	return raw, err
}

func (c *ProxyClient) Records(ctx context.Context, runID string, since int64) (fault.RecordPage, error) {
	var page fault.RecordPage
	q := url.Values{"run_id": {runID}, "since": {strconv.FormatInt(since, 10)}}
	_, err := c.do(ctx, http.MethodGet, "/admin/v1/records?"+q.Encode(), nil, &page)
	return page, err
}

func (c *ProxyClient) Stats(ctx context.Context) (fault.Stats, error) {
	var s fault.Stats
	_, err := c.do(ctx, http.MethodGet, "/admin/v1/stats", nil, &s)
	return s, err
}
