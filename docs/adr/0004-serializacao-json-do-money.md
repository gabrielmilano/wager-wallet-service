# ADR 0004 — Serialização JSON do Money no domínio

- **Status:** aceito
- **Data:** 2026-10-07

## Contexto

O desafio pede que `Money` suporte "criação a partir de string decimal, zero por moeda,
soma, subtração, negação, comparação e serialização", e que o contrato externo use
`{"amount":"25.00","currency":"BRL"}`. O mesmo formato aparece no HTTP, no envelope SQS
e nos eventos da outbox. Dinheiro não pode passar por `float32`/`float64` em nenhuma
etapa, inclusive no parsing e na serialização. Valores negativos são proibidos nas
entradas financeiras externas, mas permitidos em diferenças e cálculos internos (por
exemplo, `difference` da reconciliação).

## Decisão

`Money` implementa `json.Marshaler` e `json.Unmarshaler` no próprio pacote
`internal/domain/money`, usando apenas `encoding/json` da biblioteca padrão.

- **`MarshalJSON`** produz sempre o formato canônico, com duas casas e sinal quando
  negativo: `{"amount":"-5.00","currency":"BRL"}`.
- **`UnmarshalJSON` é estrito, mas aceita sinal:**
  - `amount` precisa ser string JSON; número JSON (`25.00` sem aspas), `null` ou outro
    tipo é recusado. Assim o valor nunca passa por um decodificador de ponto flutuante.
  - `amount` e `currency` são obrigatórios.
  - `amount` precisa estar no formato canônico (duas casas, sem notação científica,
    sem `NaN`/`Infinity`); a conversão usa o mesmo parser de `ParseMoney`.
  - O formato exato do sinal (por exemplo, se `+` ou `-0.00` são aceitos) é definido no
    plano da Fase 04.
- **A regra "não negativo para entrada externa" não fica no `Money`.** Ela é aplicada na
  construção do `wagering.Command` (e do comando de abertura de carteira), que recusa
  valores negativos com `VALIDATION_ERROR`.

## Alternativas consideradas

- **DTOs nos adapters** (`httpapi`, `sqs` e a serialização da outbox convertendo para
  `{amount, currency}` cada um): mantém o domínio livre do formato de transporte, mas
  cria três mapeamentos que precisam ficar idênticos, e qualquer divergência quebra a
  equivalência do hash de idempotência entre HTTP e SQS.
- **`UnmarshalJSON` recusando negativos:** coloca uma política de entrada externa dentro
  do value object e quebra o ida-e-volta (`Marshal` de uma diferença negativa não
  poderia ser lido de volta).

## Consequências

- Um único formato de dinheiro para HTTP, SQS e eventos, com uma única implementação e
  um único conjunto de testes.
- O domínio fica acoplado aos nomes de campo `amount` e `currency`. Como o formato faz
  parte do contrato do desafio, o acoplamento é aceito.
- Toda entrada externa precisa passar pela construção do `Command` para que a regra de
  não negatividade seja aplicada. Testes da Fase 06 cobrem valor negativo por HTTP e por
  SQS.
