package cfaccess

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func sign(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	enc := base64.RawURLEncoding
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid})
	c, _ := json.Marshal(claims)
	in := enc.EncodeToString(h) + "." + enc.EncodeToString(c)
	sum := sha256.Sum256([]byte(in))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	return in + "." + enc.EncodeToString(sig)
}

func TestVerifierAndMiddleware(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	certs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"keys":[{"kid":"k1","kty":"RSA","n":%q,"e":%q}]}`,
			base64.RawURLEncoding.EncodeToString(key.N.Bytes()), base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()))
	}))
	defer certs.Close()
	v := &Verifier{Team: "acme", AUD: "aud123", Owner: "Owner@Example.com", CertsURL: certs.URL}
	now := time.Now().Unix()
	base := func() map[string]any {
		return map[string]any{"aud": []string{"aud123"}, "iss": "https://acme.cloudflareaccess.com", "exp": now + 600, "email": "owner@example.com"}
	}
	if id, err := v.Verify(sign(t, key, "k1", base())); err != nil || id.Email != "owner@example.com" {
		t.Fatalf("owner: %+v %v", id, err)
	}
	svc := base()
	delete(svc, "email")
	svc["common_name"] = "abc.access"
	if id, err := v.Verify(sign(t, key, "k1", svc)); err != nil || id.ServiceToken != "abc.access" {
		t.Fatalf("service token: %+v %v", id, err)
	}
	bad := map[string]func(map[string]any){
		"other user": func(c map[string]any) { c["email"] = "mallory@example.com" },
		"wrong aud":  func(c map[string]any) { c["aud"] = "nope" },
		"wrong iss":  func(c map[string]any) { c["iss"] = "https://evil.cloudflareaccess.com" },
		"expired":    func(c map[string]any) { c["exp"] = now - 1 },
	}
	for name, mut := range bad {
		c := base()
		mut(c)
		if _, err := v.Verify(sign(t, key, "k1", c)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := v.Verify(sign(t, other, "k1", base())); err == nil {
		t.Error("foreign signature accepted")
	}

	h := v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }),
		func(r *http.Request) bool { return r.URL.Path == "/gh/webhook" })
	code := func(path, tok string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		if tok != "" {
			req.Header.Set("Cf-Access-Jwt-Assertion", tok)
		}
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if code("/api/v1/runners", "") != 401 || code("/api/v1/runners", "x.y.z") != 403 ||
		code("/api/v1/runners", sign(t, key, "k1", base())) != 204 || code("/gh/webhook", "") != 204 {
		t.Fatal("middleware decisions wrong")
	}
}
