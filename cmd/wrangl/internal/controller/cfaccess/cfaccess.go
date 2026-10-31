// Package cfaccess verifies Cloudflare Access JWTs (Cf-Access-Jwt-Assertion) so the
// controller does not trust the tunnel alone (spec §11).
package cfaccess

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Verifier checks Access JWTs for one application.
type Verifier struct {
	Team     string // <team>.cloudflareaccess.com
	AUD      string // the Access application's audience tag
	Owner    string // the only identity (email) allowed; service tokens are allowed by policy
	CertsURL string // default https://<team>.cloudflareaccess.com/cdn-cgi/access/certs
	HTTP     *http.Client
	Now      func() time.Time

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

func (v *Verifier) issuer() string { return "https://" + v.Team + ".cloudflareaccess.com" }

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func (v *Verifier) key(kid string) (*rsa.PublicKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if k, ok := v.keys[kid]; ok && time.Since(v.fetched) < time.Hour {
		return k, nil
	}
	if time.Since(v.fetched) < 10*time.Second && v.keys != nil { // don't hammer on unknown kids
		return nil, fmt.Errorf("unknown signing key %q", kid)
	}
	url := v.CertsURL
	if url == "" {
		url = v.issuer() + "/cdn-cgi/access/certs"
	}
	hc := v.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch Access certs: %w", err)
	}
	defer resp.Body.Close()
	var jwks struct {
		Keys []struct {
			Kid, Kty, N, E string
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return nil, fmt.Errorf("Access certs: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" {
			continue
		}
		n, err1 := base64.RawURLEncoding.DecodeString(k.N)
		e, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	v.keys, v.fetched = keys, time.Now()
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown signing key %q", kid)
}

// Identity is who a verified token belongs to.
type Identity struct {
	Email        string // users
	ServiceToken string // service tokens (common_name = client id)
}

// Verify checks a token and returns its identity.
func (v *Verifier) Verify(token string) (Identity, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Identity{}, errors.New("malformed token")
	}
	hb, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	cb, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	sig, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return Identity{}, errors.New("malformed token")
	}
	var h struct{ Alg, Kid string }
	if json.Unmarshal(hb, &h) != nil || h.Alg != "RS256" {
		return Identity{}, errors.New("token must be RS256")
	}
	k, err := v.key(h.Kid)
	if err != nil {
		return Identity{}, err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(k, crypto.SHA256, sum[:], sig) != nil {
		return Identity{}, errors.New("bad signature")
	}
	var c struct {
		Aud        json.RawMessage `json:"aud"`
		Exp        int64           `json:"exp"`
		Nbf        int64           `json:"nbf"`
		Iss        string          `json:"iss"`
		Email      string          `json:"email"`
		CommonName string          `json:"common_name"`
	}
	if err := json.Unmarshal(cb, &c); err != nil {
		return Identity{}, errors.New("malformed claims")
	}
	var auds []string
	if json.Unmarshal(c.Aud, &auds) != nil {
		var one string
		json.Unmarshal(c.Aud, &one)
		auds = []string{one}
	}
	okAud := false
	for _, a := range auds {
		okAud = okAud || a == v.AUD
	}
	now := v.now().Unix()
	switch {
	case !okAud:
		return Identity{}, errors.New("token is for another application")
	case c.Iss != v.issuer():
		return Identity{}, errors.New("token from another team")
	case c.Exp <= now:
		return Identity{}, errors.New("token expired")
	case c.Nbf > now+60:
		return Identity{}, errors.New("token not yet valid")
	}
	if c.Email == "" && c.CommonName != "" {
		return Identity{ServiceToken: c.CommonName}, nil // allowed by an Access service-auth policy
	}
	if v.Owner != "" && !strings.EqualFold(c.Email, v.Owner) {
		return Identity{}, fmt.Errorf("%s is not the owner", c.Email)
	}
	return Identity{Email: c.Email}, nil
}

// Middleware rejects requests without a valid Access token, except those skip allows.
func (v *Verifier) Middleware(next http.Handler, skip func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skip != nil && skip(r) {
			next.ServeHTTP(w, r)
			return
		}
		tok := r.Header.Get("Cf-Access-Jwt-Assertion")
		if tok == "" {
			if c, err := r.Cookie("CF_Authorization"); err == nil {
				tok = c.Value
			}
		}
		if tok == "" {
			http.Error(w, "Cloudflare Access token required", http.StatusUnauthorized)
			return
		}
		if _, err := v.Verify(tok); err != nil {
			http.Error(w, "Cloudflare Access: "+err.Error(), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
