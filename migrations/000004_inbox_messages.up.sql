-- Livro de mensagens recebidas pelo consumidor SQS: identidade durável da
-- mensagem por consumidor, para reconhecer reentregas.

CREATE TABLE inbox_messages (
    consumer_name  TEXT        NOT NULL,
    message_id     TEXT        NOT NULL,
    -- Detecta a mesma mensagem reentregue com conteúdo diferente.
    payload_hash   BYTEA       NOT NULL,
    transaction_id UUID,
    received_at    TIMESTAMPTZ NOT NULL,
    processed_at   TIMESTAMPTZ,

    CONSTRAINT inbox_messages_pkey PRIMARY KEY (consumer_name, message_id),
    CONSTRAINT inbox_transaction_fkey FOREIGN KEY (transaction_id)
        REFERENCES wager_transactions (id)
);

-- Identidade, hash e recebimento não mudam depois de gravados.
CREATE FUNCTION inbox_messages_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.consumer_name IS DISTINCT FROM OLD.consumer_name
        OR NEW.message_id IS DISTINCT FROM OLD.message_id
        OR NEW.payload_hash IS DISTINCT FROM OLD.payload_hash
        OR NEW.received_at IS DISTINCT FROM OLD.received_at THEN
        RAISE EXCEPTION 'identidade e hash da mensagem %/% são imutáveis', OLD.consumer_name, OLD.message_id
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'inbox_identity_immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER inbox_messages_guard
    BEFORE UPDATE ON inbox_messages
    FOR EACH ROW EXECUTE FUNCTION inbox_messages_guard();

GRANT SELECT, INSERT ON inbox_messages TO app_runtime;
GRANT UPDATE (transaction_id, processed_at) ON inbox_messages TO app_runtime;
