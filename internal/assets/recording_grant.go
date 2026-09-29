package assets

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"regexp"
	"strings"
	"time"
)

var mediaID = regexp.MustCompile(`^[a-zA-Z0-9-]{1,80}$`)

type RecordingSigner struct {
	key    *ecdsa.PrivateKey
	kid    string
	issuer string
}

type RecordingGrant struct {
	ExchangeCredential string    `json:"exchangeCredential"`
	ExpiresAt          time.Time `json:"expiresAt"`
}

func NewRecordingSigner(privatePEM []byte, kid, issuer string) (*RecordingSigner, error) {
	block, _ := pem.Decode(privatePEM)
	if block == nil || !mediaID.MatchString(kid) || issuer == "" {
		return nil, ErrInvalidInput
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	key, ok := parsed.(*ecdsa.PrivateKey)
	if err != nil || !ok || key.Curve != elliptic.P256() {
		return nil, ErrInvalidInput
	}
	return &RecordingSigner{key: key, kid: kid, issuer: issuer}, nil
}

// Issue only signs a caller-validated, ready private recording asset. The
// owning service must recheck member entitlement and publication first.
func (s *RecordingSigner) Issue(recordingID, assetVersionID, scopeID, objectKey string, recordingExpiry, now time.Time) (RecordingGrant, error) {
	if !mediaID.MatchString(recordingID) || !mediaID.MatchString(assetVersionID) || !mediaID.MatchString(scopeID) || !strings.HasPrefix(objectKey, "recordings/") || strings.Contains(objectKey, "..") {
		return RecordingGrant{}, ErrInvalidInput
	}
	now = now.UTC()
	expiry := now.Add(time.Hour)
	if recordingExpiry.Before(expiry) {
		expiry = recordingExpiry.UTC()
	}
	if expiry.Unix() <= now.Unix() {
		return RecordingGrant{}, ErrForbidden
	}
	base := map[string]any{
		"iss": s.issuer, "aud": "hhc-media", "recordingId": recordingID,
		"assetVersionId": assetVersionID, "scopeId": scopeID, "nbf": now.Unix(),
	}
	playback := cloneClaims(base)
	playback["typ"] = "playback"
	playback["exp"] = expiry.Unix()
	playback["objectKey"] = objectKey
	playbackToken, err := s.sign(playback)
	if err != nil {
		return RecordingGrant{}, err
	}
	if len(playbackToken) > 3072 {
		return RecordingGrant{}, ErrInvalidInput
	}
	exchangeExpiry := now.Add(time.Minute)
	if expiry.Before(exchangeExpiry) {
		exchangeExpiry = expiry
	}
	exchange := cloneClaims(base)
	exchange["typ"] = "exchange"
	exchange["exp"] = exchangeExpiry.Unix()
	exchange["playback"] = playbackToken
	exchangeToken, err := s.sign(exchange)
	if err != nil {
		return RecordingGrant{}, err
	}
	if len(exchangeToken) > 8192 {
		return RecordingGrant{}, ErrInvalidInput
	}
	return RecordingGrant{ExchangeCredential: exchangeToken, ExpiresAt: expiry}, nil
}

func cloneClaims(source map[string]any) map[string]any {
	copy := make(map[string]any, len(source)+3)
	for key, value := range source {
		copy[key] = value
	}
	return copy
}

func (s *RecordingSigner) sign(claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "ES256", "kid": s.kid, "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	hash := sha256.Sum256([]byte(input))
	r, ss, err := ecdsa.Sign(rand.Reader, s.key, hash[:])
	if err != nil {
		return "", errors.New("sign recording grant")
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	ss.FillBytes(signature[32:])
	return input + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
