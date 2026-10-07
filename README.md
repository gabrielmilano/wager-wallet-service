# wager-wallet-service

Serviço em Go que movimenta carteiras de jogadores a partir de operações de provedores de
jogos (`BET`, `WIN`, `LOSS`, `REFUND`, `ROLLBACK`), recebidas por HTTP e por SQS, com
ledger append-only, idempotência persistente, transactional outbox e coordenação por
carteira entre múltiplas instâncias.

> Em construção. As seções abaixo serão preenchidas ao longo das fases.

## Pré-requisitos

- Go 1.27.x (com cgo/gcc para `go test -race`)
- Docker e Docker Compose v2
- `make`
- Cerca de 1,7 GB de memória livre para os containers (limites: Keycloak 768 MB,
  LocalStack 512 MB, PostgreSQL 256 MB, app 128 MB)
- Portas livres: `5432` (PostgreSQL), `4566` (LocalStack), `8180` (Keycloak), `8081` (app)

## Variáveis de ambiente

Veja [.env.example](.env.example). O `.env` é **opcional**: o `docker-compose.yml` e os
testes de integração usam padrões iguais aos do exemplo. Copie para `.env` para rodar a
aplicação fora do Docker ou para trocar senhas.

## Execução

```sh
make up      # docker compose up -d --build --wait (sobe tudo e espera os healthchecks)
make ps      # estado dos serviços
make logs    # logs de todos os serviços
make down    # derruba, preservando os volumes
make clean   # derruba e apaga os volumes
```

Equivalente sem `make`: `docker compose up --build`.

| Serviço | Endereço no host | Observação |
| --- | --- | --- |
| app | <http://localhost:8081> | `GET /health/live` |
| PostgreSQL | `localhost:5432` | banco `wager_wallet`; roles `app_migrator` e `app_runtime` |
| LocalStack (SQS) | <http://localhost:4566> | versão 4.14.0, sem auth token ([ADR 0008](docs/adr/0008-versao-do-localstack.md)) |
| Keycloak | <http://localhost:8180> | console de administração: `admin` / `admin` |

> **Atenção:** o script que cria o banco e os roles
> ([deploy/postgres/initdb/01-roles.sh](deploy/postgres/initdb/01-roles.sh)) só roda
> quando o volume do PostgreSQL está **vazio**. Se mudar senhas ou o script, rode
> `make clean` antes de `make up`.

Para rodar a aplicação fora do Docker (com as dependências do Compose no ar):

```sh
cp .env.example .env
set -a; . ./.env; set +a
docker compose stop app
go run ./cmd/wallet-service
```

## Migrations

_A definir: aplicação e reversão._

## Filas SQS

As filas são criadas automaticamente quando o LocalStack fica pronto, pelo script
[deploy/localstack/init/ready.d/10-queues.sh](deploy/localstack/init/ready.d/10-queues.sh):

| Fila | Uso | Configuração |
| --- | --- | --- |
| `wager-transactions.fifo` | entrada de operações | visibility timeout 30 s; redrive para a DLQ após 5 recebimentos |
| `wager-transactions-dlq.fifo` | DLQ da entrada | retenção de 14 dias |
| `wallet-events.fifo` | eventos publicados pela outbox | — |

Todas são FIFO, sem deduplicação por conteúdo (quem envia informa o
`MessageDeduplicationId`). Os valores de visibility timeout e tentativas são revistos na
Fase 08. Para listar as filas:

```sh
docker compose exec localstack awslocal sqs list-queues
```

As políticas IAM que controlariam o acesso na AWS real estão em
[deploy/aws/iam-policies.md](deploy/aws/iam-policies.md); o LocalStack Community não
aplica IAM.

## Autenticação

O realm `wager` é importado automaticamente de
[deploy/keycloak/realm-wager.json](deploy/keycloak/realm-wager.json). Clients de teste
(fluxo `client_credentials`; os secrets são apenas locais):

| Client | Secret | Role | Claim `provider_id` |
| --- | --- | --- | --- |
| `provider-a` | `provider-a-secret` | `provider` | `provider-a` |
| `provider-b` | `provider-b-secret` | `provider` | `provider-b` |
| `wallet-internal` | `wallet-internal-secret` | `wallet-admin` | — |

Todos os tokens trazem `aud = wager-wallet-service` e
`iss = http://localhost:8180/realms/wager`, e valem 5 minutos. Para obter um token:

```sh
TOKEN=$(curl -s http://localhost:8180/realms/wager/protocol/openid-connect/token \
  -d grant_type=client_credentials \
  -d client_id=provider-a \
  -d client_secret=provider-a-secret | jq -r .access_token)   # requer jq
```

A validação dos tokens pela aplicação entra na Fase 07.

## Exemplos de chamadas

_A definir._

## Testes

```sh
make check        # gofmt, go vet e go test -race
go test ./...
go test -race ./...
go vet ./...
```

### Testes de integração

Rodam contra o ambiente real do Compose, sem mocks
([ADR 0007](docs/adr/0007-testes-de-integracao-com-compose.md)), e ficam atrás da build
tag `integration`:

```sh
make up
make test-integration   # go test -race -count=1 -tags=integration ./...
```

Hoje verificam os roles do PostgreSQL, as filas e o redrive, os tokens do Keycloak e a
liveness da aplicação.

_Múltiplas instâncias e simulações de falha: a definir (Fase 12)._

## Arquitetura

Decisões técnicas em [ARCHITECTURE.md](ARCHITECTURE.md) e nos ADRs em [docs/adr/](docs/adr/).

## Uso de IA

Este projeto foi desenvolvido com apoio do Claude Code (Anthropic) como assistente de
programação. O uso seguiu estas regras:

- Modelagem, decisões de arquitetura e planos de cada fase foram discutidos e aprovados
  pelo autor antes da implementação.
- Todo código gerado foi revisado pelo autor; os merges para `main` são feitos por ele.
- Commits com participação da IA trazem o trailer `Co-Authored-By`.
