// Package outbox implementa a lógica do publisher da transactional outbox:
// reserva de eventos com lease, publicação, confirmação e backoff.
//
// Implementação na Fase 09.
package outbox
