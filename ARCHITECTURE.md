# Arquitetura

Este documento registra as decisões técnicas do `wager-wallet-service`. Os detalhes e as
alternativas de cada decisão estão nos ADRs em [docs/adr/](docs/adr/). Seções marcadas
como *a definir* serão preenchidas nas fases indicadas.

## 1. Visão geral

Um único binário Go (`cmd/wallet-service`) executa quatro componentes, cada um ligável
por variável de ambiente ([ADR 0002](docs/adr/0002-binario-unico.md)):

| Componente | Função |
| --- | --- |
| Servidor HTTP | Abre carteiras, recebe operações de provedores, consultas, reconciliação, health checks |
| Consumidor SQS | Recebe as mesmas operações pela fila `wager-transactions.fifo` |
| Publisher da outbox | Publica em `wallet-events.fifo` os eventos já confirmados no banco |
| Worker de referências pendentes | Retoma operações em `PENDING_REFERENCE` com backoff |

Várias instâncias do mesmo binário rodam em paralelo. Toda a coordenação entre elas
acontece no PostgreSQL (locks por linha, `SKIP LOCKED`, constraints e triggers). Nenhuma
instância guarda estado que outra precise.

## 2. Camadas e regra de dependência

Detalhes em [ADR 0001](docs/adr/0001-camadas-e-regra-de-dependencia.md).

```
             ┌──────────────────────────────────────────────┐
             │ bootstrap (Fx) + cmd/                        │  liga tudo
             └──────────────────────────────────────────────┘
   ┌──────────────────────────────────┐   ┌──────────────────────────┐
   │ adapter: postgres, httpapi,      │   │ platform: config,        │
   │          sqs, oidc               │   │ logging, metrics, health │
   └──────────────────────────────────┘   └──────────────────────────┘
                  │ implementa portas / chama casos de uso
                  ▼
   ┌──────────────────────────────────┐
   │ app: store (portas), wagering,   │
   │      wallets, outbox, pendingref │
   └──────────────────────────────────┘
                  │ usa
                  ▼
   ┌──────────────────────────────────┐
   │ domain: money, wallet, wager,    │   só stdlib + UUID
   │         event                    │
   └──────────────────────────────────┘
```

- `domain` não importa nenhuma outra camada, nem Fx, `net/http`, `database/sql`, pgx ou
  o SDK da AWS.
- `app` declara as interfaces de que precisa (portas) e não importa `adapter`.
- `adapter` é a única camada que conhece pgx, `net/http`, SQS e OIDC.
- Só `bootstrap` e `cmd/` importam Fx.

A regra é verificada automaticamente por `internal/archtest` em todo `go test`.

## 3. Estrutura de pacotes

```
cmd/wallet-service/      main: carrega a config, cria a aplicação Fx; subcomando healthcheck
internal/
  domain/
    money/               Money: int64 em unidades mínimas + moeda ISO 4217
    wallet/              agregado Wallet + LedgerEntry
    wager/               WagerTransaction, tipos, estados, failureCode
    event/               envelope e eventos tipados
  app/
    store/               portas de persistência e TxRunner
    wagering/            processamento de operações (HTTP e SQS)
    wallets/             abertura, consultas, reconciliação
    outbox/              publisher da outbox
    pendingref/          worker de referências pendentes
  adapter/
    postgres/            repositórios e TxRunner com pgx
    httpapi/             rotas, handlers, DTOs, middleware de autenticação
    sqs/                 consumidor de entrada e cliente de publicação
    oidc/                validação de JWT e extração do Principal
  platform/
    config/ logging/ metrics/ health/
  bootstrap/             módulos Fx e ciclo de vida
  archtest/              teste da regra de dependência
migrations/              migrations golang-migrate (up e down)
test/integration/        testes de integração (build tag integration)
deploy/                  provisionamento do Compose (Postgres, LocalStack, Keycloak) e políticas IAM
docs/adr/                Architecture Decision Records
```

## 4. Composição com Fx e ciclo de vida

- Cada área tem um `fx.Module` em `internal/bootstrap`, com `fx.Provide` dos
  construtores e `fx.Invoke` para registrar os componentes no `fx.Lifecycle`.
