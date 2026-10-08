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
migrations/              migrations golang-migrate (up e down), embutidas no binário
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

**Erro no commit:** com `PgError` (por exemplo, um trigger diferido recusou), o
resultado é conhecido, a transação foi desfeita e o erro é definitivo; sem resposta do
servidor, o resultado é desconhecido e tratado como transitório (a idempotência torna a
repetição segura).

Isolamento, locks e `lock_timeout`: seção 13.

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

**Matriz de autorização** (roles de realm):

| Rota | `provider` | `wallet-admin` |
| --- | --- | --- |
| `POST /wagering/transactions` | sim, com o próprio `providerId` | não (`403`) |
| `GET /wagering/transactions/{id}` | só as próprias (de outro provedor: `404`) | todas |
| `GET /providers/{providerId}/wagering/transactions/{ext}` | só o próprio `providerId` (outro: `403`) | todos |
| `POST /wallets`, `GET /wallets/{id}`, `GET /wallets/{id}/ledger`, `POST /wallets/{id}/reconciliation` | não (`403`) | sim |
| `GET /health/live`, `GET /health/ready` | público | público |

Testes ponta a ponta contra o Keycloak real cobrem token ausente, malformado, adulterado
e expirado (client `provider-a-short`, tokens de 1 s), a matriz acima, o isolamento entre
provedores em consultas e replays e a ausência de efeito financeiro nos acessos negados.

## 8. Classificação de erros

Todo resultado de uma operação cai em uma destas classes. A classe decide se algo é
gravado, o status HTTP e o destino da mensagem SQS. Todo corpo de erro tem
`{"code", "message", "correlationId"}`.

| Classe | Códigos | Grava no banco? | HTTP | SQS |
| --- | --- | --- | --- | --- |
| **Sucesso** | `PROCESSED` | sim | `200` (replay também, com `idempotentReplay: true`); `POST /wallets` → `201` | apaga após o commit |
| **Pendente** | `PENDING_REFERENCE` | sim; o worker assume | `202` | apaga após o commit |
| **Rejeição definitiva** | `INSUFFICIENT_FUNDS`, `INSUFFICIENT_FUNDS_FOR_REVERSAL`, `REFERENCE_NOT_FOUND`, `REFERENCE_NOT_PROCESSED`, `REFERENCE_ALREADY_REVERSED`, `INVALID_REFERENCE_KIND`, `REFERENCE_AMOUNT_MISMATCH`, `REFERENCE_CONTEXT_MISMATCH` | sim: `REJECTED` + `WagerTransactionRejected` | `422` com `transactionId`, `status` e `failureCode`; o replay devolve `422` com `idempotentReplay: true` | apaga após o commit (terminal) |
| **Entrada corrigível** | `VALIDATION_ERROR` e `WALLET_MISMATCH` (`400`), `PROVIDER_FORBIDDEN` (`403`), `WALLET_NOT_FOUND` e `TRANSACTION_NOT_FOUND` (`404`), `IDEMPOTENCY_CONFLICT` e `WALLET_ALREADY_EXISTS` (`409`) | não | conforme o código | DLQ (reentregar não corrige) |
| **Falha transitória** | `SERVICE_UNAVAILABLE`: PostgreSQL fora, `lock_timeout`, deadlock, resultado de commit desconhecido | não (rollback) | `503` com `Retry-After: 1` | não apaga; volta após o visibility timeout; esgotadas as tentativas → DLQ |
| **Falha permanente** | `FAILED`, só para `PENDING_REFERENCE` com erro permanente no worker | sim, para auditoria | consulta mostra `FAILED`; replay → `422` | não se aplica |
| **Erro inesperado** | `INTERNAL_ERROR` (detalhe só no log, ligado pelo `correlationId`) | não | `500` | não apaga; retry até a DLQ |

Autenticação e autorização (antes dos casos de uso, nada é gravado):

- token ausente, malformado, adulterado, de outro issuer ou audiência, ou expirado →
  `401 UNAUTHORIZED` com `WWW-Authenticate: Bearer`;
- role insuficiente para a rota → `403 FORBIDDEN`;
- `providerId` do corpo ou do caminho diferente do token → `403 PROVIDER_FORBIDDEN`.

