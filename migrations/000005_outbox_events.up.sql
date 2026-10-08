-- Transactional outbox: eventos gravados na mesma transação da operação e
-- publicados depois do commit por um worker, com lease e backoff.

CREATE TABLE outbox_events (
    -- Mesmo em republicações o event_id não muda.
    event_id        UUID        NOT NULL,
    aggregate_type  TEXT        NOT NULL,
    aggregate_id    UUID        NOT NULL,
    event_type      TEXT        NOT NULL,
    event_version   INT         NOT NULL,
    correlation_id  TEXT        NOT NULL,
    causation_id    TEXT,
    -- Snapshot imutável do evento.
    payload         JSONB       NOT NULL,
    occurred_at     TIMESTAMPTZ NOT NULL,
    -- Controle de entrega.
    attempts        INT         NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL,
    locked_by       TEXT,
    locked_until    TIMESTAMPTZ,
    last_error      TEXT,
    published_at    TIMESTAMPTZ,

    CONSTRAINT outbox_events_pkey PRIMARY KEY (event_id),
    CONSTRAINT outbox_event_version_positive CHECK (event_version >= 1),
    CONSTRAINT outbox_attempts_non_negative CHECK (attempts >= 0),
    -- Lease: dono e prazo andam juntos.
    CONSTRAINT outbox_lease_coherent CHECK ((locked_by IS NULL) = (locked_until IS NULL))
);

-- Eventos ainda não publicados, na ordem de próxima tentativa.
CREATE INDEX ix_outbox_due ON outbox_events (next_attempt_at) WHERE published_at IS NULL;

-- O conteúdo do evento é imutável; só os campos de entrega mudam. Depois de
-- publicado, o evento inteiro fica congelado.
CREATE FUNCTION outbox_events_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.published_at IS NOT NULL THEN
        RAISE EXCEPTION 'evento % já publicado não muda mais', OLD.event_id
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'outbox_event_published_frozen';
    END IF;

    IF NEW.event_id IS DISTINCT FROM OLD.event_id
        OR NEW.aggregate_type IS DISTINCT FROM OLD.aggregate_type
        OR NEW.aggregate_id IS DISTINCT FROM OLD.aggregate_id
        OR NEW.event_type IS DISTINCT FROM OLD.event_type
        OR NEW.event_version IS DISTINCT FROM OLD.event_version
        OR NEW.correlation_id IS DISTINCT FROM OLD.correlation_id
        OR NEW.causation_id IS DISTINCT FROM OLD.causation_id
        OR NEW.payload IS DISTINCT FROM OLD.payload
        OR NEW.occurred_at IS DISTINCT FROM OLD.occurred_at THEN
        RAISE EXCEPTION 'conteúdo do evento % é imutável', OLD.event_id
            USING ERRCODE = 'integrity_constraint_violation', CONSTRAINT = 'outbox_event_immutable';
    END IF;

    RETURN NEW;
END;
$$;

CREATE TRIGGER outbox_events_guard
    BEFORE UPDATE ON outbox_events
    FOR EACH ROW EXECUTE FUNCTION outbox_events_guard();

GRANT SELECT, INSERT ON outbox_events TO app_runtime;
GRANT UPDATE (attempts, next_attempt_at, locked_by, locked_until, last_error, published_at)
    ON outbox_events TO app_runtime;
