// Package platform agrupa as peças transversais: configuração, logs,
// métricas e health checks.
//
// Regra de dependência: platform importa apenas a biblioteca padrão e
// bibliotecas de terceiros; nunca domain, app, adapter ou bootstrap.
package platform