Como isso aparece no código Go:

- Rejeição definitiva **não é um `error` Go**: é um resultado concluído e confirmado
  (`Result` com `status = REJECTED`). Por isso o replay pode reproduzi-la.
- Entrada corrigível e falha transitória são `*apperr.Error`, com `Kind` e `Code`;
  o mapeamento classe → status HTTP fica em `httpapi.writeError` e classe → ação SQS no
  consumidor.
- Erro de banco transitório (55P03, 40001, 40P01, conexão) vira `store.ErrUnavailable` no
  adapter e `apperr.Unavailable` no caso de uso.

## 9. Dinheiro

- `Money` é um value object imutável: `int64` em unidades mínimas (centavos) e moeda
  ISO 4217. Persistência em `BIGINT` + `CHAR(3)`.
- Serialização JSON no próprio domínio, formato `{"amount":"25.00","currency":"BRL"}`
  ([ADR 0004](docs/adr/0004-serializacao-json-do-money.md)). `amount` precisa ser string
  JSON; número JSON é recusado. O `UnmarshalJSON` aceita sinal; a proibição de negativos
  nas entradas externas fica na construção do `Command`.
- Parsing: validação do formato e conversão dos dígitos sem o ponto com
  `strconv.ParseInt`; estouro detectado por `strconv.ErrRange`.

- **Formato aceito:** sinal `-` opcional, parte inteira sem zeros à esquerda e
  exatamente duas casas (`25.00`, `0.50`, `-5.00`). Recusa vazio, espaços, `+`, vírgula,
  notação científica, `NaN`, `Infinity`, escala diferente de 2 e `-0.00`. Nada é
  arredondado.
- **Limites:** de `-92233720368547758.08` a `92233720368547758.07` (int64 em centavos).
  Parsing, soma, subtração e negação devolvem `ErrOverflow` além disso.
- **Moedas:** lista fechada `BRL`, `USD` e `EUR` (a mesma do CHECK do banco); aritmética e
  comparação entre moedas diferentes devolvem `ErrCurrencyMismatch`.

## 10. Schema e invariantes no banco

Cinco tabelas (`wallets`, `wager_transactions`, `wallet_ledger_entries`,
`inbox_messages`, `outbox_events`), criadas por migrations embutidas no binário
([ADR 0010](docs/adr/0010-migrations-embutidas.md)). O banco é a última linha de defesa
([ADR 0011](docs/adr/0011-invariantes-no-banco.md)), em três camadas:

1. **Constraints:** CHECKs (saldo ≥ 0, moeda em `BRL`/`USD`/`EUR`, valor por tipo,
   conta de cada lançamento), UNIQUEs (carteira por jogador e moeda; operação por
   provedor e ID externo; chave de idempotência por provedor; uma abertura por carteira;
   uma reversão processada por original; uma versão por carteira no extrato) e FKs
   compostas (moeda e jogador da carteira; lançamento da operação da mesma carteira).
2. **GRANTs:** `app_runtime` sem `DELETE`/`TRUNCATE`; extrato só `SELECT`/`INSERT`;
   `UPDATE` por coluna nas demais tabelas.
3. **Triggers** (valem para qualquer role): extrato append-only; `wager_transactions` sem
   `DELETE`/`TRUNCATE`; sequência do extrato; máquina de estados com terminais
   congelados; versão da carteira acompanha o saldo; e, no commit (diferidos),
   carteira igual ao último lançamento, lançamento só de operação `PROCESSED` e nenhum
   `PENDING` confirmado.

Erros de trigger usam SQLSTATE de classe 23 e o nome da regra em `ConstraintName`, como
as constraints declarativas. O trigger de sequência do extrato não é seguro sozinho sob
concorrência: a garantia é o `UNIQUE (wallet_id, wallet_version)` somado ao
`FOR UPDATE` na carteira (Fase 05).

## 11. Ambiente local e testes de integração

O `docker-compose.yml` sobe PostgreSQL 18.6, LocalStack 4.14.0, Keycloak 26.8.0 e a
aplicação, todos com healthcheck e limite de memória. A aplicação só inicia depois das
dependências saudáveis.

