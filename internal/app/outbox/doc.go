// Package outbox implementa a lógica do publisher da transactional outbox:
// reserva de eventos com lease, publicação, confirmação e backoff.
package outbox
