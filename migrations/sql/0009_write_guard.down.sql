-- SPDX-License-Identifier: AGPL-3.0-only
-- SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

-- Dev-only.
DO $$
DECLARE
  r record;
BEGIN
  FOR r IN SELECT tgname, tgrelid::regclass AS tbl FROM pg_trigger WHERE tgname LIKE '%\_write\_guard' LOOP
    EXECUTE format('DROP TRIGGER %I ON %s', r.tgname, r.tbl);
  END LOOP;
END $$;
DROP FUNCTION IF EXISTS tracker_write_guard();
