#!/bin/bash
# Cria as filas SQS no LocalStack quando ele fica pronto (hook ready.d).
#
#   wager-transactions-dlq.fifo  DLQ da entrada
#   wager-transactions.fifo      entrada de operações, com redrive para a DLQ
#   wallet-events.fifo           eventos de saída publicados pela outbox
#
# Valores provisórios, revistos na Fase 08: visibility timeout de 30 s e
# 5 recebimentos antes de mover a mensagem para a DLQ.
# ContentBasedDeduplication=false: quem publica informa o
# MessageDeduplicationId explicitamente.
set -euo pipefail

INPUT_QUEUE="${SQS_INPUT_QUEUE_NAME:-wager-transactions.fifo}"
INPUT_DLQ="${SQS_INPUT_DLQ_NAME:-wager-transactions-dlq.fifo}"
EVENTS_QUEUE="${SQS_EVENTS_QUEUE_NAME:-wallet-events.fifo}"
VISIBILITY_TIMEOUT="${SQS_INPUT_VISIBILITY_TIMEOUT:-30}"
MAX_RECEIVE_COUNT="${SQS_INPUT_MAX_RECEIVE_COUNT:-5}"

create_queue() {
	awslocal sqs create-queue --queue-name "$1" --attributes "$2" \
		--query QueueUrl --output text
}

dlq_url=$(create_queue "$INPUT_DLQ" \
	'{"FifoQueue":"true","ContentBasedDeduplication":"false","MessageRetentionPeriod":"1209600"}')
dlq_arn=$(awslocal sqs get-queue-attributes --queue-url "$dlq_url" \
	--attribute-names QueueArn --query Attributes.QueueArn --output text)

redrive=$(printf '{\\"deadLetterTargetArn\\":\\"%s\\",\\"maxReceiveCount\\":\\"%s\\"}' "$dlq_arn" "$MAX_RECEIVE_COUNT")
create_queue "$INPUT_QUEUE" \
	"{\"FifoQueue\":\"true\",\"ContentBasedDeduplication\":\"false\",\"VisibilityTimeout\":\"$VISIBILITY_TIMEOUT\",\"RedrivePolicy\":\"$redrive\"}" >/dev/null

create_queue "$EVENTS_QUEUE" \
	'{"FifoQueue":"true","ContentBasedDeduplication":"false"}' >/dev/null

# Marca de pronto lida pelo healthcheck do Compose; só é criada se todos os
# comandos acima deram certo (set -e).
touch /tmp/queues-ready
echo "filas criadas: $INPUT_DLQ, $INPUT_QUEUE, $EVENTS_QUEUE"
