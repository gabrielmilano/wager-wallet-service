// Package wagering implementa o processamento de operações de provedores
// (BET, WIN, LOSS, REFUND, ROLLBACK): validação do Command, hash canônico,
// idempotência, replay, reversões e referências pendentes.
//
// HTTP e SQS usam o mesmo núcleo: Process (HTTP) e ProcessFromMessage (SQS,
// com inbox na mesma transação SQL). Ver ADR 0006.
//
// Implementação na Fase 06.
package wagering
