.DEFAULT_GOAL := help

GO ?= go

.PHONY: help fmt fmt-check vet test test-race tidy check

help: ## Lista os alvos disponíveis
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-12s %s\n", $$1, $$2}'

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
