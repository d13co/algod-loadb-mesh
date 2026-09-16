// Package algodhttp implements ports.AlgodClient over algod's REST API.
package algodhttp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/d13co/algod-loadb-mesh/internal/ports"
)

// Client is a thin algod REST client for the control plane.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// Factory builds Clients sharing one transport.
type Factory struct {
	HTTP *http.Client
}

// NewAlgodClient implements ports.AlgodClientFactory.
func (f Factory) NewAlgodClient(baseURL, token string) ports.AlgodClient {
	return New(baseURL, token, f.HTTP)
}

// New builds a client. hc may be nil for a default with sane timeouts.
func New(baseURL, token string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Transport: &http.Transport{
			MaxIdleConnsPerHost: 4, IdleConnTimeout: 90 * time.Second,
			ResponseHeaderTimeout: 70 * time.Second, // above wait-for-block's server timeout
		}}
	}
	return &Client{base: strings.TrimSuffix(baseURL, "/"), token: token, http: hc}
}

func (c *Client) get(ctx context.Context, path string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("X-Algo-API-Token", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}

func (c *Client) getJSON(ctx context.Context, path string, v any) ([]byte, error) {
	code, body, err := c.get(ctx, path)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, fmt.Errorf("%s: status %d: %s", path, code, truncate(body))
	}
	if v != nil {
		if err := json.Unmarshal(body, v); err != nil {
			return nil, fmt.Errorf("%s: decode: %w", path, err)
		}
	}
	return body, nil
}

// Status implements ports.AlgodClient.
func (c *Client) Status(ctx context.Context) (ports.Status, error) {
	var st ports.Status
	body, err := c.getJSON(ctx, "/v2/status", &st)
	if err != nil {
		return st, err
	}
	st.Raw = body
	return st, nil
}

// WaitForBlockAfter implements ports.AlgodClient.
func (c *Client) WaitForBlockAfter(ctx context.Context, round uint64) (ports.Status, error) {
	var st ports.Status
	body, err := c.getJSON(ctx, fmt.Sprintf("/v2/status/wait-for-block-after/%d", round), &st)
	if err != nil {
		return st, err
	}
	st.Raw = body
	return st, nil
}

// HasBlock probes /v2/blocks/{r}/hash, which is tiny and fails exactly when
// the block is not in the ledger.
func (c *Client) HasBlock(ctx context.Context, round uint64) (bool, error) {
	code, body, err := c.get(ctx, fmt.Sprintf("/v2/blocks/%d/hash", round))
	if err != nil {
		return false, err
	}
	switch {
	case code == 200:
		return true, nil
	case code == 404, code == 500 && strings.Contains(string(body), "ledger"):
		return false, nil
	}
	return false, fmt.Errorf("blocks/%d/hash: status %d: %s", round, code, truncate(body))
}

// Versions implements ports.AlgodClient.
func (c *Client) Versions(ctx context.Context) (ports.Versions, error) {
	var raw struct {
		GenesisID string `json:"genesis_id"`
		Build     struct {
			Major       int    `json:"major"`
			Minor       int    `json:"minor"`
			BuildNumber int    `json:"build_number"`
			Channel     string `json:"channel"`
		} `json:"build"`
	}
	if _, err := c.getJSON(ctx, "/versions", &raw); err != nil {
		return ports.Versions{}, err
	}
	return ports.Versions{GenesisID: raw.GenesisID,
		Build: fmt.Sprintf("%d.%d.%d-%s", raw.Build.Major, raw.Build.Minor, raw.Build.BuildNumber, raw.Build.Channel)}, nil
}

// BoxNames implements ports.AlgodClient.
func (c *Client) BoxNames(ctx context.Context, appID uint64) ([][]byte, error) {
	var raw struct {
		Boxes []struct {
			Name string `json:"name"`
		} `json:"boxes"`
	}
	if _, err := c.getJSON(ctx, fmt.Sprintf("/v2/applications/%d/boxes", appID), &raw); err != nil {
		return nil, err
	}
	out := make([][]byte, 0, len(raw.Boxes))
	for _, b := range raw.Boxes {
		name, err := base64.StdEncoding.DecodeString(b.Name)
		if err != nil {
			return nil, fmt.Errorf("box name %q: %w", b.Name, err)
		}
		out = append(out, name)
	}
	return out, nil
}

// Box implements ports.AlgodClient.
func (c *Client) Box(ctx context.Context, appID uint64, name []byte) ([]byte, error) {
	q := url.Values{"name": {"b64:" + base64.StdEncoding.EncodeToString(name)}}
	code, body, err := c.get(ctx, fmt.Sprintf("/v2/applications/%d/box?%s", appID, q.Encode()))
	if err != nil {
		return nil, err
	}
	if code == 404 {
		return nil, ports.ErrNotFound
	}
	if code != 200 {
		return nil, fmt.Errorf("box: status %d: %s", code, truncate(body))
	}
	var raw struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(raw.Value)
}

func truncate(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}
