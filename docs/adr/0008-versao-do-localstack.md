# ADR 0008 — LocalStack fixado na última versão Community sem token

- **Status:** aceito
- **Data:** 2026-10-07

## Contexto

O desafio pede SQS executado localmente com LocalStack ou MiniStack e uma solução
reproduzível a partir de um clone limpo. A partir da versão 2026.03, a imagem
`localstack/localstack` passou a exigir um auth token para iniciar, mesmo no plano
gratuito (Hobby, de uso não comercial, com cadastro), e o repositório Community foi
arquivado.

Verificado em 2026-10-07: a tag `4.14.0` (publicada em 2026-02-26, digest
`sha256:3ebc3759…c364a`) é a última anterior à mudança. Ela inicia **sem token** e cria
filas FIFO com redrive para DLQ.

## Decisão

Usar `localstack/localstack:4.14.0`, fixada por tag **e** digest no
`docker-compose.yml`, com `SERVICES=sqs`.

**Saída documentada:** se a imagem antiga der problema nas Fases 08 ou 12 (por exemplo,
um comportamento de SQS FIFO ausente ou incorreto), a alternativa é o **MiniStack**, que
o enunciado aceita. A troca se limita à infraestrutura: o código da aplicação usa o SDK
da AWS com `AWS_ENDPOINT_URL`, então basta apontar essa variável para o novo emulador e
portar o script de criação das filas. Antes de trocar, é preciso verificar no MiniStack o
suporte a FIFO, `MessageGroupId`, `MessageDeduplicationId`, visibility timeout e redrive.

## Alternativas consideradas

- **Versão atual com `LOCALSTACK_AUTH_TOKEN` no `.env`:** recebe correções, mas quem
  avalia precisaria criar conta e gerar um token, e "uso não comercial" num desafio de
  contratação é uma zona cinzenta.
- **MiniStack desde já:** evita a imagem congelada, mas troca uma ferramenta conhecida
  por outra que ainda precisaria ser validada; a decisão anterior (decisão 8) era não
  adotá-lo por ora.
- **ElasticMQ:** emulador maduro de SQS, mas não está entre as opções citadas pelo
  enunciado.

## Consequências

- O clone limpo sobe sem cadastro nem token.
- A imagem está congelada: não recebe correções. Os testes de integração (filas, redrive
  e deduplicação FIFO) servem de alarme caso algo necessário não funcione.
- O LocalStack Community não aplica IAM; isso continua documentado como limitação
  (ADR 0003 e `deploy/aws/iam-policies.md`).
