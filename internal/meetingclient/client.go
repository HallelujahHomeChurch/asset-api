package meetingclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Window struct {
	StartsAt time.Time `json:"startsAt"`
	EndsAt   time.Time `json:"endsAt"`
}

type Client struct {
	baseURL string
	http    *http.Client
	token   func(context.Context) (string, error)
}

func New(baseURL string, httpClient *http.Client, token func(context.Context) (string, error)) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: httpClient, token: token}
}

func (c *Client) ListSyncWindows(ctx context.Context, from, to time.Time) ([]Window, error) {
	query := url.Values{"from": {from.UTC().Format(time.RFC3339)}, "to": {to.UTC().Format(time.RFC3339)}}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/priv/meeting-sync-windows?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	token, err := c.token(ctx)
	if err != nil {
		return nil, fmt.Errorf("authorize meeting sync windows: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := c.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("list meeting sync windows: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list meeting sync windows: status %d", response.StatusCode)
	}
	var envelope struct {
		Data []Window `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode meeting sync windows: %w", err)
	}
	return envelope.Data, nil
}
