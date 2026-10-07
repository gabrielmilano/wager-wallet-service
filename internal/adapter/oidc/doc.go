// Package oidc valida os tokens JWT emitidos pelo IdP (assinatura via JWKS,
// iss, aud e exp) e extrai o Principal: o claim provider_id e as roles
// (ADR 0003).
//
// Implementação na Fase 07.
package oidc
