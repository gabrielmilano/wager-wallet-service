# ADR 0011 — Invariantes financeiras impostas pelo banco

- **Status:** aceito
- **Data:** 2026-10-07

## Contexto

O desafio exige que unicidade, não negatividade e imutabilidade do ledger sejam impostas
pelo schema, por constraints e por mecanismos do banco, independentemente de locks locais
e da deduplicação do SQS. A regra de ouro da modelagem é que o PostgreSQL seja a última
linha de defesa: mesmo com um bug no Go, ele recusa um estado financeiro inválido.

## Decisão

### Três camadas

1. **Constraints declarativas:** `CHECK`, `UNIQUE`, FKs compostas (moeda e jogador da
   operação são os da carteira; o lançamento pertence à operação da mesma carteira) e
   índices únicos parciais (idempotência, abertura única, reversão única).
2. **Permissões (GRANTs):** o `app_runtime` não tem `DELETE` nem `TRUNCATE` em tabela
   alguma; o extrato é só `SELECT, INSERT`; os demais `UPDATE`s são concedidos **por
   coluna** (só o que pode mudar).
3. **Triggers:** valem para **qualquer** role, inclusive o dono das tabelas.

| Regra | Onde | Tipo |
| --- | --- | --- |
| Extrato append-only (`UPDATE`, `DELETE`, `TRUNCATE`) | `wallet_ledger_entries` | trigger |
| `DELETE` e `TRUNCATE` proibidos | `wager_transactions` | trigger |
| Sequência do extrato e regra do primeiro lançamento | `wallet_ledger_entries` | trigger |
| E1: carteira = último lançamento no commit | `wallets` (INSERT/UPDATE) e `wallet_ledger_entries` (INSERT) | constraint trigger diferido |
| E2: lançamento coerente com a operação (valor, moeda, direção) | `wallet_ledger_entries` | trigger |
| E2: operação lançada está `PROCESSED` no commit | `wallet_ledger_entries` | constraint trigger diferido |
| E3: `PENDING` nunca chega ao commit | `wager_transactions` | constraint trigger diferido |
| E4: máquina de estados, terminais congelados, campos de negócio imutáveis | `wager_transactions` | trigger |
| E5: versão da carteira acompanha o saldo; identidade imutável | `wallets` | trigger |
| Conteúdo do evento imutável; evento publicado congelado | `outbox_events` | trigger |
| Identidade e hash da mensagem imutáveis | `inbox_messages` | trigger |

### Máquina de estados (E4)

| Situação | Permitido |
| --- | --- |
| INSERT de operação externa | só `PENDING` |
| INSERT de `OPENING` | só `PROCESSED` |
| de `PENDING` | `PENDING_REFERENCE`, `PROCESSED`, `REJECTED` |
| de `PENDING_REFERENCE` | `PENDING_REFERENCE`, `PROCESSED`, `REJECTED`, `FAILED` |
| de `PROCESSED`, `REJECTED`, `FAILED` | nada |

Consequência prática: um `UPDATE` que mantém `PENDING` é recusado. A aplicação grava a
referência resolvida, o código de falha e o resultado **no mesmo `UPDATE`** que leva ao
estado final.

### Como os erros chegam à aplicação

Triggers usam `RAISE EXCEPTION ... USING ERRCODE = <classe 23>, CONSTRAINT = '<nome>'`:
`check_violation` (`23514`) para regras de valor e transição e
`integrity_constraint_violation` (`23000`) para imutabilidade e append-only. Assim CHECK,
UNIQUE, FK e trigger chegam ao Go do mesmo jeito, em `pgconn.PgError.Code` e
`.ConstraintName`; a Fase 05 mapeia por nome, sem comparar texto de mensagem.

### Triggers diferidos

E1, E2 (parte de commit) e E3 são `CONSTRAINT TRIGGER ... DEFERRABLE INITIALLY
DEFERRED`: rodam no commit, quando a transação já fez todos os passos. Eles **releem a
linha atual** com `SELECT` em vez de confiar no `NEW` do evento que os enfileirou, porque
a mesma linha pode ter mudado depois (por exemplo, `PENDING` → `PROCESSED`). Uma recusa no
commit chega como `PgError` no `Commit()`, com a transação já desfeita pelo servidor: é
resultado conhecido e definitivo (ADR 0005).

### Concorrência: o trigger de sequência não basta sozinho

O trigger de sequência lê o último lançamento da carteira e confere `balance_before` e
`wallet_version`. **Sozinho, ele não é seguro sob concorrência:** duas transações
simultâneas na mesma carteira podem ler o mesmo "último lançamento" antes de qualquer uma
confirmar, e as duas passariam na verificação. A garantia real vem de:

- `UNIQUE (wallet_id, wallet_version)`: só uma das duas consegue gravar aquela versão; a
  outra falha com `23505`;
- `SELECT ... FOR UPDATE` na carteira (Fase 05): a segunda transação espera a primeira
  terminar e então lê o estado já atualizado.

O trigger acrescenta a verificação de conteúdo (o "antes" é o "depois" anterior), que o
`UNIQUE` não faz.

## Alternativas consideradas

- **Só constraints declarativas:** não expressam "append-only", transições de estado nem
  regras entre linhas e tabelas.
- **Só GRANTs:** protegem contra a aplicação, mas não contra uma migration ou um operador
  com o role dono.
- **Validar tudo só no Go:** um bug ou um acesso direto ao banco quebraria as invariantes;
  o desafio exige o banco como garantia.
- **Triggers diferidos que confiam no `NEW`:** mais simples, mas validariam um estado
  intermediário já superado dentro da mesma transação.

## Consequências

- Cada invariante tem teste de integração que confere SQLSTATE e nome da constraint, como
  `app_runtime` e, quando a permissão já barra, como `app_migrator`.
- Há custo extra por escrita: leituras do último lançamento e da operação dentro dos
  triggers. Os índices únicos de `(wallet_id, wallet_version)` e das PKs atendem essas
  leituras.
- **Limitação:** o dono da tabela ou um superusuário consegue desligar triggers
  (`ALTER TABLE ... DISABLE TRIGGER`) ou alterar o schema. Por isso a aplicação nunca usa
  o `app_migrator`; numa instalação real, o acesso a esse role seria restrito e auditado.
