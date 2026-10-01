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
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestRecordingGrantIsScopedAndBoundedByOccurrenceExpiry(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewRecordingSigner(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), "key-1", "hhc-media-test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 27, 13, 0, 0, 0, time.UTC)
	grant, err := signer.Issue("rec-1", "file-1", "scope-1", "recordings/file-1.mp4", now.Add(30*time.Minute), now)
	if err != nil {
		t.Fatal(err)
	}
	if !grant.ExpiresAt.Equal(now.Add(30 * time.Minute)) {
		t.Fatalf("expiry = %v", grant.ExpiresAt)
	}
	var exchange, playback map[string]any
	verifyJWT(t, grant.ExchangeCredential, &key.PublicKey, &exchange)
	verifyJWT(t, exchange["playback"].(string), &key.PublicKey, &playback)
	if exchange["typ"] != "exchange" || playback["typ"] != "playback" || playback["objectKey"] != "recordings/file-1.mp4" || playback["scopeId"] != "scope-1" {
		t.Fatalf("unexpected signed grants: exchange=%v playback=%v", exchange, playback)
	}
	if int64(exchange["exp"].(float64)) != now.Add(time.Minute).Unix() || int64(playback["exp"].(float64)) != now.Add(30*time.Minute).Unix() {
		t.Fatalf("unexpected grant expiries: exchange=%v playback=%v", exchange["exp"], playback["exp"])
	}
	if _, err := signer.Issue("rec-1", "file-1", "scope-1", "recordings/file-1.mp4", now, now); err == nil {
		t.Fatal("expired occurrence must not produce a grant")
	}
}

func verifyJWT(t *testing.T, token string, key *ecdsa.PublicKey, claims *map[string]any) {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal("token is not a compact JWS")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != 64 {
		t.Fatal("invalid ES256 signature")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(key, sum[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		t.Fatal("invalid JWS signature")
	}
	encoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, claims); err != nil {
		t.Fatal(err)
	}
}

func TestPackageGrantScopesImmutablePrefixAndClampsRetention(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewRecordingSigner(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), "key-1", "hhc-media-test")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ready, expiry := now.Add(-time.Hour), now.Add(20*time.Minute)
	p := RecordingPackage{ID: "package-a", RecordingID: "rec-a", OwnerService: "hhc-web-api", State: "ready", ReadyAt: &ready, MediaExpiresAt: &expiry, FinalPrefix: "recordings/packages/package-a/final/attempt-a/"}
	grant, err := signer.IssuePackage(p, "scope-a", now.Add(3*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	var exchange, playback map[string]any
	verifyJWT(t, grant.ExchangeCredential, &key.PublicKey, &exchange)
	verifyJWT(t, exchange["playback"].(string), &key.PublicKey, &playback)
	if playback["packageId"] != p.ID || playback["prefix"] != p.FinalPrefix || playback["objectKey"] != nil || playback["assetVersionId"] != nil || !grant.ExpiresAt.Equal(expiry) {
		t.Fatalf("claims: %v", playback)
	}
	p.FinalPrefix = "recordings/packages/other/final/attempt-a/"
	if _, err := signer.IssuePackage(p, "scope-a", expiry, now); err == nil {
		t.Fatal("cross-package prefix accepted")
	}
	p.FinalPrefix = "recordings/packages/package-a/staging/"
	if _, err := signer.IssuePackage(p, "scope-a", expiry, now); err == nil {
		t.Fatal("mutable prefix accepted")
	}
}
