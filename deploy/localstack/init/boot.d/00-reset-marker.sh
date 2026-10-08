#!/bin/bash
# Roda na inicialização do LocalStack, antes do ready.d. Apaga a marca de
# "filas criadas" de uma execução anterior: depois de um restart, o
# healthcheck só volta a passar quando o script de filas terminar de novo.
rm -f /tmp/queues-ready
