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
cmd/wallet-service/      main: cria a aplicação Fx (Fase 02)
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
deploy/                  arquivos do Compose: realm do Keycloak, init do LocalStack
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
- Variáveis de componentes (nomes provisórios): `APP_ENABLE_HTTP`,
  `APP_ENABLE_SQS_CONSUMER`, `APP_ENABLE_OUTBOX_PUBLISHER`, `APP_ENABLE_PENDING_WORKER`.

*A definir (Fases 02, 07 a 10):* prazos de shutdown por componente, término observável
dos workers e teste da composição Fx.

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
- **Validação:** local, pelo JWKS do realm: assinatura, `iss`, `aud`
  (`wager-wallet-service`, via audience mapper) e `exp`.
- **Identidade:** o `providerId` vem do claim `provider_id` do token, nunca do corpo. Um
  corpo com outro `providerId` é recusado com `403 PROVIDER_FORBIDDEN`.
- **Permissões:** role `provider` (enviar e consultar as próprias operações) e role
  `wallet-admin` (operações de carteira, restritas ao serviço interno).
- **SQS:** a mensagem não carrega token. A entrada é protegida pela política do broker:
  só as credenciais de um provedor podem enviar para a fila. Na AWS real, o consumidor
  mapearia o atributo de sistema `SenderId` da mensagem para um `providerId` e recusaria
  a mensagem quando ele divergisse de `data.providerId`. **No LocalStack Community as
  políticas IAM não são aplicadas; essa é uma limitação documentada.** As validações de
  domínio continuam no consumidor.

*A definir (Fases 02 e 07):* realm, clients e identidades de teste; políticas IAM que
seriam aplicadas na AWS; matriz rota × role.

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

## 10. Seções a definir

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
