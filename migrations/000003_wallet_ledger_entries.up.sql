-- Extrato append-only: cada linha é uma movimentação, com saldo antes e depois.
-- Correções financeiras são novos lançamentos, nunca edição.

CREATE TABLE wallet_ledger_entries (
    id                   UUID        NOT NULL,
    wallet_id            UUID        NOT NULL,
    transaction_id       UUID        NOT NULL,
    -- Versão da carteira depois deste lançamento; também é o cursor da paginação.
    wallet_version       BIGINT      NOT NULL,
    direction            TEXT        NOT NULL,
    amount_minor         BIGINT      NOT NULL,
    currency             CHAR(3)     NOT NULL,
    balance_before_minor BIGINT      NOT NULL,
    balance_after_minor  BIGINT      NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL,

    CONSTRAINT wallet_ledger_entries_pkey PRIMARY KEY (id),
    CONSTRAINT ledger_wallet_currency_fkey FOREIGN KEY (wallet_id, currency)
        REFERENCES wallets (id, currency),
    -- A operação lançada pertence à mesma carteira (correção 5).
    CONSTRAINT ledger_transaction_wallet_fkey FOREIGN KEY (transaction_id, wallet_id)
        REFERENCES wager_transactions (id, wallet_id),
    -- Uma operação gera no máximo um lançamento.
    CONSTRAINT ledger_wallet_transaction_key UNIQUE (wallet_id, transaction_id),
    -- Versões sem repetição por carteira: é esta constraint, com o FOR UPDATE na
    -- carteira, que garante a sequência sob concorrência (ADR 0011).
    CONSTRAINT ledger_wallet_version_key UNIQUE (wallet_id, wallet_version),

    CONSTRAINT ledger_direction_valid CHECK (direction IN ('DEBIT', 'CREDIT')),
    CONSTRAINT ledger_amount_positive CHECK (amount_minor > 0),
    CONSTRAINT ledger_wallet_version_positive CHECK (wallet_version >= 1),
    CONSTRAINT ledger_balance_before_non_negative CHECK (balance_before_minor >= 0),
    CONSTRAINT ledger_balance_after_non_negative CHECK (balance_after_minor >= 0),
    CONSTRAINT ledger_balance_arithmetic CHECK (
        (direction = 'CREDIT' AND balance_after_minor = balance_before_minor + amount_minor)
        OR (direction = 'DEBIT' AND balance_after_minor = balance_before_minor - amount_minor)
    )
);

-- Append-only: UPDATE, DELETE e TRUNCATE são recusados para qualquer role.
CREATE FUNCTION ledger_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'o extrato é append-only: % não é permitido', TG_OP
        USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'ledger_append_only';
END;
$$;

CREATE TRIGGER ledger_append_only
    BEFORE UPDATE OR DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_append_only();

CREATE TRIGGER ledger_no_truncate
    BEFORE TRUNCATE ON wallet_ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_append_only();

-- E2 (parte imediata): o lançamento corresponde à sua operação: mesmo valor e
-- moeda, e a direção que o tipo exige. ROLLBACK é o oposto do original.
CREATE FUNCTION ledger_matches_transaction() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    tx_kind      TEXT;
    tx_currency  CHAR(3);
    tx_amount    BIGINT;
    tx_reference UUID;
    original     TEXT;
    expected     TEXT;
BEGIN
    SELECT kind, currency, amount_minor, reference_transaction_id
      INTO tx_kind, tx_currency, tx_amount, tx_reference
      FROM wager_transactions
     WHERE id = NEW.transaction_id AND wallet_id = NEW.wallet_id;
    IF NOT FOUND THEN
        -- A FK composta ledger_transaction_wallet_fkey reporta o erro.
        RETURN NEW;
    END IF;

    IF NEW.amount_minor <> tx_amount OR NEW.currency <> tx_currency THEN
        RAISE EXCEPTION 'lançamento difere da operação %: valor %/% moeda %/%',
            NEW.transaction_id, NEW.amount_minor, tx_amount, NEW.currency, tx_currency
            USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_matches_transaction';
    END IF;

    IF tx_kind = 'ROLLBACK' THEN
        SELECT kind INTO original FROM wager_transactions WHERE id = tx_reference;
        expected := CASE original
            WHEN 'BET' THEN 'CREDIT'
            WHEN 'WIN' THEN 'DEBIT'
            WHEN 'REFUND' THEN 'DEBIT'
        END;
    ELSE
        expected := CASE tx_kind
            WHEN 'BET' THEN 'DEBIT'
            WHEN 'WIN' THEN 'CREDIT'
            WHEN 'REFUND' THEN 'CREDIT'
            WHEN 'OPENING' THEN 'CREDIT'
        END;
    END IF;

    IF expected IS NULL THEN
        RAISE EXCEPTION 'operação % (%) não gera lançamento', NEW.transaction_id, tx_kind
            USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_matches_transaction';
    END IF;
    IF NEW.direction <> expected THEN
        RAISE EXCEPTION 'operação % (%) exige %, recebido %', NEW.transaction_id, tx_kind, expected, NEW.direction
            USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_matches_transaction';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER ledger_matches_transaction
    BEFORE INSERT ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_matches_transaction();

