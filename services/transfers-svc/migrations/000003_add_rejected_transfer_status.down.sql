ALTER TABLE transfers DROP CONSTRAINT transfers_status_check;
ALTER TABLE transfers ADD CONSTRAINT transfers_status_check CHECK (status IN ('pending', 'completed', 'failed'));
