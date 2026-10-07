# ADR 0005 — Delimitação da transação SQL entre repositórios (TxRunner)

- **Status:** aceito
- **Data:** 2026-10-07

## Contexto

Uma operação financeira grava, de forma atômica, a transação de aposta, o saldo da
carteira, o lançamento do extrato, os eventos da outbox e, na entrada por SQS, o registro
da inbox. Cada tabela tem seu repositório, mas todos precisam usar a mesma transação SQL.
O desafio pede que a delimitação da transação entre os repositórios seja explícita e
documentada, e o ADR 0001 proíbe que a camada `app` conheça `pgx`.

## Decisão

A camada `app` declara, no pacote `internal/app/store`, uma porta de transação baseada em
callback:

```go
type TxRunner interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context, r Repos) error) error
}

type Repos struct {
	Wallets      WalletRepository
	Transactions TransactionRepository
	Ledger       LedgerRepository
	Inbox        InboxRepository
	Outbox       OutboxRepository
}
```

`internal/adapter/postgres` implementa o `TxRunner`:

1. abre a transação com `pool.BeginTx` (READ COMMITTED);
2. aplica `SET LOCAL lock_timeout` (configuração `DB_LOCK_TIMEOUT`);
3. registra `defer tx.Rollback(ctx)`, que não tem efeito depois de um commit e cobre
   retorno antecipado e `panic`;
4. monta os repositórios ligados a essa `pgx.Tx` e chama `fn`;
5. se `fn` retornar erro, faz rollback e devolve o erro; senão, faz commit.

Regras de uso:

- **Os repositórios só existem dentro de `fn`.** Não há como gravar fora de uma
  transação nem misturar duas transações.
- **Sem I/O externo dentro de `fn`** (nada de publicar no SQS ou chamar HTTP). Efeitos
  externos saem pela outbox, depois do commit.
- **Sem `WithinTx` aninhado.**
- **Erro no commit é tratado como resultado desconhecido** (a conexão pode ter caído
  depois de o servidor confirmar). Ele é classificado como falha transitória; a
  repetição é segura porque a idempotência é persistente.
- A ordem fixa de locks (inbox → transação → carteira) e o uso de `FOR UPDATE` são
  detalhados na Fase 05.

## Alternativas consideradas

- **Transação escondida no `context.Context`:** os repositórios procuram a `pgx.Tx` no
  contexto. A assinatura fica limpa, mas a fronteira da transação fica implícita: um
  repositório chamado com o contexto errado grava fora da transação sem nenhum erro.
- **Unit of Work com `Begin`/`Commit`/`Rollback` expostos à camada app:** explícito, mas
  cada caso de uso precisa lembrar do rollback em todos os caminhos de erro.
- **Repositórios recebendo `pgx.Tx`:** simples, mas faz a camada `app` importar `pgx`,
  violando o ADR 0001.
- **Um método de repositório por caso de uso** (ex.: `SaveProcessedOperation(...)` com
  todo o SQL dentro): esconde a regra de negócio no adapter e dificulta testar a
  orquestração.

## Consequências

- A fronteira da transação fica visível no código do caso de uso: tudo o que está
  dentro de `WithinTx` é atômico.
- Os testes de caso de uso podem usar um `TxRunner` real (integração) ou, nos testes
  unitários, uma implementação em memória da porta. Isso não substitui o PostgreSQL nos
  testes de integração.
- A reconciliação precisa de uma leitura `REPEATABLE READ READ ONLY`; ela terá uma
  variante própria da porta, definida na Fase 11.
- Se e onde haverá retry automático para falhas transitórias (por exemplo, `lock_timeout`
  ou deadlock) é decidido nas Fases 05 e 06; o `TxRunner` em si não repete.
