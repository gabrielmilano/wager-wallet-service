# ADR 0013 — Entrada SQS com inbox e saída por transactional outbox

- **Status:** aceito
- **Data:** 2026-10-07

**Contexto.** O SQS entrega pelo menos uma vez; a aplicação pode cair entre o commit e a
remoção da mensagem, ou entre o commit e a publicação de um evento. Nenhum dos casos pode
duplicar movimentação nem perder evento confirmado.

**Decisão.** Entrada: a inbox `(consumerName, messageId)` é gravada e concluída na mesma
transação da operação; a mensagem só é apagada depois do commit; reentrega devolve o
resultado gravado. Erros corrigíveis e mensagens inválidas vão para a DLQ com
`failureCode`; falhas transitórias voltam à fila com backoff de visibilidade, e o redrive
(5 recebimentos) leva à DLQ. Saída: eventos gravados na outbox no mesmo commit; um worker
reserva lotes com `SKIP LOCKED` e lease, publica com `MessageDeduplicationId = eventId` e
confirma depois; o lote de quem caiu volta quando o lease vence, com o mesmo `eventId`.

**Alternativa.** Publicar direto no SQS dentro do caso de uso: publicaria antes do commit
(eliminatório) ou perderia o evento numa queda entre o commit e o envio.
