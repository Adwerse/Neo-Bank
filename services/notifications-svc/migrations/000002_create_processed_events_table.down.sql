-- notifications_processed_events, NOT "processed_events" — the latter is
-- accounts-svc's table in this same shared "neobank" database, and this
-- file used to name it. That bug was silent in both directions: IF EXISTS
-- meant no error, so rolling notifications-svc back one version would
-- have dropped accounts-svc's dedup table (making it reprocess every
-- UserActivated it had ever consumed) while leaving this service's table
-- in place, so the subsequent re-up would fail on "relation already
-- exists" and mark the migration dirty. See the up migration's comment
-- for why the prefix exists in the first place.
DROP TABLE IF EXISTS notifications_processed_events;
