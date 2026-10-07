# ADR 0007 — Testes de integração contra o Docker Compose

- **Status:** aceito
- **Data:** 2026-10-07

## Contexto

O desafio exige testes de integração com PostgreSQL, IdP e LocalStack/MiniStack em
containers reais. Mocks substituindo essa infraestrutura são eliminatórios. A Fase 12
precisa, além disso, de pelo menos três instâncias independentes da aplicação disputando
o mesmo banco e as mesmas filas.

## Decisão

Os testes de integração rodam contra o ambiente do `docker-compose.yml`, o mesmo usado em
desenvolvimento:

```sh
make up                # sobe e espera os healthchecks
make test-integration  # go test -race -count=1 -tags=integration ./...
```

- Os testes ficam atrás da build tag `integration`; `go test ./...` sem a tag não precisa
  de Docker.
- Os endereços vêm das mesmas variáveis de ambiente da aplicação, com padrões iguais aos
  do `.env.example`, então rodam sem `.env`.
- O estado é compartilhado entre execuções. Cada teste cria seus próprios dados (IDs
  únicos, filas temporárias, transações desfeitas) e não depende de banco vazio nem de
  ordem de execução.

## Alternativas consideradas

- **testcontainers-go:** cada pacote de teste sobe seus próprios containers, com
  isolamento total. Em contrapartida:
  - o provisionamento (realm do Keycloak, script das filas, roles do PostgreSQL) teria
    de ser duplicado em Go ou montado a partir de `deploy/`, com dois caminhos para
    manter iguais;
  - o Keycloak leva cerca de 30 s para ficar pronto, e subir um por pacote de teste
    tornaria a suíte lenta;
  - os testes de múltiplas instâncias da Fase 12 precisariam do Compose de qualquer
    forma.
- **Banco e filas embutidos ou em memória:** violam o requisito de containers reais.

## Consequências

- `make up` é pré-requisito explícito, documentado no README; sem ele, os testes falham
  com mensagem indicando o comando.
- O ambiente testado é o mesmo do desenvolvimento e da demonstração; uma divergência de
  configuração aparece nos testes.
- Como os testes compartilham estado, eles não podem usar `TRUNCATE` nem contar linhas
  globais; as asserções são sempre sobre os dados que o próprio teste criou.
- Na CI, o pipeline executaria `make up` antes de `make test-integration`.
