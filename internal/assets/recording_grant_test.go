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
	if exchange["exp"] != float64(now.Add(time.Minute).Unix()) {
		t.Fatal("exchange must expire after one minute")
	}
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
