-- 000002_users.down.sql

BEGIN;

DROP TRIGGER IF EXISTS users_set_updated_at ON users;
DROP TABLE IF EXISTS users;
DROP FUNCTION IF EXISTS trg_set_updated_at();

-- citext extension intentionally NOT dropped: other migrations / objects may rely on it.

COMMIT;
