#!/bin/sh
# Cria o banco da aplicação e os dois roles (decisão da modelagem):
#   app_migrator: dono do schema e das tabelas; usado só pelas migrations.
#   app_runtime:  usado pela aplicação; as permissões nas tabelas são dadas
#                 pelas migrations (Fase 03).
#
# Roda apenas na primeira inicialização, com o volume de dados vazio
# (comportamento do initdb da imagem oficial). Para rodar de novo: make clean.
set -eu

: "${APP_MIGRATOR_PASSWORD:?defina APP_MIGRATOR_PASSWORD}"
: "${APP_RUNTIME_PASSWORD:?defina APP_RUNTIME_PASSWORD}"

# As senhas entram como variáveis do psql (:'nome'), que o psql escapa como
# literal SQL; nada é concatenado no texto do comando.
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
	-v migrator_password="$APP_MIGRATOR_PASSWORD" \
	-v runtime_password="$APP_RUNTIME_PASSWORD" <<'SQL'
CREATE ROLE app_migrator LOGIN PASSWORD :'migrator_password';
CREATE ROLE app_runtime  LOGIN PASSWORD :'runtime_password';

CREATE DATABASE wager_wallet OWNER app_migrator;
REVOKE ALL ON DATABASE wager_wallet FROM PUBLIC;
GRANT CONNECT ON DATABASE wager_wallet TO app_migrator, app_runtime;
SQL

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname wager_wallet <<'SQL'
-- O schema public passa a ser do app_migrator; ninguém mais cria objetos nele.
ALTER SCHEMA public OWNER TO app_migrator;
REVOKE ALL ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO app_runtime;
SQL