- Domínio e casos de uso não conhecem Fx: seus construtores são funções Go comuns.
- **Inicialização:** configuração validada → logger → pool do PostgreSQL (com ping) →
  cliente SQS → casos de uso → componentes ligados (HTTP, consumidor, workers).
- **Encerramento:** o Fx executa os `OnStop` na ordem inversa. Primeiro param as
  entradas (o HTTP para de aceitar conexões; o consumidor para de buscar mensagens),
  depois o trabalho em andamento termina ou é liberado, e só então o pool do banco e os
  clientes são fechados.
- Variáveis de componentes: `APP_ENABLE_HTTP`, `APP_ENABLE_SQS_CONSUMER`,
  `APP_ENABLE_OUTBOX_PUBLISHER`, `APP_ENABLE_PENDING_WORKER` (padrão `true`; pelo menos
  um ligado). Um componente desligado não entra no grafo do Fx.
- A configuração é lida e validada em `main`, antes do Fx (`platform/config`, só
  stdlib); todos os erros são reportados de uma vez. Prazo total de encerramento:
  `APP_SHUTDOWN_TIMEOUT` (`fx.StopTimeout`).
- O servidor HTTP abre a porta de forma síncrona no `OnStart` (falha cedo se estiver
  ocupada) e usa `Shutdown` com prazo no `OnStop`. Se ele cair depois de iniciado, a
  aplicação inteira encerra com código 1 (`fx.Shutdowner`).
- `wallet-service healthcheck` chama `/health/live` da própria instância; é o
  healthcheck do container, já que a imagem distroless não tem `curl`.
- Testes: `fx.ValidateApp` para cada combinação de componentes e `fxtest` para início,
  resposta HTTP e liberação da porta após o encerramento.

*A definir (Fases 07 a 10):* prazos de shutdown por componente, término observável
dos workers e teste com `fxtest` comprovando a liberação de recursos dos workers.

## 5. Fluxo de uma operação

HTTP e SQS compartilham o mesmo núcleo ([ADR 0006](docs/adr/0006-entradas-http-e-sqs.md)):

```
HTTP ─► httpapi: autentica, valida providerId do token,
        DTO → wagering.Command ──────────────► wagering.Process(ctx, cmd)
                                                        │
SQS  ─► sqs: lê envelope, DTO → Command,                │
        monta InboundMessage ───────► wagering.ProcessFromMessage(ctx, cmd, msg)
                                                        │
                       TxRunner.WithinTx(ctx, func(ctx, repos) error {
                           [SQS] registra a mensagem na inbox
                           núcleo: hash canônico, idempotência, lock da carteira,
                                   regras de domínio, saldo, ledger, outbox
                           [SQS] marca a mensagem como concluída
                       })  ─► COMMIT
                                                        │
                       HTTP responde / SQS apaga a mensagem (só depois do commit)
```

- O `Command` contém apenas campos de negócio: é exatamente o que entra no hash de
  idempotência. Metadados de transporte (chave de idempotência, `messageId`) ficam fora.
- A construção do `Command` recusa valores negativos com `VALIDATION_ERROR`
  ([ADR 0004](docs/adr/0004-serializacao-json-do-money.md)).
- O publisher da outbox e o worker de referências pendentes rodam separadamente e
  também usam o `TxRunner`.

## 6. Transação SQL entre repositórios

Detalhes em [ADR 0005](docs/adr/0005-delimitacao-da-transacao-sql.md).

A camada `app` declara `store.TxRunner`, com
`WithinTx(ctx, func(ctx, store.Repos) error) error`; `adapter/postgres` a implementa com
pgx. Os repositórios só existem dentro do callback, então toda gravação acontece dentro
de uma transação. Erro no callback → rollback; sucesso → commit. Dentro do callback não
há I/O externo: eventos saem pela outbox depois do commit.

*A definir (Fase 05):* isolamento, `FOR UPDATE`, ordem fixa de locks e `lock_timeout`.

## 7. Autenticação e autorização

Detalhes em [ADR 0003](docs/adr/0003-identidade-do-provedor.md).

