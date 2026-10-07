# ADR 0009 — Validação OIDC sem discovery: JWKS direto e iss como texto

- **Status:** aceito
- **Data:** 2026-10-07

## Contexto

O `iss` de um token emitido pelo Keycloak depende, por padrão, do endereço usado para
pedi-lo. No ambiente local há dois endereços para o mesmo Keycloak:

- `http://localhost:8180`, usado pelos provedores e testes rodando no host;
- `http://keycloak:8080`, o nome do serviço dentro da rede do Docker, usado pela app.

Se a app usasse OIDC discovery (`/.well-known/openid-configuration`) a partir de
`keycloak:8080`, o issuer anunciado não bateria com o `iss` dos tokens pedidos de fora, e
toda validação falharia. Usar `localhost:8180` de dentro do container não funciona,
porque ali `localhost` é o próprio container.

## Decisão

1. **O Keycloak tem o hostname fixo:** `KC_HOSTNAME=http://localhost:8180`. Todo token,
   pedido de fora ou de dentro da rede, sai com
   `iss = http://localhost:8180/realms/wager`. `KC_HOSTNAME_BACKCHANNEL_DYNAMIC=true`
   permite que chamadas de backchannel (token e JWKS) sejam atendidas pelo endereço
   interno.
2. **A app não usa discovery.** Ela recebe duas configurações independentes:
   - `OIDC_ISSUER_URL`: o valor esperado do claim `iss`, comparado como **texto exato**;
   - `OIDC_JWKS_URL`: de onde buscar as chaves públicas
     (`http://keycloak:8080/realms/wager/protocol/openid-connect/certs` no Compose).
3. `aud` deve conter `OIDC_AUDIENCE` (`wager-wallet-service`); `exp` é obrigatório. A
   implementação é da Fase 07.

Verificado em 2026-10-07: um token pedido com o header `Host: keycloak:8080` sai com o
mesmo `iss` de um pedido a `localhost:8180`.

## Alternativas consideradas

- **Discovery com o issuer interno:** quebra a validação de tokens pedidos de fora da
  rede.
- **Aceitar uma lista de issuers:** funciona, mas afrouxa a validação e deixa o mesmo
  realm com duas identidades.
- **Rodar a app com `network_mode: host` ou um alias de `localhost`:** depende do
  sistema operacional e foge do comportamento de produção.

## Consequências

- Em produção as duas URLs seriam o mesmo endereço público do IdP, e a separação continua
  válida sem mudar código.
- Sem discovery, a rotação de chaves depende de a app recarregar o JWKS quando aparecer um
  `kid` desconhecido (Fase 07).
- Um erro de configuração (issuer com barra final, porta errada) faz todo token ser
  recusado; o teste de integração de tokens confere o `iss` exato.
