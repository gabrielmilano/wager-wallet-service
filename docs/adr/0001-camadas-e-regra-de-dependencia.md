# ADR 0001 — Camadas e regra de dependência

- **Status:** aceito
- **Data:** 2026-10-07

## Contexto

O desafio exige que o domínio permaneça independente de Fx, HTTP, SQS e bibliotecas de
persistência, e que a mesma regra de negócio atenda duas entradas (HTTP e SQS) e dois
workers (outbox e referências pendentes). A organização dos pacotes fica a critério do
candidato.

## Decisão

Arquitetura hexagonal pragmática, em quatro grupos de pacotes sob `internal/`:

| Camada | Pacotes | Pode importar |
| --- | --- | --- |
| `domain` | `money`, `wallet`, `wager`, `event` | stdlib, UUID e o próprio `domain` |
| `app` | `store`, `wagering`, `wallets`, `outbox`, `pendingref` | `domain`, `platform` |
| `adapter` | `postgres`, `httpapi`, `sqs`, `oidc` | `app`, `domain`, `platform`, bibliotecas de infraestrutura |
| `platform` | `config`, `logging`, `metrics`, `health` | stdlib e bibliotecas de terceiros (sem Fx) |

`internal/bootstrap` (e `cmd/`) é o único lugar que importa Fx e liga tudo.

As interfaces (portas) são declaradas por quem as usa, na camada `app` (idioma Go:
"aceite interfaces, devolva structs"). Os adapters as implementam sem declarar isso
explicitamente, porque em Go a satisfação de interfaces é implícita.

A regra é verificada por `internal/archtest`, que falha o `go test` quando um pacote
importa algo proibido para a sua camada.

## Alternativas consideradas

- **Pacote por tipo técnico** (`models/`, `services/`, `repositories/`): comum, mas
  mistura domínio e infraestrutura e não impede `models` de importar `pgx`.
- **Pacote único `internal/wallet` com tudo:** simples no começo, mas a independência do
  domínio passaria a depender de disciplina, sem verificação automática.
- **Clean Architecture completa** (entidades, casos de uso, controllers, presenters,
  gateways): mais camadas e mapeamentos do que o problema pede.
- **Verificar a regra com `depguard`/`golangci-lint`:** funciona, mas adiciona uma
  ferramenta externa; um teste com `go/build` roda no `go test` que já é exigido.

## Consequências

- O domínio é testável com `go test` puro, sem banco nem Fx.
- Trocar pgx, roteador HTTP ou SDK da AWS afeta apenas `adapter` e `bootstrap`.
- Custo: há mapeamento entre linhas do banco e entidades de domínio nos repositórios, e
  entre DTOs HTTP/SQS e o `wagering.Command`.
- Toda nova dependência de infraestrutura precisa entrar na lista de `archtest`, senão a
  regra não a cobre.