- **IdP:** Keycloak, com realm importado automaticamente no Compose. Provedores e o
  serviço interno são clients confidenciais que usam `client_credentials`.
- **Validação:** local, sem discovery
  ([ADR 0009](docs/adr/0009-validacao-oidc-sem-discovery.md)): chaves buscadas em
  `OIDC_JWKS_URL`; `iss` comparado como texto exato com `OIDC_ISSUER_URL`; `aud` deve
  conter `wager-wallet-service` (audience mapper); `exp` obrigatório. O Keycloak tem
  hostname fixo (`KC_HOSTNAME`), então o `iss` é o mesmo para tokens pedidos de fora ou
  de dentro da rede do Docker.
- **Identidade:** o `providerId` vem do claim `provider_id` do token, nunca do corpo. Um
  corpo com outro `providerId` é recusado com `403 PROVIDER_FORBIDDEN`.
- **Permissões:** roles **de realm** `provider` (enviar e consultar as próprias
  operações) e `wallet-admin` (operações de carteira, restritas ao serviço interno),
  atribuídas à service account de cada client e lidas do claim `realm_access.roles`.
- **SQS:** a mensagem não carrega token. A entrada é protegida pela política do broker:
  só as credenciais de um provedor podem enviar para a fila. Na AWS real, o consumidor
  mapearia o atributo de sistema `SenderId` da mensagem para um `providerId` e recusaria
  a mensagem quando ele divergisse de `data.providerId`. **No LocalStack Community as
  políticas IAM não são aplicadas; essa é uma limitação documentada.** As validações de
  domínio continuam no consumidor. As políticas que seriam aplicadas na AWS estão em
  [deploy/aws/iam-policies.md](deploy/aws/iam-policies.md).
- **Identidades de teste:** clients `provider-a`, `provider-b` (role de realm
  `provider`) e `wallet-internal` (role de realm `wallet-admin`) no realm
  [deploy/keycloak/realm-wager.json](deploy/keycloak/realm-wager.json).

*A definir (Fase 07):* matriz rota × role.

## 8. Classificação de erros

Todo resultado de uma operação cai em uma destas classes. A classe decide se algo é
gravado, o status HTTP e o destino da mensagem SQS.

| Classe | Exemplos (`failureCode`) | Grava no banco? | HTTP *(proposta, Fase 07)* | SQS |
| --- | --- | --- | --- | --- |
| **Sucesso** | `PROCESSED` | sim | `200` (replay também `200`, com `idempotentReplay: true`) | apaga após o commit |
| **Pendente** | `PENDING_REFERENCE` | sim, e o worker assume | `202` | apaga após o commit |
| **Rejeição definitiva** | `INSUFFICIENT_FUNDS`, `INSUFFICIENT_FUNDS_FOR_REVERSAL`, `REFERENCE_NOT_FOUND`, `REFERENCE_NOT_PROCESSED`, `REFERENCE_ALREADY_REVERSED`, `INVALID_REFERENCE_KIND`, `REFERENCE_AMOUNT_MISMATCH`, `REFERENCE_CONTEXT_MISMATCH` | sim: `REJECTED` + evento `WagerTransactionRejected` | `422`, com `transactionId`, `status` e `failureCode`; o replay devolve a mesma resposta | apaga após o commit (terminal) |
| **Entrada corrigível** | `VALIDATION_ERROR` (`400`), `WALLET_MISMATCH` (`400`), `PROVIDER_FORBIDDEN` (`403`), `WALLET_NOT_FOUND` (`404`), `IDEMPOTENCY_CONFLICT` (`409`) | não | conforme o código ao lado | erro permanente da mensagem → DLQ |
| **Falha transitória** | PostgreSQL indisponível, `lock_timeout`, deadlock, resultado de commit desconhecido, prazo do `context` esgotado | não (rollback) | `503` com `Retry-After` | não apaga; volta após o visibility timeout; esgotadas as tentativas → DLQ |
| **Falha permanente** | `FAILED`, só para `PENDING_REFERENCE` com erro permanente no worker | sim, para auditoria | visível na consulta da transação | não se aplica (a mensagem já foi concluída) |

Outros casos:

