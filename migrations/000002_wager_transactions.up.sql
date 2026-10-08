-- Protocolo de cada operação: base da idempotência e da máquina de estados.
-- OPENING é interna (sem dados de provedor); os demais tipos são externos.

CREATE TABLE wager_transactions (
    id                       UUID        NOT NULL,
    origin                   TEXT        NOT NULL,
    kind                     TEXT        NOT NULL,
    status                   TEXT        NOT NULL,
    wallet_id                UUID        NOT NULL,
    player_id                UUID        NOT NULL,
    currency                 CHAR(3)     NOT NULL,
    amount_minor             BIGINT      NOT NULL,
    -- Só para operações externas.
    provider_id              TEXT,
    external_transaction_id  TEXT,
    idempotency_key          TEXT,
    payload_hash             BYTEA,
    round_id                 TEXT,
    game_id                  TEXT,
    reference_external_id    TEXT,
    reference_transaction_id UUID,
    -- Resposta guardada para o replay.
    failure_code             TEXT,
    result_balance_minor     BIGINT,
    -- Retentativas de PENDING_REFERENCE.
    attempts                 INT         NOT NULL DEFAULT 0,
    next_attempt_at          TIMESTAMPTZ,
    expires_at               TIMESTAMPTZ,
    created_at               TIMESTAMPTZ NOT NULL,
    updated_at               TIMESTAMPTZ NOT NULL,
    completed_at             TIMESTAMPTZ,

    CONSTRAINT wager_transactions_pkey PRIMARY KEY (id),
    -- Alvo da FK composta do extrato (transaction_id, wallet_id).
    CONSTRAINT wager_transactions_id_wallet_key UNIQUE (id, wallet_id),
    -- Moeda e jogador precisam ser os da carteira.
    CONSTRAINT wager_tx_wallet_fkey FOREIGN KEY (wallet_id, player_id, currency)
        REFERENCES wallets (id, player_id, currency),
    CONSTRAINT wager_tx_reference_fkey FOREIGN KEY (reference_transaction_id)
        REFERENCES wager_transactions (id),

    CONSTRAINT wager_tx_origin_valid CHECK (origin IN ('INTERNAL', 'EXTERNAL')),
    CONSTRAINT wager_tx_kind_valid CHECK (kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
    CONSTRAINT wager_tx_status_valid CHECK (status IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')),

    -- OPENING é interno e não tem dados de provedor; o resto é externo e completo.
    CONSTRAINT wager_tx_origin_shape CHECK (
        (origin = 'INTERNAL' AND kind = 'OPENING'
            AND num_nonnulls(provider_id, external_transaction_id, idempotency_key, payload_hash,
                             round_id, game_id, reference_external_id, reference_transaction_id) = 0)
        OR
        (origin = 'EXTERNAL' AND kind <> 'OPENING'
            AND num_nulls(provider_id, external_transaction_id, idempotency_key, payload_hash,
                          round_id, game_id) = 0)
    ),
    -- LOSS vale exatamente zero; todo o resto precisa ser maior que zero.
    CONSTRAINT wager_tx_amount_by_kind CHECK (
        (kind = 'LOSS' AND amount_minor = 0) OR (kind <> 'LOSS' AND amount_minor > 0)
    ),
    -- Reversões sempre dizem o que revertem.
    CONSTRAINT wager_tx_reference_required CHECK (
        kind NOT IN ('REFUND', 'ROLLBACK') OR reference_external_id IS NOT NULL
    ),
    -- Código de falha existe se e somente se a operação foi recusada ou falhou.
    CONSTRAINT wager_tx_failure_code_coherent CHECK (
        (status IN ('REJECTED', 'FAILED')) = (failure_code IS NOT NULL)
    ),
    CONSTRAINT wager_tx_attempts_non_negative CHECK (attempts >= 0),
    CONSTRAINT wager_tx_result_balance_non_negative CHECK (
        result_balance_minor IS NULL OR result_balance_minor >= 0
    ),
    -- completed_at existe se e somente se o estado é terminal.
    CONSTRAINT wager_tx_completed_at_coherent CHECK (
        (status IN ('PROCESSED', 'REJECTED', 'FAILED')) = (completed_at IS NOT NULL)
    ),
    -- Quem espera referência precisa de agenda de retentativa e de prazo.
    CONSTRAINT wager_tx_pending_reference_schedule CHECK (
        status <> 'PENDING_REFERENCE' OR (next_attempt_at IS NOT NULL AND expires_at IS NOT NULL)
    ),
    CONSTRAINT wager_tx_no_self_reference CHECK (
        reference_transaction_id IS NULL OR reference_transaction_id <> id
    )
);

-- A mesma operação do provedor nunca é aplicada duas vezes, nem com outra chave.
CREATE UNIQUE INDEX ux_tx_provider_external
    ON wager_transactions (provider_id, external_transaction_id) WHERE origin = 'EXTERNAL';

-- A chave de idempotência vale por provedor.
CREATE UNIQUE INDEX ux_tx_provider_idem_key
    ON wager_transactions (provider_id, idempotency_key) WHERE origin = 'EXTERNAL';

-- No máximo um crédito inicial por carteira.
CREATE UNIQUE INDEX ux_tx_single_opening
    ON wager_transactions (wallet_id) WHERE kind = 'OPENING';

-- No máximo uma reversão bem-sucedida (REFUND ou ROLLBACK) por operação original.
CREATE UNIQUE INDEX ux_tx_single_reversal
    ON wager_transactions (reference_transaction_id)
    WHERE status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK');

-- Fila do worker de referências: só PENDING_REFERENCE (PENDING nunca é confirmado).
CREATE INDEX ix_tx_pending_due
    ON wager_transactions (next_attempt_at) WHERE status = 'PENDING_REFERENCE';

-- Acordar reversões que esperam por uma operação que acabou de chegar.
CREATE INDEX ix_tx_waiting_reference
    ON wager_transactions (provider_id, reference_external_id) WHERE status = 'PENDING_REFERENCE';

-- Listar operações de uma carteira.
CREATE INDEX ix_tx_wallet ON wager_transactions (wallet_id, created_at);

-- E4: máquina de estados, congelamento dos estados terminais e dos campos de
-- negócio, e proibição de DELETE. Vale para qualquer role.
CREATE FUNCTION wager_transactions_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'operação % não pode ser apagada', OLD.id
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'wager_tx_append_only';
    END IF;

    IF TG_OP = 'INSERT' THEN
        IF NEW.origin = 'EXTERNAL' AND NEW.status <> 'PENDING' THEN
            RAISE EXCEPTION 'operação externa deve nascer PENDING (recebido %)', NEW.status
                USING ERRCODE = 'check_violation', CONSTRAINT = 'wager_tx_transition';
        END IF;
        IF NEW.origin = 'INTERNAL' AND NEW.status <> 'PROCESSED' THEN
            RAISE EXCEPTION 'OPENING deve nascer PROCESSED (recebido %)', NEW.status
                USING ERRCODE = 'check_violation', CONSTRAINT = 'wager_tx_transition';
        END IF;
        RETURN NEW;
    END IF;

    -- UPDATE
    IF OLD.status IN ('PROCESSED', 'REJECTED', 'FAILED') THEN
        RAISE EXCEPTION 'operação % está em estado terminal (%) e não muda mais', OLD.id, OLD.status
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'wager_tx_terminal';
    END IF;

    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.origin IS DISTINCT FROM OLD.origin
        OR NEW.kind IS DISTINCT FROM OLD.kind
        OR NEW.wallet_id IS DISTINCT FROM OLD.wallet_id
        OR NEW.player_id IS DISTINCT FROM OLD.player_id
        OR NEW.currency IS DISTINCT FROM OLD.currency
        OR NEW.amount_minor IS DISTINCT FROM OLD.amount_minor
        OR NEW.provider_id IS DISTINCT FROM OLD.provider_id
        OR NEW.external_transaction_id IS DISTINCT FROM OLD.external_transaction_id
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.payload_hash IS DISTINCT FROM OLD.payload_hash
        OR NEW.round_id IS DISTINCT FROM OLD.round_id
        OR NEW.game_id IS DISTINCT FROM OLD.game_id
        OR NEW.reference_external_id IS DISTINCT FROM OLD.reference_external_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'campos de negócio da operação % são imutáveis', OLD.id
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'wager_tx_immutable';
    END IF;

    IF NOT (
        (OLD.status = 'PENDING'
            AND NEW.status IN ('PENDING_REFERENCE', 'PROCESSED', 'REJECTED'))
        OR (OLD.status = 'PENDING_REFERENCE'
            AND NEW.status IN ('PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED'))
    ) THEN
        RAISE EXCEPTION 'transição inválida da operação %: % -> %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'check_violation', CONSTRAINT = 'wager_tx_transition';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER wager_transactions_guard
    BEFORE INSERT OR UPDATE OR DELETE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION wager_transactions_guard();

CREATE FUNCTION wager_transactions_no_truncate() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'wager_transactions não pode ser truncada'
        USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'wager_tx_append_only';
END;
$$;

CREATE TRIGGER wager_transactions_no_truncate
    BEFORE TRUNCATE ON wager_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION wager_transactions_no_truncate();

-- E3: PENDING só existe dentro da transação SQL; nunca chega ao commit.
-- Trigger diferido: roda no commit e relê a linha atual, porque o NEW do
-- evento que o enfileirou pode já estar desatualizado.
CREATE FUNCTION wager_transactions_not_pending_at_commit() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    current_status TEXT;
BEGIN
    SELECT status INTO current_status FROM wager_transactions WHERE id = NEW.id;
    IF current_status = 'PENDING' THEN
        RAISE EXCEPTION 'operação % não pode ser confirmada em PENDING', NEW.id
            USING ERRCODE = 'check_violation', CONSTRAINT = 'wager_tx_pending_not_committed';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER wager_transactions_not_pending_at_commit
    AFTER INSERT OR UPDATE ON wager_transactions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION wager_transactions_not_pending_at_commit();

-- app_runtime: sem DELETE/TRUNCATE; UPDATE só nas colunas de estado e retentativa.
GRANT SELECT, INSERT ON wager_transactions TO app_runtime;
GRANT UPDATE (status, reference_transaction_id, failure_code, result_balance_minor,
              attempts, next_attempt_at, expires_at, updated_at, completed_at)
    ON wager_transactions TO app_runtime;
