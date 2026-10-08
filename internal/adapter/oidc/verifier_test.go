package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	issuer   = "http://localhost:8180/realms/wager"
	audience = "wager-wallet-service"
)

// idp simula o Keycloak: chave RSA e JWKS servido por HTTP.
type idp struct {
	key    *rsa.PrivateKey
	kid    string
	server *httptest.Server
}

func newIDP(t *testing.T) *idp {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &idp{key: key, kid: "kid-1"}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.key.PublicKey, KeyID: p.kid, Algorithm: "RS256", Use: "sig"}}}
		_ = json.NewEncoder(w).Encode(set)
	}))
	t.Cleanup(p.server.Close)
	return p
}

func (p *idp) sign(t *testing.T, key *rsa.PrivateKey, alg jose.SignatureAlgorithm, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", p.kid))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func validClaims() map[string]any {
	return map[string]any{
		"iss":          issuer,
		"aud":          []string{audience, "account"},
		"exp":          time.Now().Add(5 * time.Minute).Unix(),
		"iat":          time.Now().Unix(),
		"sub":          "service-account-provider-a",
		"azp":          "provider-a",
		"provider_id":  "provider-a",
		"realm_access": map[string]any{"roles": []string{"provider", "offline_access"}},
	}
}

func TestVerifyValidToken(t *testing.T) {
	p := newIDP(t)
	v := NewVerifier(issuer, p.server.URL, audience)

	principal, err := v.Verify(context.Background(), p.sign(t, p.key, jose.RS256, validClaims()))
	if err != nil {
		t.Fatal(err)
	}
	if principal.ProviderID != "provider-a" || principal.ClientID != "provider-a" ||
		!principal.HasRole(RoleProvider) || principal.HasRole(RoleWalletAdmin) {
		t.Errorf("principal = %+v", principal)
	}
}

func TestVerifyAcceptsAudienceAsString(t *testing.T) {
	p := newIDP(t)
	v := NewVerifier(issuer, p.server.URL, audience)
	c := validClaims()
	c["aud"] = audience
	if _, err := v.Verify(context.Background(), p.sign(t, p.key, jose.RS256, c)); err != nil {
		t.Errorf("aud como string: %v", err)
	}
}

func TestVerifyRejectsInvalidTokens(t *testing.T) {
	p := newIDP(t)
	v := NewVerifier(issuer, p.server.URL, audience)
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)

	tests := []struct {
		name  string
		token func() string
	}{
		{"ausente", func() string { return "" }},
		{"malformado", func() string { return "nao.e.jwt" }},
		{"expirado", func() string {
			c := validClaims()
			c["exp"] = time.Now().Add(-time.Minute).Unix()
			return p.sign(t, p.key, jose.RS256, c)
		}},
		{"outro issuer", func() string {
			c := validClaims()
			c["iss"] = "http://keycloak:8080/realms/wager"
			return p.sign(t, p.key, jose.RS256, c)
		}},
		{"issuer com barra final", func() string {
			c := validClaims()
			c["iss"] = issuer + "/"
			return p.sign(t, p.key, jose.RS256, c)
		}},
		{"outra audiência", func() string {
			c := validClaims()
			c["aud"] = "account"
			return p.sign(t, p.key, jose.RS256, c)
		}},
		{"assinado por outra chave", func() string { return p.sign(t, otherKey, jose.RS256, validClaims()) }},
		{"algoritmo não permitido", func() string { return p.sign(t, p.key, jose.PS256, validClaims()) }},
		{"payload adulterado", func() string {
			raw := p.sign(t, p.key, jose.RS256, validClaims())
			other := p.sign(t, p.key, jose.RS256, map[string]any{
				"iss": issuer, "aud": audience, "exp": time.Now().Add(time.Hour).Unix(), "provider_id": "provider-b",
			})
			// Cabeçalho e assinatura do primeiro, payload do segundo.
			return splitJWT(raw)[0] + "." + splitJWT(other)[1] + "." + splitJWT(raw)[2]
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := v.Verify(context.Background(), tt.token()); !errors.Is(err, ErrInvalidToken) {
				t.Errorf("erro = %v, want ErrInvalidToken", err)
			}
		})
	}
}

func splitJWT(raw string) []string { return strings.SplitN(raw, ".", 3) }
