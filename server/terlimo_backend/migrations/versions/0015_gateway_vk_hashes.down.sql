-- Revert 0015. Fail-closed while any confirmed snapshot exists; no rows are deleted.

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM gateways WHERE vk_hashes IS NOT NULL) THEN
        RAISE EXCEPTION 'ROLLBACK_UNSAFE_0015: confirmed gateway vk_hashes exist';
    END IF;
END $$;

ALTER TABLE gateways DROP COLUMN IF EXISTS vk_hashes;