- **Autenticação:** token ausente, inválido ou expirado → `401`; role insuficiente →
  `403`. Nada é gravado.
- **Mensagem SQS inválida** (JSON malformado, tipo desconhecido, mesmo `messageId` com
  hash diferente) → DLQ.
- **Erro inesperado** (bug): HTTP `500`; no SQS a mensagem não é apagada e segue o
  caminho de retry até a DLQ.

Como isso aparece no código Go:

- Rejeição definitiva **não é um `error` Go**: é um resultado concluído e confirmado
  (`Result` com `status = REJECTED`). Por isso o replay pode reproduzi-la.
- Entrada corrigível e falha transitória são `error`, classificáveis com
  `errors.Is`/`errors.As`.
- O mapeamento classe → status HTTP e classe → ação SQS fica em uma única função em cada
  adapter (`httpapi` e `sqs`).

*A definir:* corpo exato das respostas de erro e código da falha transitória (Fase 07);
conflito na abertura de carteira duplicada (`409`, código a definir na Fase 06); limites
de tentativas, visibility timeout e mecanismo de envio à DLQ (Fase 08).

## 9. Dinheiro

- `Money` é um value object imutável: `int64` em unidades mínimas (centavos) e moeda
  ISO 4217. Persistência em `BIGINT` + `CHAR(3)`.
- Serialização JSON no próprio domínio, formato `{"amount":"25.00","currency":"BRL"}`
  ([ADR 0004](docs/adr/0004-serializacao-json-do-money.md)). `amount` precisa ser string
  JSON; número JSON é recusado. O `UnmarshalJSON` aceita sinal; a proibição de negativos
  nas entradas externas fica na construção do `Command`.
- Parsing: validação do formato e conversão dos dígitos sem o ponto com
  `strconv.ParseInt`; estouro detectado por `strconv.ErrRange`.

*A definir (Fase 04):* limites numéricos, regras exatas de formato e overflow na
aritmética.

## 10. Ambiente local e testes de integração

O `docker-compose.yml` sobe PostgreSQL 18.6, LocalStack 4.14.0, Keycloak 26.8.0 e a
aplicação, todos com healthcheck e limite de memória. A aplicação só inicia depois das
dependências saudáveis.

| Serviço | Provisionamento | Healthcheck | Memória |
| --- | --- | --- | --- |
| PostgreSQL | `deploy/postgres/initdb/01-roles.sh`: banco e roles, senhas por variável | `pg_isready` via TCP (só passa após o initdb) | 256 MB |
| LocalStack | `deploy/localstack/init/ready.d/10-queues.sh`: 3 filas FIFO e redrive | arquivo de pronto criado pelo script | 512 MB |
| Keycloak | import do realm `wager` | `/health/ready` na porta 9000 via `/dev/tcp` (sem `curl`) | 768 MB, heap até 512 MB |
| app | — | `wallet-service healthcheck` | 128 MB, `GOMEMLIMIT=100MiB` |

- **LocalStack fixado** na última versão Community que não exige auth token, com o
  MiniStack como saída documentada
  ([ADR 0008](docs/adr/0008-versao-do-localstack.md)).
- **Testes de integração** contra esse mesmo ambiente, com a build tag `integration`;
  cada teste cria os próprios dados
  ([ADR 0007](docs/adr/0007-testes-de-integracao-com-compose.md)).
- **Imagem da aplicação:** build multi-stage, binário estático, imagem final
  `distroless/static-debian13:nonroot` (cerca de 18 MB).

## 11. Seções a definir

| Tema | Fase |
| --- | --- |
| Concorrência e locks por carteira | 05 |
| Idempotência: hash canônico, campos e normalizações | 06 |
| Reversões (`REFUND`/`ROLLBACK`) e suas combinações | 06 |
| Inbox, visibility timeout, `MessageGroupId`/`MessageDeduplicationId` | 08 |
| Outbox: lease, backoff, contrato dos eventos de saída | 09 |
| Referências pendentes: backoff, TTL, estados da referência | 10 |
| Reconciliação, observabilidade e health checks | 11 |
| Testes de concorrência e falhas | 12 |
| Limitações, interpretações adotadas e trabalho não concluído | 13 |
