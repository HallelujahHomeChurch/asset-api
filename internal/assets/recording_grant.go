package assets

import (
	"context"
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
var recordingFinalPrefix = regexp.MustCompile(`^recordings/packages/[a-zA-Z0-9-]{1,80}/final/[a-zA-Z0-9-]{1,80}/$`)

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

func (s *RecordingSigner) IssuePackage(p RecordingPackage, scopeID string, recordingExpiry, now time.Time) (RecordingGrant, error) {
	if !mediaID.MatchString(p.ID) || !mediaID.MatchString(p.RecordingID) || !mediaID.MatchString(scopeID) || p.State != "ready" || p.OwnerService != "hhc-web-api" || p.ReadyAt == nil || p.MediaExpiresAt == nil || !recordingFinalPrefix.MatchString(p.FinalPrefix) || strings.Split(p.FinalPrefix, "/")[2] != p.ID {
		return RecordingGrant{}, ErrInvalidInput
	}
	if p.MediaExpiresAt.Before(recordingExpiry) {
		recordingExpiry = *p.MediaExpiresAt
	}
	return s.issue(map[string]any{"recordingId": p.RecordingID, "packageId": p.ID, "scopeId": scopeID}, map[string]any{"prefix": p.FinalPrefix}, recordingExpiry, now)
}

func (s *RecordingSigner) issue(base, resource map[string]any, recordingExpiry, now time.Time) (RecordingGrant, error) {
	now = now.UTC()
	expiry := now.Add(time.Hour)
	if recordingExpiry.Before(expiry) {
		expiry = recordingExpiry.UTC()
	}
	if expiry.Unix() <= now.Unix() {
		return RecordingGrant{}, ErrForbidden
	}
	base["iss"], base["aud"], base["nbf"] = s.issuer, "hhc-media", now.Unix()
	playback := cloneClaims(base)
	playback["typ"] = "playback"
	playback["exp"] = expiry.Unix()
	for key, value := range resource {
		playback[key] = value
	}
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

// GrantLive runs signing and the cleanup fence in the same owner transaction.
// CMS owns membership and stopped-scope policy; Asset independently bounds bytes and time.
func (s *RecordingCaptureService) GrantLive(ctx context.Context, signer *RecordingSigner, id, recording, user, scope string, requested time.Time) (RecordingGrant, error) {
	var grant RecordingGrant
	if signer == nil {
		return grant, errors.New("live signer unavailable")
	}
	if !captureUUID.MatchString(recording) || !captureUUID.MatchString(user) || !captureUUID.MatchString(scope) || requested.IsZero() {
		return grant, ErrInvalidInput
	}
	_, err := s.repository.UpdateCapture(ctx, id, func(c *RecordingCapture) error {
		now := s.now().UTC()
		if c.RecordingID != recording {
			return ErrForbidden
		}
		if !now.Before(c.ExpiresAt) {
			return ErrCaptureExpired
		}
		if c.TerminalAt != nil || (c.State != "uploading" && c.State != "freezing" && c.State != "validating" && c.State != "ready") || c.Progress.LastSequence < 2 {
			return ErrConflict
		}
		expiry := now.Add(5 * time.Minute)
		for _, deadline := range []time.Time{requested, c.ExpiresAt} {
			if deadline.Before(expiry) {
				expiry = deadline
			}
		}
		if c.Progress.Ended {
			if c.Progress.EndedAt == nil {
				return ErrConflict
			}
			replay := c.Progress.EndedAt.Add(time.Duration(c.Progress.MediaEndSeconds*float64(time.Second)) + 30*time.Minute)
			if replay.Before(expiry) {
				expiry = replay
			}
		}
		if expiry.Unix() <= now.Unix() {
			return ErrCaptureExpired
		}
		var err error
		grant, err = signer.issue(map[string]any{"recordingId": recording, "captureId": c.ID, "scopeId": scope, "userId": user}, map[string]any{"prefix": "recordings/captures/" + c.ID + "/final/"}, expiry, now)
		if err != nil {
			return err
		}
		if c.ReadGrantUntil == nil || c.ReadGrantUntil.Before(grant.ExpiresAt) {
			c.ReadGrantUntil = &grant.ExpiresAt
		}
		return nil
	})
	return grant, err
}
func (s *RecordingCaptureService) LiveProgress(ctx context.Context, id, actor string) (RecordingLiveProgress, error) {
	c, err := s.repository.GetCapture(ctx, id)
	if err == nil {
		err = captureOwner(c, actor)
	}
	return c.Progress, err
}
