# ADR 0002 — Binário único com componentes ligáveis

- **Status:** aceito
- **Data:** 2026-10-07

## Contexto

O serviço tem quatro componentes de longa duração: servidor HTTP, consumidor SQS,
publisher da outbox e worker de referências pendentes. O desafio exige demonstrar as
garantias com pelo menos três processos independentes, cada um com suas próprias
conexões e memória.

## Decisão

Um único binário, `cmd/wallet-service`, que pode executar todos os componentes. Cada
componente tem uma variável de ambiente para ligá-lo ou desligá-lo (nomes provisórios,
definidos na Fase 02):

| Variável | Componente | Padrão |
| --- | --- | --- |
| `APP_ENABLE_HTTP` | servidor HTTP | `true` |
| `APP_ENABLE_SQS_CONSUMER` | consumidor de `wager-transactions.fifo` | `true` |
| `APP_ENABLE_OUTBOX_PUBLISHER` | publisher da outbox | `true` |
| `APP_ENABLE_PENDING_WORKER` | worker de referências pendentes | `true` |

Em `internal/bootstrap`, cada componente é um `fx.Module` incluído ou não conforme a
configuração. A configuração é validada na inicialização: pelo menos um componente
precisa estar ligado.

## Alternativas consideradas

- **Um binário por papel** (`api`, `consumer`, `worker`): separação explícita, mas três
  Dockerfiles ou alvos de build, três composições Fx e mais configuração no Compose,
  sem ganho de correção. A coordenação entre instâncias acontece no PostgreSQL, não no
  processo.
- **Binário único sem chaves:** mais simples, mas impede rodar, por exemplo, uma
  instância só de publisher nos testes de falha da Fase 12.

## Consequências

- As três instâncias do teste de concorrência são três réplicas da mesma imagem.
- Os testes da Fase 12 podem isolar papéis (ex.: dois publishers disputando a outbox)
  apenas mudando variáveis.
- Toda a correção entre instâncias continua dependendo do banco (locks por linha,
  `SKIP LOCKED`, constraints); nenhum componente pode supor ser o único em execução.
