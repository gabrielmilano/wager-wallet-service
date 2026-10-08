# ADR 0010 — Migrations embutidas no binário

- **Status:** aceito
- **Data:** 2026-10-07

## Contexto

O desafio exige migrations versionadas, com aplicação e reversão documentadas. A modelagem
define dois roles: `app_migrator` (dono do schema, só para migrations) e `app_runtime`
(usado pela aplicação). Várias instâncias da aplicação podem subir ao mesmo tempo.

## Decisão

- Arquivos SQL do golang-migrate em `migrations/` (`NNNNNN_nome.up.sql` e
  `.down.sql`), embutidos no binário com `go:embed` (pacote `migrations`).
- O binário ganha o subcomando:

  ```
  wallet-service migrate up | down [N] | version | force V
  ```

  - `up`: aplica tudo o que estiver pendente;
  - `down [N]`: reverte as últimas N (padrão 1, nunca "tudo" por omissão);
  - `version`: mostra a versão e se está *dirty*;
  - `force V`: marca V como aplicada e limpa o estado *dirty*, sem executar SQL. É a
    recuperação depois de uma migration que falhou no meio, **após** conferir e corrigir
    o banco à mão.
- Conecta com `MIGRATIONS_DATABASE_URL` (role `app_migrator`) pelo driver pgx v5 do
  golang-migrate. A aplicação nunca usa esse role.
- No Compose, um serviço `migrate` (mesma imagem da app) roda `migrate up` e termina; a
  app depende de `service_completed_successfully`. No Makefile: `migrate-up`,
  `migrate-down N=…`, `migrate-version`, `migrate-force V=…`.
- Cada migration é uma tabela com seus índices, triggers e GRANTs, e o `down`
  correspondente desfaz tudo isso (verificado por teste).

## Alternativas consideradas

- **CLI oficial (`migrate/migrate`) no Compose:** sem código Go, mas é mais uma imagem,
  com versão independente da aplicação; os testes não poderiam aplicar e reverter
  migrations direto do Go.
- **Aplicar migrations no início da própria aplicação:** acopla a inicialização de cada
  instância à evolução do schema e exigiria o role `app_migrator` na aplicação.

## Consequências

- Um único artefato: a versão do schema esperada pelo código está no próprio binário.
- O golang-migrate usa advisory lock no PostgreSQL, então dois `migrate up` simultâneos
  não se atropelam.
- Os testes de integração aplicam e revertem migrations num banco temporário
  (`TestMigrationsRoundTrip`), provando que todo `down` desfaz o seu `up`.
- `migrations/` não pode ficar fora do contexto de build do Docker.
