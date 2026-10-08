package oidc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
)

// Roles de realm (ADR 0003), lidas de realm_access.roles.
const (
	RoleProvider    = "provider"
	RoleWalletAdmin = "wallet-admin"
)

// ErrInvalidToken: token ausente, malformado, com assinatura inválida,
// emitido por outro issuer, para outra audiência ou expirado (HTTP 401).
var ErrInvalidToken = errors.New("oidc: token inválido")

// Principal é a identidade autenticada.
type Principal struct {
	Subject    string
	ClientID   string // azp
	ProviderID string // claim provider_id ("" para o serviço interno)
	Roles      []string
}

// HasRole informa se a identidade tem a role de realm.
func (p Principal) HasRole(role string) bool { return slices.Contains(p.Roles, role) }

// Verifier valida tokens localmente, sem discovery (ADR 0009): chaves de
// jwksURL (cacheadas e recarregadas quando aparece um kid novo), iss
// comparado como texto exato, aud contendo audience, exp obrigatório e só
// RS256.
type Verifier struct {
	verifier *gooidc.IDTokenVerifier
}

// NewVerifier não faz rede: o JWKS é buscado na primeira validação.
func NewVerifier(issuer, jwksURL, audience string) *Verifier {
	client := &http.Client{Timeout: 5 * time.Second}
	ctx := gooidc.ClientContext(context.Background(), client)
	keys := gooidc.NewRemoteKeySet(ctx, jwksURL)
	return &Verifier{verifier: gooidc.NewVerifier(issuer, keys, &gooidc.Config{
		ClientID:             audience,
		SupportedSigningAlgs: []string{gooidc.RS256},
	})}
}

// claims são os campos do access token do Keycloak usados aqui.
type claims struct {
	Subject     string `json:"sub"`
	AZP         string `json:"azp"`
	ProviderID  string `json:"provider_id"`
	RealmAccess struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

// Verify valida o token e devolve a identidade.
func (v *Verifier) Verify(ctx context.Context, raw string) (Principal, error) {
	if raw == "" {
		return Principal{}, fmt.Errorf("%w: ausente", ErrInvalidToken)
	}
	token, err := v.verifier.Verify(ctx, raw)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	var c claims
	if err := token.Claims(&c); err != nil {
		return Principal{}, fmt.Errorf("%w: claims: %v", ErrInvalidToken, err)
	}
	return Principal{
		Subject:    c.Subject,
		ClientID:   c.AZP,
		ProviderID: c.ProviderID,
		Roles:      c.RealmAccess.Roles,
	}, nil
}
