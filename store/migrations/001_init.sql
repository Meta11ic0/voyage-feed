CREATE TABLE schema_migrations (
    version text PRIMARY KEY,
    applied_at timestamptz NOT NULL
);

CREATE TABLE market_instances (
    market_id text PRIMARY KEY,
    writer_lock_key bigint GENERATED ALWAYS AS IDENTITY UNIQUE,
    dataset_sha256 text NOT NULL,
    dataset_id text NOT NULL,
    model_version text NOT NULL,
    seed numeric(20, 0) NOT NULL,
    trade_probability_bps int NOT NULL,
    max_move_bps int NOT NULL,
    cap_budget_cents bigint NOT NULL,
    clock_origin timestamptz NOT NULL,
    logical_tick bigint NOT NULL,
    last_event_seq bigint NOT NULL,
    first_retained_seq bigint NOT NULL DEFAULT 1,
    market_date date NOT NULL,
    CHECK (logical_tick >= 0),
    CHECK (last_event_seq >= 0)
);

CREATE TABLE instruments (
    market_id text NOT NULL REFERENCES market_instances (market_id),
    instrument_id text NOT NULL,
    symbol text NOT NULL,
    name text NOT NULL,
    sector text NOT NULL,
    role text NOT NULL,
    chapter_count integer,
    popularity_rank integer,
    initial_price_cents bigint NOT NULL,
    target_cap_cents bigint NOT NULL,
    shares_total bigint NOT NULL,
    shares_float bigint NOT NULL,
    PRIMARY KEY (market_id, instrument_id),
    UNIQUE (market_id, symbol),
    CHECK (initial_price_cents > 0),
    CHECK (target_cap_cents > 0),
    CHECK (shares_total > 0),
    CHECK (shares_float > 0),
    CHECK (shares_float <= shares_total)
);

CREATE TABLE instrument_states (
    market_id text NOT NULL,
    instrument_id text NOT NULL,
    prev_close_cents bigint NOT NULL,
    last_price_cents bigint,
    last_event_seq bigint,
    last_trade_time timestamptz,
    day_open_cents bigint,
    day_high_cents bigint,
    day_low_cents bigint,
    day_volume bigint NOT NULL,
    day_turnover_cents bigint NOT NULL,
    PRIMARY KEY (market_id, instrument_id),
    FOREIGN KEY (market_id, instrument_id) REFERENCES instruments (market_id, instrument_id),
    CHECK (day_volume >= 0),
    CHECK (day_turnover_cents >= 0)
);

CREATE TABLE market_events (
    market_id text NOT NULL REFERENCES market_instances (market_id),
    event_seq bigint NOT NULL,
    logical_tick bigint NOT NULL,
    event_time timestamptz NOT NULL,
    generated_at timestamptz NOT NULL,
    market_date date NOT NULL,
    type text NOT NULL,
    instrument_id text,
    price_cents bigint,
    quantity bigint,
    turnover_cents bigint,
    PRIMARY KEY (market_id, event_seq)
);

CREATE INDEX market_events_time_idx ON market_events (market_id, event_time, event_seq);

CREATE INDEX market_events_instrument_idx ON market_events (market_id, instrument_id, event_time, event_seq);

CREATE TABLE bars_1m (
    market_id text NOT NULL,
    instrument_id text NOT NULL,
    bucket_start timestamptz NOT NULL,
    open_cents bigint NOT NULL,
    high_cents bigint NOT NULL,
    low_cents bigint NOT NULL,
    close_cents bigint NOT NULL,
    volume bigint NOT NULL,
    turnover_cents bigint NOT NULL,
    trade_count bigint NOT NULL,
    first_event_seq bigint NOT NULL,
    last_event_seq bigint NOT NULL,
    PRIMARY KEY (market_id, instrument_id, bucket_start),
    FOREIGN KEY (market_id, instrument_id) REFERENCES instruments (market_id, instrument_id)
);
