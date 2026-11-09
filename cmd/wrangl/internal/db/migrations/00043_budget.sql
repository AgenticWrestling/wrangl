-- +goose Up
-- The hourly cost budget (ADR 0009 section 8) and what runners cost. budget_hourly_usd NULL = no budget:
-- nothing is enforced and launching behaves as before. budget_warned remembers that the "approaching"
-- note was given, for hysteresis. runner_capacity.hourly_usd is the price (instance plus volume) stamped
-- when the controller launched the runner; NULL = free or unknown.
ALTER TABLE placement_config ADD COLUMN budget_hourly_usd REAL CHECK (budget_hourly_usd IS NULL OR budget_hourly_usd > 0);
ALTER TABLE placement_config ADD COLUMN budget_warn_pct   INTEGER NOT NULL DEFAULT 80 CHECK (budget_warn_pct BETWEEN 1 AND 100);
ALTER TABLE placement_config ADD COLUMN budget_warned     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE runner_capacity ADD COLUMN hourly_usd REAL;

-- +goose Down
ALTER TABLE runner_capacity DROP COLUMN hourly_usd;
ALTER TABLE placement_config DROP COLUMN budget_warned;
ALTER TABLE placement_config DROP COLUMN budget_warn_pct;
ALTER TABLE placement_config DROP COLUMN budget_hourly_usd;
