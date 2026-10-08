# ADR 0012 — Idempotência por provedor com hash canônico

- **Status:** aceito
- **Data:** 2026-10-07

**Contexto.** Operações chegam repetidas (HTTP e SQS, inclusive ao mesmo tempo). O replay
deve devolver o resultado original; conteúdo diferente com a mesma chave é conflito; a
mesma operação não pode ser reaplicada com outra chave.

**Decisão.** Unicidade no banco por `(provider_id, idempotency_key)` e por
`(provider_id, external_transaction_id)`. O hash é SHA-256 do JSON canônico do `Command`
(só campos de negócio, chaves em ordem alfabética, `money` como strings, UUIDs
minúsculos, sem a chave de idempotência). O fluxo é: buscar a operação (replay ou
conflito); validar a carteira; `INSERT ... ON CONFLICT DO NOTHING` em `PENDING`; se o
INSERT não gravou, uma requisição concorrente venceu, e a busca é repetida depois que a
transação dela termina. O replay devolve o estado e o saldo gravados no processamento
original.

**Alternativa.** Cache de idempotência em memória ou Redis: não sobrevive a reinício e não
coordena processos; o banco já é a fonte da verdade.