| Serviço | Provisionamento | Healthcheck | Memória |
| --- | --- | --- | --- |
| PostgreSQL | `deploy/postgres/initdb/01-roles.sh`: banco e roles, senhas por variável | `pg_isready` via TCP (só passa após o initdb) | 256 MB |
| LocalStack | `deploy/localstack/init/ready.d/10-queues.sh`: 3 filas FIFO e redrive | arquivo de pronto criado pelo script | 512 MB |
| Keycloak | import do realm `wager` | `/health/ready` na porta 9000 via `/dev/tcp` (sem `curl`) | 768 MB, heap até 512 MB |
| migrate | `wallet-service migrate up` como `app_migrator`; termina | — (a app espera ele terminar com sucesso) | 128 MB |
| app | — | `wallet-service healthcheck` | 128 MB, `GOMEMLIMIT=100MiB` |

- **LocalStack fixado** na última versão Community que não exige auth token, com o
  MiniStack como saída documentada
  ([ADR 0008](docs/adr/0008-versao-do-localstack.md)).
- **Testes de integração** contra esse mesmo ambiente, com a build tag `integration`;
  cada teste cria os próprios dados
  ([ADR 0007](docs/adr/0007-testes-de-integracao-com-compose.md)).
- **Imagem da aplicação:** build multi-stage, binário estático, imagem final
  `distroless/static-debian13:nonroot` (cerca de 18 MB).

## 12. Idempotência

Detalhes em [ADR 0012](docs/adr/0012-idempotencia-e-hash-canonico.md).

- **Chave:** header `Idempotency-Key` (HTTP) ou `data.idempotencyKey` (SQS), obrigatória;
  o servidor nunca a substitui por outra calculada. Escopo por provedor.
- **Hash:** SHA-256 do JSON canônico do `Command`: `externalTransactionId`, `gameId`,
  `kind`, `money` (`{"amount","currency"}` como strings), `playerId`, `providerId`,
  `referenceExternalTransactionId` (omitido se ausente), `roundId` e `walletId`, com as
  chaves em ordem alfabética. Fora do hash: a chave de idempotência e os metadados de
  transporte (`correlationId`, `messageId`). Normalização: UUIDs na forma minúscula com
  hífens (a entrada aceita maiúsculas). Valores monetários não são normalizados, porque o
  parsing só aceita a forma canônica. HTTP e SQS montam o mesmo `Command`, então geram o
  mesmo hash.
- **Resultados:** mesma chave e conteúdo → replay do estado e do saldo gravados
  (`idempotentReplay: true`), inclusive para rejeições e pendências; mesma chave com outro
  conteúdo → `IDEMPOTENCY_CONFLICT`; mesmo `externalTransactionId` com outra chave →
  `IDEMPOTENCY_CONFLICT`.
- **Corrida:** `INSERT ... ON CONFLICT DO NOTHING` espera a transação concorrente terminar.
  As duas buscas (por chave e por id externo) são comandos separados em READ COMMITTED; o
  código compara a chave do registro encontrado, para que um commit concorrente entre
  elas seja tratado como replay, e não como conflito. Esse caso foi encontrado pelo teste
  de 50 envios paralelos e tem teste de regressão.

## 13. Concorrência e locks

- **Fila por carteira:** `SELECT ... FOR UPDATE` na carteira. Carteiras diferentes não se
  esperam (sem lock global); a mesma carteira é processada uma operação por vez, em
  qualquer instância, porque o lock fica no PostgreSQL.
- **Ordem fixa de locks:** inbox (SQS) → operação (INSERT da linha) → carteira. Todos os
  caminhos seguem a mesma ordem, o que evita deadlock.
- **`lock_timeout`** (`DB_LOCK_TIMEOUT`, padrão 3 s) via `SET LOCAL`: quem espera além
  disso recebe falha transitória (503 / retry do SQS) e nada é gravado.
- **Lost update:** além do lock, o `UPDATE` da carteira exige `version = nova - 1`; o
  banco ainda confere o saldo contra o extrato no commit (E1, ADR 0011).
- **Isolamento:** READ COMMITTED. Depois de obter o lock, cada comando vê os commits
  anteriores, então a segunda aposta de 80.00 lê o saldo 20.00 e é rejeitada.

