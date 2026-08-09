-- trace_context on the resource rows themselves, not just the outbox.
--
-- The reconciliation workers wake on a timer and resolve rows that were
-- left in a non-terminal state minutes or hours earlier. Their spans have
-- no parent by construction — nothing called them. Recording the
-- originating request's trace here is what lets the worker link its
-- resolution back to the request that created the row, so the full story
-- of a stuck transfer is navigable: original request -> the point it
-- stopped -> the worker that finished it, minutes later.
--
-- Same nullability reasoning as the outbox column: absent context is a
-- normal state, never an error.
ALTER TABLE transfers ADD COLUMN trace_context JSONB;
ALTER TABLE deposits ADD COLUMN trace_context JSONB;
