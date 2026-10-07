# Políticas IAM para o SQS (AWS real)

O LocalStack Community não aplica políticas IAM: no ambiente local, qualquer credencial
consegue enviar para qualquer fila. Este documento mostra as políticas que controlariam o
acesso à mensageria em uma conta AWS real (ADR 0003, decisão 8). Nada aqui é aplicado
pelo `docker compose`.

Contas e nomes usados nos exemplos (fictícios):

| Identidade | Conta | Papel |
| --- | --- | --- |
| `arn:aws:iam::111111111111:role/wager-wallet-service` | conta do serviço | consome a entrada e publica eventos |
| `arn:aws:iam::222222222222:role/provider-a-sender` | conta do provedor A | envia operações |
| `arn:aws:iam::333333333333:role/provider-b-sender` | conta do provedor B | envia operações |

Filas na conta `111111111111`, região `us-east-1`: `wager-transactions.fifo`,
`wager-transactions-dlq.fifo` e `wallet-events.fifo`.

## 1. Política da fila de entrada (resource-based)

Só os roles dos provedores conhecidos podem enviar mensagens; só o serviço pode consumir.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ProvidersSendOnly",
      "Effect": "Allow",
      "Principal": {
        "AWS": [
          "arn:aws:iam::222222222222:role/provider-a-sender",
          "arn:aws:iam::333333333333:role/provider-b-sender"
        ]
      },
      "Action": "sqs:SendMessage",
      "Resource": "arn:aws:sqs:us-east-1:111111111111:wager-transactions.fifo"
    },
    {
      "Sid": "ServiceConsumes",
      "Effect": "Allow",
      "Principal": { "AWS": "arn:aws:iam::111111111111:role/wager-wallet-service" },
      "Action": [
        "sqs:ReceiveMessage",
        "sqs:DeleteMessage",
        "sqs:ChangeMessageVisibility",
        "sqs:GetQueueAttributes",
        "sqs:GetQueueUrl"
      ],
      "Resource": "arn:aws:sqs:us-east-1:111111111111:wager-transactions.fifo"
    }
  ]
}
```

## 2. Política do role de cada provedor (identity-based)

Na conta do provedor A (o provedor B é igual, com o próprio role):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["sqs:SendMessage", "sqs:GetQueueUrl"],
      "Resource": "arn:aws:sqs:us-east-1:111111111111:wager-transactions.fifo"
    }
  ]
}
```

## 3. Política do role do serviço (identity-based)

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ConsumeInput",
      "Effect": "Allow",
      "Action": [
        "sqs:ReceiveMessage",
        "sqs:DeleteMessage",
        "sqs:ChangeMessageVisibility",
        "sqs:GetQueueAttributes",
        "sqs:GetQueueUrl"
      ],
      "Resource": "arn:aws:sqs:us-east-1:111111111111:wager-transactions.fifo"
    },
    {
      "Sid": "MoveInvalidToDLQ",
      "Effect": "Allow",
      "Action": ["sqs:SendMessage", "sqs:GetQueueUrl"],
      "Resource": "arn:aws:sqs:us-east-1:111111111111:wager-transactions-dlq.fifo"
    },
    {
      "Sid": "PublishEvents",
      "Effect": "Allow",
      "Action": ["sqs:SendMessage", "sqs:GetQueueUrl"],
      "Resource": "arn:aws:sqs:us-east-1:111111111111:wallet-events.fifo"
    }
  ]
}
```

O statement `MoveInvalidToDLQ` só é necessário se a Fase 08 optar por mover mensagens
inválidas para a DLQ explicitamente, em vez de esperar o redrive por `maxReceiveCount`.

## 4. Do `SenderId` ao `providerId`

A política da fila garante que só provedores conhecidos enviam, mas sozinha não impede o
provedor A de enviar uma mensagem com `"providerId": "provider-b"` no corpo. Na AWS real,
o consumidor fecharia essa brecha assim:

1. Pede o atributo de sistema `SenderId` no `ReceiveMessage`
   (`MessageSystemAttributeNames: ["SenderId"]`). Para um role assumido, o valor tem a
   forma `AROAEXEMPLOPROVIDERA:nome-da-sessao` (ID único do role + sessão).
2. Mapeia o ID único do role (a parte antes de `:`) para um `providerId` por
   configuração, por exemplo
   `SQS_SENDER_PROVIDERS=AROAEXEMPLOPROVIDERA=provider-a,AROAEXEMPLOPROVIDERB=provider-b`.
3. Se o `SenderId` não estiver no mapa, ou o `providerId` mapeado for diferente de
   `data.providerId`, a mensagem é recusada como entrada corrigível
   (`PROVIDER_FORBIDDEN`) e vai para a DLQ, sem efeito financeiro.

No LocalStack Community o `SenderId` não reflete uma identidade IAM real, então esse
mapeamento **não é implementado nem testado** neste projeto. É uma limitação documentada
no `ARCHITECTURE.md`.
