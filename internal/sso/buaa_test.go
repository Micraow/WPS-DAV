package sso

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"
)

// TestSignSSIDEncoding verifies the exact wire format the WPS login endpoint
// expects: base64url(compact JWK) + base64url(r||s) over SHA-256(ssid).
func TestSignSSIDEncoding(t *testing.T) {
	const ssid = "test-ssid-123"
	key, pubB64, sigB64, err := signSSID(ssid)
	if err != nil {
		t.Fatalf("signSSID: %v", err)
	}

	rawJWK, err := base64.RawURLEncoding.DecodeString(pubB64)
	if err != nil {
		t.Fatalf("public_key is not base64url: %v", err)
	}
	var jwk struct {
		Crv    string   `json:"crv"`
		Ext    bool     `json:"ext"`
		KeyOps []string `json:"key_ops"`
		Kty    string   `json:"kty"`
		X      string   `json:"x"`
		Y      string   `json:"y"`
	}
	if err := json.Unmarshal(rawJWK, &jwk); err != nil {
		t.Fatalf("public_key is not a JWK: %v", err)
	}
	if jwk.Crv != "P-256" || jwk.Kty != "EC" || !jwk.Ext || len(jwk.KeyOps) != 1 || jwk.KeyOps[0] != "verify" {
		t.Fatalf("unexpected JWK header: %+v", jwk)
	}
	x, err := base64.RawURLEncoding.DecodeString(jwk.X)
	if err != nil {
		t.Fatalf("jwk.x: %v", err)
	}
	y, err := base64.RawURLEncoding.DecodeString(jwk.Y)
	if err != nil {
		t.Fatalf("jwk.y: %v", err)
	}
	if new(big.Int).SetBytes(x).Cmp(key.PublicKey.X) != 0 || new(big.Int).SetBytes(y).Cmp(key.PublicKey.Y) != 0 {
		t.Fatal("JWK coordinates do not match the signing key")
	}

	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil {
		t.Fatalf("ssid_sign is not base64url: %v", err)
	}
	if len(sig) != 64 {
		t.Fatalf("ssid_sign must be r||s (64 bytes), got %d", len(sig))
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	hash := sha256.Sum256([]byte(ssid))
	if !ecdsa.Verify(&key.PublicKey, hash[:], r, s) {
		t.Fatal("signature does not verify against the advertised key")
	}
}

// TestLiveLoginPage checks (network) that the entry URL still leads to the
// CAS form and that `execution` can be extracted. Run with WPSDAV_LIVE_SSO=1.
func TestLiveLoginPage(t *testing.T) {
	if os.Getenv("WPSDAV_LIVE_SSO") == "" {
		t.Skip("set WPSDAV_LIVE_SSO=1 to run the live check")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := New("", "")
	resp, finalURL, err := c.follow(ctx, c.entryURL)
	if err != nil {
		t.Fatalf("follow: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		t.Fatalf("read login page: %v", err)
	}
	if !strings.Contains(string(body), `id="loginForm"`) {
		t.Fatalf("not the CAS login page; stopped at %s (http %d)", finalURL, resp.StatusCode)
	}
	m := executionRe.FindSubmatch(body)
	if m == nil {
		t.Fatal("execution field not found; CAS page structure changed")
	}
	t.Logf("reached %s; execution=%s...", finalURL, string(m[1])[:min(8, len(m[1]))])
}
