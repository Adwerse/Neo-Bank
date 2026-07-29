DROP INDEX IF EXISTS idx_transfers_recipient_completed_created_id;
DROP INDEX IF EXISTS idx_transfers_sender_created_id;

CREATE INDEX idx_transfers_sender_account_id ON transfers (sender_account_id);
CREATE INDEX idx_transfers_recipient_account_id ON transfers (recipient_account_id);
