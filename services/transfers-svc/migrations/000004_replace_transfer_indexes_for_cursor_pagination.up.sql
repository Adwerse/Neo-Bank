DROP INDEX IF EXISTS idx_transfers_sender_account_id;
DROP INDEX IF EXISTS idx_transfers_recipient_account_id;

-- Sender side keeps all statuses visible; a composite index covers both the
-- equality filter and the ORDER BY created_at DESC, id DESC scan.
CREATE INDEX idx_transfers_sender_created_id
    ON transfers (sender_account_id, created_at DESC, id DESC);

-- Recipient side only ever queries status = 'completed' rows (see the
-- history endpoint's visibility rule) — a partial index avoids indexing
-- pending/failed/rejected rows this query path never matches.
CREATE INDEX idx_transfers_recipient_completed_created_id
    ON transfers (recipient_account_id, created_at DESC, id DESC)
    WHERE status = 'completed';
