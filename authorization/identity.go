package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func digest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type Identity struct {
	Name string `json:"principal_name"`
	Kind string `json:"principal_kind"`
	jwt.RegisteredClaims
}
type Verifier struct {
	Issuer, Audience, JWKS string
	mu                     sync.Mutex
	keys                   map[string]*rsa.PublicKey
	fetched, timeAttempt   time.Time
	client                 *http.Client
}

func (v *Verifier) key(token *jwt.Token) (any, error) {
	kid, _ := token.Header["kid"].(string)
	if kid == "" {
		return nil, fmt.Errorf("missing kid")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if key := v.keys[kid]; key != nil && time.Since(v.fetched) < time.Minute {
		return key, nil
	}
	if time.Since(v.timeAttempt) < time.Second {
		return nil, fmt.Errorf("JWKS refresh throttled")
	}
	v.timeAttempt = time.Now()
	client := v.client
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	res, err := client.Get(v.JWKS)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("JWKS unavailable")
	}
	var data struct {
		Keys []struct{ Kty, Kid, N, E, Use, Alg string }
	}
	if err = json.NewDecoder(http.MaxBytesReader(nil, res.Body, 1<<20)).Decode(&data); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range data.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		n, e1 := base64.RawURLEncoding.DecodeString(k.N)
		e, e2 := base64.RawURLEncoding.DecodeString(k.E)
		if e1 != nil || e2 != nil || len(e) > 4 || len(n) < 256 {
			continue
		}
		exponent := new(big.Int).SetBytes(e).Int64()
		if exponent < 3 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exponent)}
	}
	v.keys = keys
	v.fetched = time.Now()
	if keys[kid] == nil {
		return nil, fmt.Errorf("unknown signing key")
	}
	return keys[kid], nil
}
func (v *Verifier) Verify(header string) (Identity, error) {
	var claims Identity
	if !strings.HasPrefix(header, "Bearer ") {
		return claims, fmt.Errorf("missing bearer")
	}
	_, err := jwt.ParseWithClaims(strings.TrimPrefix(header, "Bearer "), &claims, v.key, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(v.Issuer), jwt.WithAudience(v.Audience), jwt.WithExpirationRequired())
	if err != nil {
		return claims, err
	}
	if claims.Subject == "" || claims.Name == "" || (claims.Kind != "agent" && claims.Kind != "service" && claims.Kind != "user") {
		return claims, fmt.Errorf("invalid principal claims")
	}
	return claims, nil
}
