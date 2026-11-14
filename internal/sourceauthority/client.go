package sourceauthority

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/config"
)

var errUnavailable = errors.New("source authorization dependency unavailable")

type Client struct {
	http     *http.Client
	settings config.SourceAccess
}

func New(gateOrigin, gateToken, accountOrigin, accountToken string) (*Client, error) {
	settings := config.SourceAccess{GateOrigin: gateOrigin, GateToken: gateToken, AccountOrigin: accountOrigin, AccountToken: accountToken}
	if !settings.Valid() {
		return nil, errors.New("invalid source authorization configuration")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Client{settings: settings, http: &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) CheckSource(ctx context.Context, r assets.SourceAccessRequest) (assets.SourceDecision, error) {
	var result assets.SourceDecision
	err := c.post(ctx, c.settings.GateOrigin, "/priv/source/check", "X-HHC-Harness-Service-", c.settings.GateToken, r, &result)
	if err != nil || result.Version != 1 || result.PolicyRevision < 1 || result.RevocationRevision < 1 {
		return assets.SourceDecision{}, errUnavailable
	}
	switch result.State {
	case "active", "paused", "expired", "revoked":
		return result, nil
	default:
		return assets.SourceDecision{}, errUnavailable
	}
}
func (c *Client) CheckSources(ctx context.Context, requests []assets.SourceAccessRequest) ([]assets.SourceDecision, error) {
	if len(requests) == 0 || len(requests) > assets.SourceBatchLimit {
		return nil, errUnavailable
	}
	type entry struct {
		ID      string                     `json:"id"`
		Request assets.SourceAccessRequest `json:"request"`
	}
	input := struct {
		Version  int     `json:"version"`
		Requests []entry `json:"requests"`
	}{Version: 1}
	for i, r := range requests {
		input.Requests = append(input.Requests, entry{strconv.Itoa(i), r})
	}
	var result struct {
		Version   int `json:"version"`
		Decisions []struct {
			ID       string                `json:"id"`
			Decision assets.SourceDecision `json:"decision"`
		} `json:"decisions"`
	}
	if err := c.postBounded(ctx, c.settings.GateOrigin, "/priv/source/check-batch", "X-HHC-Harness-Service-", c.settings.GateToken, input, &result, 65536, 16384); err != nil || result.Version != 1 || len(result.Decisions) != len(requests) {
		return nil, errUnavailable
	}
	decisions := make([]assets.SourceDecision, len(requests))
	for i, v := range result.Decisions {
		d := v.Decision
		if v.ID != strconv.Itoa(i) || d.Version != 1 || d.PolicyRevision < 1 || d.RevocationRevision < 1 {
			return nil, errUnavailable
		}
		switch d.State {
		case "active", "paused", "expired", "revoked":
		default:
			return nil, errUnavailable
		}
		decisions[i] = d
	}
	return decisions, nil
}

func (c *Client) CurrentSubject(ctx context.Context, id string) (assets.CollectionSubject, error) {
	var result struct {
		Version int      `json:"version"`
		Active  *bool    `json:"active"`
		UserID  string   `json:"userId"`
		RoleIDs []string `json:"roleIds"`
	}
	err := c.post(ctx, c.settings.AccountOrigin, "/priv/account/v1/current-subject", "X-HHC-Account-Service-", c.settings.AccountToken, struct {
		UserID string `json:"userId"`
	}{id}, &result)
	if err != nil || result.Version != 1 || result.Active == nil || result.UserID != id || result.RoleIDs == nil || len(result.RoleIDs) > 128 {
		return assets.CollectionSubject{}, errUnavailable
	}
	if !*result.Active {
		return assets.CollectionSubject{}, assets.ErrForbidden
	}
	for _, role := range result.RoleIDs {
		if role == "" || len(role) > 128 || strings.ContainsFunc(role, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
			return assets.CollectionSubject{}, errUnavailable
		}
	}
	return assets.CollectionSubject{UserID: result.UserID, RoleIDs: result.RoleIDs}, nil
}
func (c *Client) post(ctx context.Context, origin, path, header, token string, input, output any) error {
	return c.postBounded(ctx, origin, path, header, token, input, output, 8192, 8192)
}
func (c *Client) postBounded(ctx context.Context, origin, path, header, token string, input, output any, requestLimit, responseLimit int) error {
	body, err := json.Marshal(input)
	if err != nil || len(body) > requestLimit {
		return errUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(origin, "/")+path, bytes.NewReader(body))
	if err != nil {
		return errUnavailable
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(header+"Caller", "asset-api")
	request.Header.Set(header+"Token", token)
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return errUnavailable
	}
	request.Header.Set("X-HHC-Request-ID", hex.EncodeToString(id[:]))
	response, err := c.http.Do(request)
	if err != nil {
		return errUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > int64(responseLimit) {
		return errUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, int64(responseLimit+1)))
	if err != nil || len(raw) > responseLimit {
		return errUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return errUnavailable
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errUnavailable
	}
	return nil
}
