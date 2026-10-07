# wager-wallet-service

Serviço em Go que movimenta carteiras de jogadores a partir de operações de provedores de
jogos (`BET`, `WIN`, `LOSS`, `REFUND`, `ROLLBACK`), recebidas por HTTP e por SQS, com
ledger append-only, idempotência persistente, transactional outbox e coordenação por
carteira entre múltiplas instâncias.

> Em construção. As seções abaixo serão preenchidas ao longo das fases.

## Pré-requisitos

- Go 1.27.x (com cgo/gcc para `go test -race`)
- Docker e Docker Compose
- `make`

## Variáveis de ambiente

Veja [.env.example](.env.example). Copie para `.env` e ajuste se necessário.

## Execução

_A definir (Fase 02): `docker compose up --build`._

## Migrations

_A definir: aplicação e reversão._

## Filas SQS

_A definir: provisionamento de `wager-transactions.fifo`, `wager-transactions-dlq.fifo` e
`wallet-events.fifo`._

## Autenticação

_A definir: provisionamento do Keycloak, identidades de teste e obtenção de tokens._

## Exemplos de chamadas

_A definir._

## Testes

```sh
make check        # gofmt, go vet e go test -race
go test ./...
go test -race ./...
go vet ./...
```

_Testes de integração, múltiplas instâncias e simulações de falha: a definir._

## Arquitetura

Decisões técnicas em `ARCHITECTURE.md` e nos ADRs em [docs/adr/](docs/adr/).

## Uso de IA

Este projeto foi desenvolvido com apoio do Claude Code (Anthropic) como assistente de
programação. O uso seguiu estas regras:

- Modelagem, decisões de arquitetura e planos de cada fase foram discutidos e aprovados
  pelo autor antes da implementação.
- Todo código gerado foi revisado pelo autor; os merges para `main` são feitos por ele.
- Commits com participação da IA trazem o trailer `Co-Authored-By`.
