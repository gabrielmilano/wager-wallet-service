# syntax=docker/dockerfile:1

# Etapa de build: mesma versão de Go declarada no go.mod.
FROM golang:1.27.1-trixie AS build
WORKDIR /src

# Dependências primeiro, para aproveitar o cache de camadas.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO desligado: binário estático, roda na imagem distroless sem libc.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wallet-service ./cmd/wallet-service

# Etapa final: só o binário, sem shell nem gerenciador de pacotes,
# executando como usuário não-root.
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/wallet-service /wallet-service
USER nonroot:nonroot
EXPOSE 8081
ENTRYPOINT ["/wallet-service"]
