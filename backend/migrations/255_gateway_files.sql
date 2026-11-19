-- Private, locally durable Files. API key is provenance, never ownership.
-- Deliberately no key FK: rotating/deleting a key must not remove user files.
-- Rollback: disable gateway.files.enabled and preserve both tables and the
-- private volume. Dropping them is a separate destructive data-retention action.
CREATE TABLE gateway_files (
    id BIGSERIAL PRIMARY KEY,
    public_id VARCHAR(64) NOT NULL UNIQUE,
    user_id BIGINT NOT NULL,
    api_key_id BIGINT NOT NULL,
    group_id BIGINT NOT NULL,
    storage_key CHAR(32) NOT NULL UNIQUE,
    filename VARCHAR(255) NOT NULL,
    purpose VARCHAR(32) NOT NULL,
    mime VARCHAR(255) NOT NULL,
    state VARCHAR(16) NOT NULL CHECK (state IN ('staging', 'ready', 'tombstone', 'deleted')),
    bytes BIGINT NOT NULL DEFAULT 0 CHECK (bytes >= 0),
    reserved_bytes BIGINT NOT NULL CHECK (reserved_bytes >= 0 AND bytes <= reserved_bytes),
    sha256 CHAR(64) NOT NULL DEFAULT '',
    upload_token VARCHAR(64),
    upload_lease_until TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((state IN ('staging', 'tombstone') AND upload_token IS NOT NULL AND upload_lease_until IS NOT NULL)
        OR (state IN ('ready', 'deleted') AND upload_token IS NULL AND upload_lease_until IS NULL))
);

CREATE INDEX idx_gateway_files_owner ON gateway_files (user_id, group_id, id) WHERE state = 'ready';
CREATE INDEX idx_gateway_files_budget ON gateway_files (user_id) INCLUDE (reserved_bytes) WHERE state <> 'deleted';
CREATE INDEX idx_gateway_files_recovery ON gateway_files (upload_lease_until, id) WHERE state IN ('staging', 'tombstone');

-- Finite, renewable read leases coordinate readers with future tombstone/delete.
CREATE TABLE gateway_file_read_leases (
    file_id BIGINT NOT NULL REFERENCES gateway_files(id) ON DELETE CASCADE,
    token VARCHAR(64) NOT NULL,
    lease_until TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (file_id, token)
);
CREATE INDEX idx_gateway_file_read_leases_expiry ON gateway_file_read_leases (lease_until);
