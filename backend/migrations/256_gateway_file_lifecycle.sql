-- Lifecycle columns are additive. Keep 255 and its staging recovery separate:
-- only ready files may become retired; retired files never use upload tokens.
ALTER TABLE gateway_files
    ADD COLUMN expires_at TIMESTAMPTZ,
    ADD COLUMN lifecycle_version BIGINT NOT NULL DEFAULT 1 CHECK (lifecycle_version > 0),
    ADD COLUMN remote_activity_possible BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN retired_at TIMESTAMPTZ,
    ADD COLUMN retire_reason VARCHAR(32),
    ADD COLUMN read_drain_until TIMESTAMPTZ,
    ADD COLUMN local_purged_at TIMESTAMPTZ,
    ADD COLUMN cleanup_token VARCHAR(64),
    ADD COLUMN cleanup_lease_until TIMESTAMPTZ,
    ADD COLUMN upload_deadline_at TIMESTAMPTZ;

ALTER TABLE gateway_files
    DROP CONSTRAINT gateway_files_state_check,
    DROP CONSTRAINT gateway_files_check,
    DROP CONSTRAINT gateway_files_check1,
    ADD CONSTRAINT gateway_files_state_check
        CHECK (state IN ('staging', 'ready', 'tombstone', 'retired', 'deleted')),
    ADD CONSTRAINT gateway_files_upload_lease_check
        CHECK ((state IN ('staging', 'tombstone') AND upload_token IS NOT NULL AND upload_lease_until IS NOT NULL)
            OR (state IN ('ready', 'retired', 'deleted') AND upload_token IS NULL AND upload_lease_until IS NULL)),
    ADD CONSTRAINT gateway_files_local_charge_check CHECK (
        reserved_bytes >= 0 AND (
            (local_purged_at IS NULL AND (
                (state IN ('staging', 'ready', 'tombstone', 'retired') AND reserved_bytes >= bytes)
                OR (state = 'deleted' AND reserved_bytes = 0)))
            OR (local_purged_at IS NOT NULL AND reserved_bytes = 0 AND state IN ('retired', 'deleted')))),
    ADD CONSTRAINT gateway_files_retirement_check CHECK (
        state <> 'retired' OR (retired_at IS NOT NULL AND retire_reason IS NOT NULL
            AND read_drain_until IS NOT NULL AND read_drain_until >= retired_at)),
    ADD CONSTRAINT gateway_files_cleanup_lease_check CHECK (
        ((cleanup_token IS NULL) = (cleanup_lease_until IS NULL))
        AND (cleanup_token IS NULL OR state = 'retired'));

CREATE INDEX idx_gateway_files_visible_page ON gateway_files (user_id, group_id, created_at, id) WHERE state = 'ready';
CREATE INDEX idx_gateway_files_expiry ON gateway_files (expires_at, id) WHERE state = 'ready' AND expires_at IS NOT NULL;
CREATE INDEX idx_gateway_files_retired_cleanup ON gateway_files (cleanup_lease_until, id) WHERE state = 'retired' AND local_purged_at IS NULL;

-- Write-ahead evidence is monotonic. An absent remote lifecycle port is safe
-- only while this flag has never been set for this individual file.
CREATE FUNCTION gateway_files_keep_remote_activity() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.remote_activity_possible AND NOT NEW.remote_activity_possible THEN
        RAISE EXCEPTION 'remote activity evidence cannot be cleared' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER gateway_files_remote_activity_sticky
    BEFORE UPDATE OF remote_activity_possible ON gateway_files
    FOR EACH ROW EXECUTE FUNCTION gateway_files_keep_remote_activity();
