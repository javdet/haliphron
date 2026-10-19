-- A secret's endpoint: where the credential is to be presented, when that is
-- not its provider's default. Today only the model credential has one — the
-- root of a gateway such as OpenRouter, which the lease carries to the pod as
-- llm-base-url.
--
-- A column on the secret and not a setting of its own, because the two are
-- one fact. Stored apart, replacing one without the other is a window in which
-- a lease pairs the new key with the old endpoint, and an Anthropic key sent to
-- a gateway is a key handed to a third party. On the row, every write of the
-- value states the endpoint as well, and a write that does not name one clears
-- it.
--
-- Not encrypted: it is not secret, and the UI shows it.

-- +goose Up
ALTER TABLE secrets ADD COLUMN endpoint text
  CHECK (endpoint IS NULL OR length(endpoint) BETWEEN 1 AND 2048);

-- +goose Down
ALTER TABLE secrets DROP COLUMN endpoint;
