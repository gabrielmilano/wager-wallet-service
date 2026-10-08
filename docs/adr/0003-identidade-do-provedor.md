# ADR 0003 — Identidade do provedor: token no HTTP, política do broker no SQS

- **Status:** aceito
- **Data:** 2026-10-07

## Contexto

O desafio exige que a identidade autenticada determine o `providerId` autorizado, que
provedores acessem apenas suas próprias transações (inclusive em replays) e que operações
de carteira sejam restritas ao serviço interno. O acesso à mensageria deve ser controlado
por credenciais e políticas do broker, preservando as validações de domínio no
consumidor. O IdP recomendado é o Keycloak, com `client_credentials`.

## Decisão

### HTTP

- Cada provedor é um client confidencial no Keycloak, que obtém tokens via
  `client_credentials`.
- O serviço valida o JWT localmente: assinatura pelas chaves do JWKS do realm, `iss`,
  `aud` (`wager-wallet-service`) e `exp`.
- O `providerId` vem de um **claim dedicado `provider_id`**, preenchido por um mapper
  fixo (hardcoded claim) no client de cada provedor. Um **audience mapper**, também
  configurado em cada client, coloca `wager-wallet-service` em `aud` (Fase 02).
- **Roles de realm** (não roles de client): `provider` (envia e consulta as próprias
  operações) e `wallet-admin` (serviço interno: abre carteiras, lê carteira e extrato,
  reconcilia). São atribuídas à service account de cada client e chegam no token em
  `realm_access.roles`, que é onde a aplicação as lê.
- O middleware HTTP coloca um `Principal{ProviderID, Roles}` no `context.Context`. Se o
  corpo trouxer um `providerId` diferente do token → `403 PROVIDER_FORBIDDEN`, sem gravar
  nada. Consultas por transação filtram pelo `providerId` do token: uma transação de
  outro provedor responde como inexistente (`404`), para não revelar sua existência
  (aprovado; implementado no bloco C).

### SQS

- A mensagem não carrega token; o `providerId` vem do corpo (`data.providerId`).
- A autorização é feita pela **política do broker**: só as credenciais IAM de um provedor
  podem enviar mensagens para a fila de entrada.
- **Na AWS real**, o consumidor leria o atributo de sistema `SenderId` da mensagem (a
  identidade IAM de quem enviou), mapearia para um `providerId` por configuração e
  recusaria a mensagem quando ele divergisse de `data.providerId`.
- **No LocalStack Community** as políticas IAM não são aplicadas; isso é uma limitação
  documentada. O `ARCHITECTURE.md` mostra as políticas que seriam usadas na AWS.
- As validações de domínio continuam no consumidor (carteira existe; jogador e moeda
  batem com a carteira; referência pertence ao mesmo provedor).

## Alternativas consideradas

- **Roles de client** (em `resource_access.<client>.roles`): permitiriam permissões
  diferentes por client consumidor, mas há um único serviço protegido e duas roles; roles
  de realm deixam o token e a leitura mais simples.
- **Usar `client_id`/`azp` como `providerId`:** dispensa o mapper, mas acopla a regra de
  negócio ao nome técnico do client e impede trocar o client sem trocar o provedor.
- **Introspecção do token no Keycloak a cada chamada:** detecta revogação imediata, mas
  adiciona uma chamada de rede por requisição e torna o IdP dependência síncrona.
- **Assinar a mensagem SQS (ex.: JWT dentro do envelope):** daria identidade por
  mensagem, mas o contrato de mensagem do desafio não prevê isso.
- **Filas por provedor:** isolamento forte no broker, mas o desafio define uma única
  fila de entrada.

## Consequências

- Revogação de token só tem efeito na expiração; usar tokens de vida curta.
- O `providerId` do corpo HTTP é redundante; ele só é comparado com o do token, nunca
  usado como fonte de verdade.
- A rotação de chaves do Keycloak exige recarregar o JWKS quando aparecer um `kid`
  desconhecido (Fase 07).
- No ambiente local, a segurança da entrada SQS depende da limitação do LocalStack; o
  documento deixa isso explícito em vez de simular uma garantia inexistente.