## 14. Operações, reversões e referências

| Tipo | Movimento | Regras |
| --- | --- | --- |
| `BET` | débito | valor > 0; sem saldo → `INSUFFICIENT_FUNDS` |
| `WIN` | crédito | valor > 0; com referência, ela precisa ser uma `BET` da mesma rodada e contexto |
| `LOSS` | nenhum | valor `0.00`; `PROCESSED` sem lançamento e sem mudar a versão |
| `REFUND` | crédito | referência obrigatória a uma `BET`; valor igual |
| `ROLLBACK` | oposto do original | referência obrigatória a `BET` (crédito), `WIN` ou `REFUND` (débito); valor igual; sem saldo → `INSUFFICIENT_FUNDS_FOR_REVERSAL` |

Resolução da referência (função pura `wager.ResolveReference`), nesta ordem:

1. não encontrada → `PENDING_REFERENCE` (primeira retentativa em 1 s, prazo de 1 h);
2. tipo incompatível → `INVALID_REFERENCE_KIND`;
3. jogador, carteira, moeda ou rodada diferentes → `REFERENCE_CONTEXT_MISMATCH`;
4. original em `PENDING_REFERENCE` → continua esperando;
5. original `REJECTED` ou `FAILED` → `REFERENCE_NOT_PROCESSED`;
6. valor diferente (REFUND/ROLLBACK) → `REFERENCE_AMOUNT_MISMATCH`;
7. original já revertida (REFUND/ROLLBACK) → `REFERENCE_ALREADY_REVERSED`.

**Combinações de REFUND e ROLLBACK:** cada operação original admite no máximo **uma**
reversão `PROCESSED` (REFUND **ou** ROLLBACK), garantida pelo domínio e pelo índice único
`ux_tx_single_reversal`. Uma BET reembolsada não pode ser desfeita de novo (o débito não é
devolvido duas vezes). Um `ROLLBACK` do próprio `REFUND` é permitido (debita de volta),
porque a original dele é o REFUND, e não a BET.

## 15. Limitações e interpretações adotadas

Interpretações (decididas sem regra explícita no enunciado):

- `referenceExternalTransactionId` em `BET` ou `LOSS` → `VALIDATION_ERROR` (o contrato só
  prevê referência em WIN, REFUND e ROLLBACK).
- `WIN` com referência confere tipo (`BET`), contexto e estado da BET, mas não o valor nem
  se a BET já foi revertida.
- A referência resolvida é registrada também nas rejeições por referência, para
  auditoria.
- Sem `correlationId` informado (header `X-Correlation-Id`), a API gera um UUID; os
  eventos usam esse valor.
- O serviço interno (`wallet-admin`) não envia operações de provedor e pode consultar
  operações de qualquer provedor.
- Replay de uma operação `FAILED` responde `422`, como as rejeições.
- Códigos da camada HTTP: `UNAUTHORIZED` (401), `FORBIDDEN` (role insuficiente),
  `INTERNAL_ERROR` (500).
- Códigos técnicos além do catálogo: `TRANSACTION_NOT_FOUND` (consulta), `MESSAGE_CONFLICT`
  (mesmo `messageId` com conteúdo diferente) e `SERVICE_UNAVAILABLE` (falha transitória).
- Eventos do mesmo commit são ordenados por `occurred_at` e `event_id` (UUIDv7,
  monotônico no processo).

Limitações:

- O "despertar" imediato de reversões pendentes quando a original chega (correção 1 da
  modelagem) não foi implementado: a retomada depende do worker de pendências.
- O dono das tabelas ou um superusuário pode desligar triggers (ADR 0011).
- LocalStack Community não aplica IAM (ADR 0003 e 0008).

## 16. Seções a definir

| Tema | Fase |
| --- | --- |
| Inbox, visibility timeout, `MessageGroupId`/`MessageDeduplicationId` | 08 |
| Outbox: lease, backoff, contrato dos eventos de saída | 09 |
| Referências pendentes: backoff, TTL, estados da referência | 10 |
| Reconciliação, observabilidade e health checks | 11 |
| Testes de concorrência e falhas | 12 |
| Trabalho não concluído (consolidado na entrega) | 13 |
