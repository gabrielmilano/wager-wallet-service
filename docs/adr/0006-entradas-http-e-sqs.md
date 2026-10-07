# ADR 0006 — Entradas HTTP e SQS sobre o mesmo núcleo

- **Status:** aceito
- **Data:** 2026-10-07

## Contexto

HTTP e SQS devem compartilhar o caso de uso e as garantias de idempotência financeira.
A entrada por SQS tem uma obrigação a mais: o registro da inbox
(`consumerName`, `messageId`, hash) e sua conclusão precisam estar na **mesma transação
SQL** das alterações de domínio, do ledger e da outbox. O hash de idempotência deve
excluir a chave de idempotência e os metadados de transporte.

Duas formas foram avaliadas:

- **(A)** um único método `Process(ctx, cmd)`, com um campo opcional `Inbox` no
  `Command`;
- **(B)** dois métodos públicos sobre um núcleo privado comum:
  `Process(ctx, cmd)` e `ProcessFromMessage(ctx, cmd, msg)`.

## Decisão

**Opção (B).**

```go
// HTTP
func (s *Service) Process(ctx context.Context, cmd Command) (Result, error)

// SQS
func (s *Service) ProcessFromMessage(ctx context.Context, cmd Command, msg InboundMessage) (Result, error)
```

- Os dois abrem a transação com `TxRunner.WithinTx` e chamam o mesmo núcleo privado
  (`apply`), que calcula o hash canônico a partir do `Command`, aplica idempotência,
  regras e gravações.
- `ProcessFromMessage` envolve o núcleo, **dentro do mesmo callback**: registra a
  mensagem na inbox antes; se ela já existir, compara o hash da mensagem e devolve o
  resultado persistido; depois do núcleo, marca a mensagem como concluída.
- O worker de referências pendentes ganhará uma terceira entrada sobre o mesmo núcleo
  (Fase 10).

Justificativa:

1. **Metadado de transporte fora do `Command`.** O `Command` contém apenas campos de
   negócio, exatamente o que entra no hash. Com a opção (A), um dado de transporte
   moraria na mesma struct do que é hasheado, e um erro na seleção de campos o colocaria
   no hash, quebrando a equivalência entre HTTP e SQS.
2. **Garantia expressa no tipo.** Em `ProcessFromMessage`, a `InboundMessage` é
   obrigatória (não é ponteiro opcional): não existe caminho SQS sem inbox, nem caminho
   HTTP com inbox por engano, e o núcleo não precisa de `if cmd.Inbox != nil`.
3. **Equivalência preservada.** A regra de negócio e o hash continuam num único lugar,
   o núcleo `apply`. As entradas diferem apenas no que envolvem ao redor dele.

## Alternativas consideradas

- **(A) Campo `Inbox` opcional no `Command`:** um único método público, mas mistura
  transporte com negócio e espalha verificações de `nil` pelo fluxo.
- **Decorator no adapter SQS** abrindo a transação e chamando o núcleo com os
  repositórios: exigiria exportar o núcleo e deixaria o adapter orquestrar a transação,
  tirando regra de fluxo da camada `app`.

## Consequências

- Dois métodos públicos para testar. Os testes de equivalência HTTP↔SQS (mesma operação
  pelas duas entradas → um único efeito financeiro) exercitam o mesmo núcleo.
- A colisão entre uma entrega HTTP e uma SQS da mesma operação é resolvida pela unicidade
  de `(providerId, externalTransactionId)` no banco, não pela inbox.
- O cálculo do hash da mensagem da inbox e o tratamento de mensagens inválidas são
  detalhados na Fase 08.
