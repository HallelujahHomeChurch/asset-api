package httpapi

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/config"

	"github.com/golang-jwt/jwt/v5"
)

const (
	workloadHeader   = "X-HHC-Workload-Authorization"
	workloadKeyTTL   = 5 * time.Minute
	workloadRetry    = 30 * time.Second
	workloadJWKSSize = 256 * 1024
)

var errWorkloadKeysUnavailable = errors.New("workload verification keys unavailable")

type workloadPrincipalKey struct{}

type workloadVerifier struct {
	config config.WorkloadAuth
	client *http.Client
	now    func() time.Time

	refresh chan struct{}
	mu      sync.RWMutex
	keys    map[string]*rsa.PublicKey
	expires time.Time
	retryAt time.Time
	failed  bool
}

func (h *Handler) WithWorkloadAuth(settings *config.WorkloadAuth) *Handler {
	if settings != nil {
		h.workloadAuth = newWorkloadVerifier(*settings, nil, time.Now)
	}
	return h
}

func newWorkloadVerifier(settings config.WorkloadAuth, transport http.RoundTripper, now func() time.Time) *workloadVerifier {
	return &workloadVerifier{
		config: settings, now: now, refresh: make(chan struct{}, 1),
		client: &http.Client{
			Transport: transport,
			Timeout:   2 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (h *Handler) authenticateWorkload(r *http.Request, legacy func(*http.Request) string) (string, string, error) {
	if workloadHeaderPresent(r) {
		return h.verifyWorkload(r)
	}
	caller := legacy(r)
	if caller == "" || !h.legacyWorkloadAllowed(caller) {
		return "", "", assets.ErrForbidden
	}
	return "", caller, nil
}

func workloadHeaderPresent(r *http.Request) bool {
	_, present := r.Header[http.CanonicalHeaderKey(workloadHeader)]
	return present
}

func (h *Handler) verifyWorkload(r *http.Request) (principal, caller string, err error) {
	values := r.Header.Values(workloadHeader)
	if h.workloadAuth == nil || len(values) != 1 ||
		len(r.Header.Values("Dapr-Caller-App-Id")) != 0 || len(r.Header.Values("Dapr-Api-Token")) != 0 ||
		len(r.Header.Values("X-Internal-Caller-App-Id")) != 0 {
		return "", "", assets.ErrUnauthorized
	}
	raw, ok := strings.CutPrefix(values[0], "Bearer ")
	if !ok || len(raw) == 0 || len(raw) > 8192 || strings.ContainsAny(raw, " \t\r\n,") {
		return "", "", assets.ErrUnauthorized
	}
	principal, caller, err = h.workloadAuth.verify(r.Context(), raw)
	if err != nil {
		return "", "", err
	}
	if aliases, present := r.Header[http.CanonicalHeaderKey("X-HHC-Caller-App-Id")]; present &&
		(len(aliases) != 1 || aliases[0] != caller) {
		return "", "", assets.ErrUnauthorized
	}
	return principal, caller, nil
}

func (h *Handler) uploadProfilesAllowed(r *http.Request, input assets.CreateUploadInput) bool {
	principal, _ := r.Context().Value(workloadPrincipalKey{}).(string)
	if principal == "" || h.workloadAuth == nil {
		return false
	}
	for _, scope := range h.workloadAuth.config.UploadProfileScopes[principal] {
		if scope.Allows(input.Namespace, input.UploadProfiles) {
			return true
		}
	}
	return false
}

func (h *Handler) legacyWorkloadAllowed(caller string) bool {
	return h.workloadAuth == nil || !slices.Contains(h.workloadAuth.config.DisableLegacyCallers, caller)
}

func writeWorkloadAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errWorkloadKeysUnavailable):
		writeError(w, http.StatusServiceUnavailable, "AST_UNAVAILABLE", "workload authentication is unavailable")
	case errors.Is(err, assets.ErrForbidden):
		writeError(w, http.StatusForbidden, "AST_FORBIDDEN", "caller is not allowed")
	default:
		handleError(w, err)
	}
}

