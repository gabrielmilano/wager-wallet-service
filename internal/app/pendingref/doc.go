// Package pendingref implementa o worker que retoma operações em
// PENDING_REFERENCE com backoff exponencial até resolver a referência ou
// rejeitá-la por expiração.
//
// Implementação na Fase 10.
package pendingref
