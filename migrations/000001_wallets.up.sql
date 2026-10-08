-- Carteira: raiz do agregado financeiro. Saldo em unidades mínimas (BIGINT).
-- IDs e timestamps são fornecidos pela aplicação (UUIDv7 e Clock).

CREATE TABLE wallets (
    id            UUID        NOT NULL,
    player_id     UUID        NOT NULL,
    currency      CHAR(3)     NOT NULL,
    balance_minor BIGINT      NOT NULL,
    version       BIGINT      NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL,

    CONSTRAINT wallets_pkey PRIMARY KEY (id),
    -- Lista fechada, igual à do Money.
    CONSTRAINT wallets_currency_supported CHECK (currency IN ('BRL', 'USD', 'EUR')),
    CONSTRAINT wallets_balance_non_negative CHECK (balance_minor >= 0),
    CONSTRAINT wallets_version_positive CHECK (version >= 1),
    -- Uma carteira por jogador e moeda.
    CONSTRAINT wallets_player_currency_key UNIQUE (player_id, currency),
    -- Alvos das FKs compostas: o extrato aponta para (id, currency) e as
    -- operações para (id, player_id, currency).
    CONSTRAINT wallets_id_currency_key UNIQUE (id, currency),
    CONSTRAINT wallets_id_player_currency_key UNIQUE (id, player_id, currency)
);

-- E5: a carteira nasce na versão 1; a versão sobe exatamente 1 quando, e só
-- quando, o saldo muda; a identidade nunca muda. Vale para qualquer role.
CREATE FUNCTION wallets_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.version <> 1 THEN
            RAISE EXCEPTION 'carteira % deve nascer na versão 1 (recebida %)', NEW.id, NEW.version
                USING ERRCODE = 'check_violation', CONSTRAINT = 'wallets_version_rule';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.player_id IS DISTINCT FROM OLD.player_id
        OR NEW.currency IS DISTINCT FROM OLD.currency
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'identidade da carteira % é imutável', OLD.id
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'wallets_identity_immutable';
    END IF;

    IF NEW.balance_minor <> OLD.balance_minor AND NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'carteira %: saldo mudou, versão deveria ir de % para %', OLD.id, OLD.version, OLD.version + 1
            USING ERRCODE = 'check_violation', CONSTRAINT = 'wallets_version_rule';
    END IF;
    IF NEW.balance_minor = OLD.balance_minor AND NEW.version <> OLD.version THEN
        RAISE EXCEPTION 'carteira %: versão só muda junto com o saldo', OLD.id
            USING ERRCODE = 'check_violation', CONSTRAINT = 'wallets_version_rule';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER wallets_guard
    BEFORE INSERT OR UPDATE ON wallets
    FOR EACH ROW EXECUTE FUNCTION wallets_guard();

-- app_runtime: sem DELETE/TRUNCATE; UPDATE só nas colunas que mudam com o saldo.
GRANT SELECT, INSERT ON wallets TO app_runtime;
GRANT UPDATE (balance_minor, version, updated_at) ON wallets TO app_runtime;