-- Sequência do extrato (correção 4): o "antes" de um lançamento é o "depois"
-- do anterior, e a versão é a anterior + 1. Primeiro lançamento: antes = 0 e
-- versão 1 se for OPENING; senão versão 2 (a carteira abriu com saldo zero na
-- versão 1). Sozinho, este trigger não é seguro sob concorrência (dois
-- inserts simultâneos veem o mesmo anterior): a garantia real é o UNIQUE
-- (wallet_id, wallet_version) somado ao FOR UPDATE na carteira (ADR 0011).
CREATE FUNCTION ledger_sequence() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    prev_after       BIGINT;
    prev_version     BIGINT;
    tx_kind          TEXT;
    expected_before  BIGINT;
    expected_version BIGINT;
BEGIN
    SELECT balance_after_minor, wallet_version
      INTO prev_after, prev_version
      FROM wallet_ledger_entries
     WHERE wallet_id = NEW.wallet_id
     ORDER BY wallet_version DESC
     LIMIT 1;

    IF NOT FOUND THEN
        SELECT kind INTO tx_kind FROM wager_transactions WHERE id = NEW.transaction_id;
        expected_before := 0;
        expected_version := CASE WHEN tx_kind = 'OPENING' THEN 1 ELSE 2 END;
    ELSE
        expected_before := prev_after;
        expected_version := prev_version + 1;
    END IF;

    IF NEW.balance_before_minor <> expected_before OR NEW.wallet_version <> expected_version THEN
        RAISE EXCEPTION 'sequência do extrato da carteira %: esperado antes=% versão=%, recebido antes=% versão=%',
            NEW.wallet_id, expected_before, expected_version, NEW.balance_before_minor, NEW.wallet_version
            USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_sequence';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER ledger_sequence
    BEFORE INSERT ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_sequence();

-- E2 (parte diferida): no commit, a operação lançada está PROCESSED. Impede
-- extrato de operação recusada ou pendente. Relê a operação atual.
CREATE FUNCTION ledger_transaction_processed() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    current_status TEXT;
BEGIN
    SELECT status INTO current_status FROM wager_transactions WHERE id = NEW.transaction_id;
    IF current_status IS DISTINCT FROM 'PROCESSED' THEN
        RAISE EXCEPTION 'lançamento % aponta para operação % em %, não PROCESSED',
            NEW.id, NEW.transaction_id, current_status
            USING ERRCODE = 'check_violation', CONSTRAINT = 'ledger_transaction_processed';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER ledger_transaction_processed
    AFTER INSERT ON wallet_ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_transaction_processed();

-- E1: no commit, saldo e versão da carteira são os do último lançamento; sem
-- lançamentos, saldo 0 e versão 1. Dispara pela carteira (INSERT/UPDATE) e
-- pelo extrato (INSERT), e relê as linhas atuais das duas tabelas.
CREATE FUNCTION wallet_ledger_consistency() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    target_wallet  UUID;
    wallet_balance BIGINT;
    wallet_version BIGINT;
    last_after     BIGINT;
    last_version   BIGINT;
BEGIN
    IF TG_TABLE_NAME = 'wallets' THEN
        target_wallet := NEW.id;
    ELSE
        target_wallet := NEW.wallet_id;
    END IF;

    SELECT w.balance_minor, w.version
      INTO wallet_balance, wallet_version
      FROM wallets w
     WHERE w.id = target_wallet;

    SELECT l.balance_after_minor, l.wallet_version
      INTO last_after, last_version
      FROM wallet_ledger_entries l
     WHERE l.wallet_id = target_wallet
     ORDER BY l.wallet_version DESC
     LIMIT 1;
    IF NOT FOUND THEN
        last_after := 0;
        last_version := 1;
    END IF;

    IF wallet_balance <> last_after OR wallet_version <> last_version THEN
        RAISE EXCEPTION 'carteira % (saldo %, versão %) diverge do extrato (saldo %, versão %)',
            target_wallet, wallet_balance, wallet_version, last_after, last_version
            USING ERRCODE = 'check_violation', CONSTRAINT = 'wallet_ledger_consistency';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER wallet_ledger_consistency
    AFTER INSERT OR UPDATE ON wallets
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION wallet_ledger_consistency();

CREATE CONSTRAINT TRIGGER wallet_ledger_consistency
    AFTER INSERT ON wallet_ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION wallet_ledger_consistency();

-- app_runtime só insere e lê o extrato.
GRANT SELECT, INSERT ON wallet_ledger_entries TO app_runtime;
