// Package wagerwallet é a raiz do módulo wager-wallet-service: um serviço que
// movimenta carteiras de jogadores a partir de operações de provedores de jogos,
// recebidas por HTTP e SQS, com ledger append-only, idempotência persistente e
// transactional outbox.
//
// Os binários ficam em cmd/ e o código da aplicação em internal/.
package wagerwallet
