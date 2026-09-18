-- 000010_status_transition.down.sql
--
-- Reverses 000010 completely. Unlike 000009 there is no residue: a CHECK
-- constraint and a table privilege are both fully droppable.
--
-- Rolling this back leaves ProcessStatusTransition unable to write — the UPDATE
-- privilege is gone, so every transition fails with SQLSTATE 42501 rather than
-- silently doing nothing. That is the correct failure direction: a rollback that
-- disarmed the audit trail while leaving the status writable would let a player
-- be suspended or excluded with no record of why.

BEGIN;

REVOKE UPDATE ON users FROM engine_writer;

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_self_exclusion_has_term;

COMMIT;
