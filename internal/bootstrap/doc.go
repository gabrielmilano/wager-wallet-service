// Package bootstrap faz a composição da aplicação com Uber Fx: fx.Module por
// área, fx.Provide dos construtores e fx.Invoke/fx.Lifecycle para iniciar e
// encerrar servidor, consumidor e workers.
//
// É o único pacote (além de cmd/) que importa Fx (ADR 0001).
//
// Na Fase 02 compõe config, logger e o servidor HTTP; os demais componentes
// entram nas fases seguintes.
package bootstrap