func (v *workloadVerifier) verify(ctx context.Context, raw string) (string, string, error) {
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		kid, _ := token.Header["kid"].(string)
		if kid == "" || len(kid) > 128 {
			return nil, assets.ErrUnauthorized
		}
		audience, err := claims.GetAudience()
		notBefore, nbfErr := claims.GetNotBefore()
		_, delegated := claims["scp"]
		_, hasIDType := claims["idtyp"]
		idType, _ := claims["idtyp"].(string)
		if err != nil || len(audience) != 1 || nbfErr != nil || notBefore == nil || delegated ||
			(hasIDType && idType != "app") {
			return nil, assets.ErrUnauthorized
		}
		return v.key(ctx, kid)
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(v.config.Issuer), jwt.WithAudience(v.config.Audience),
		jwt.WithExpirationRequired(), jwt.WithTimeFunc(v.now), jwt.WithStrictDecoding())
	if errors.Is(err, errWorkloadKeysUnavailable) {
		return "", "", errWorkloadKeysUnavailable
	}
	if err != nil {
		return "", "", assets.ErrUnauthorized
	}
	principal, _ := claims[v.config.PrincipalClaim].(string)
	caller := v.config.Principals[principal]
	if caller == "" {
		return "", "", assets.ErrForbidden
	}
	return principal, caller, nil
}

func (v *workloadVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	if key, err, cached := v.cachedKey(kid); cached {
		return key, err
	}
	select {
	case v.refresh <- struct{}{}:
	case <-ctx.Done():
		return nil, errWorkloadKeysUnavailable
	}
	defer func() { <-v.refresh }()
	if key, err, cached := v.cachedKey(kid); cached {
		return key, err
	}

	if ctx.Err() != nil {
		return nil, errWorkloadKeysUnavailable
	}
	// An abandoned client request should not suppress the next refresh attempt.
	// The shared fetch still has the HTTP client's two-second timeout.
	keys, err := v.fetchKeys(context.WithoutCancel(ctx))
	now := v.now()
	v.mu.Lock()
	v.retryAt = now.Add(workloadRetry)
	v.failed = err != nil
	if err == nil {
		v.keys, v.expires = keys, now.Add(workloadKeyTTL)
	}
	v.mu.Unlock()
	if err != nil {
		return nil, errWorkloadKeysUnavailable
	}
	if key := keys[kid]; key != nil {
		return key, nil
	}
	return nil, assets.ErrUnauthorized
}

func (v *workloadVerifier) cachedKey(kid string) (*rsa.PublicKey, error, bool) {
	now := v.now()
	v.mu.RLock()
	defer v.mu.RUnlock()
	if key := v.keys[kid]; key != nil && now.Before(v.expires) {
		return key, nil, true
	}
	if now.Before(v.retryAt) {
		if v.failed {
			return nil, errWorkloadKeysUnavailable, true
		}
		return nil, assets.ErrUnauthorized, true
	}
	return nil, nil, false
}

func (v *workloadVerifier) fetchKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, v.config.JWKSURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := v.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errWorkloadKeysUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, workloadJWKSSize+1))
	if err != nil || len(body) > workloadJWKSSize {
		return nil, errWorkloadKeysUnavailable
	}
	var payload struct {
		Keys []struct {
			ID        string   `json:"kid"`
			Type      string   `json:"kty"`
			Algorithm string   `json:"alg"`
			Use       string   `json:"use"`
			Ops       []string `json:"key_ops"`
			N         string   `json:"n"`
			E         string   `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Keys) > 128 {
		return nil, errWorkloadKeysUnavailable
	}
	keys := make(map[string]*rsa.PublicKey, len(payload.Keys))
	for _, jwk := range payload.Keys {
		if jwk.Type != "RSA" || (jwk.Algorithm != "" && jwk.Algorithm != "RS256") ||
			(jwk.Use != "" && jwk.Use != "sig") || (len(jwk.Ops) != 0 && !slices.Contains(jwk.Ops, "verify")) {
			continue
		}
		modulus, nErr := base64.RawURLEncoding.Strict().DecodeString(jwk.N)
		exponent, eErr := base64.RawURLEncoding.Strict().DecodeString(jwk.E)
		if jwk.ID == "" || len(jwk.ID) > 128 || keys[jwk.ID] != nil ||
			nErr != nil || len(modulus) < 256 || len(modulus) > 1024 || eErr != nil || len(exponent) == 0 || len(exponent) > 4 {
			return nil, errWorkloadKeysUnavailable
		}
		n := new(big.Int).SetBytes(modulus)
		var eBytes [4]byte
		copy(eBytes[4-len(exponent):], exponent)
		e := binary.BigEndian.Uint32(eBytes[:])
		if n.BitLen() < 2048 || n.Bit(0) == 0 || e < 3 || e%2 == 0 || e > 1<<31-1 {
			return nil, errWorkloadKeysUnavailable
		}
		keys[jwk.ID] = &rsa.PublicKey{N: n, E: int(e)}
	}
	if len(keys) == 0 {
		return nil, errWorkloadKeysUnavailable
	}
	return keys, nil
}
