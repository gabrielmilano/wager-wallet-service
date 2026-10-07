.DEFAULT_GOAL := help

GO ?= go
COMPOSE ?= docker compose

.PHONY: help fmt fmt-check vet test test-race tidy check up down clean logs ps test-integration

help: ## Lista os alvos disponíveis
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-18s %s\n", $$1, $$2}'

fmt: ## Formata o código com gofmt
	gofmt -w .

fmt-check: ## Falha se algum arquivo não estiver formatado
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "Arquivos sem gofmt:"; echo "$$out"; exit 1; fi

vet: ## Executa go vet
	$(GO) vet ./...

test: ## Executa os testes
	$(GO) test ./...

test-race: ## Executa os testes com o race detector (exige cgo/gcc)
	CGO_ENABLED=1 $(GO) test -race ./...

tidy: ## Sincroniza go.mod e go.sum
	$(GO) mod tidy

check: fmt-check vet test-race ## Verificações exigidas antes de abrir PR

up: ## Sobe o ambiente (build da app) e espera os healthchecks
	$(COMPOSE) up -d --build --wait

down: ## Derruba o ambiente, preservando os volumes
	$(COMPOSE) down

clean: ## Derruba o ambiente e apaga os volumes (o initdb do Postgres roda de novo)
	$(COMPOSE) down -v --remove-orphans

logs: ## Acompanha os logs de todos os serviços
	$(COMPOSE) logs -f

ps: ## Mostra o estado dos serviços
	$(COMPOSE) ps

test-integration: ## Testes de integração contra o ambiente do Compose (exige make up)
	CGO_ENABLED=1 $(GO) test -race -count=1 -tags=integration ./...
