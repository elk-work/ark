-- A repository's server revision is a counter that only ever increases. If a
-- pull answers with a revision *below* one this checkout has already synced
-- past, the history the client was tracking is not the history the service is
-- serving: the repository's database was reset, lost, or restored from before
-- that point.
--
-- That happened, and nothing noticed for six weeks (elk-work/ark#58). A
-- checkout synced cleanly to revision 18 on 2026-07-13 with all seventeen of
-- its mutations acknowledged; the repository was absent from the service
-- afterwards, and its row was minted fresh — `created_at` today, revision 4 —
-- by the next sync. Every local signal was correct and every one of them said
-- everything was fine, because the queue was empty and honest. The client had
-- no notion of "the service agrees these records exist".
--
-- These columns give it one. They record the event rather than a derived
-- state, because the derived state stops being true almost immediately: once
-- the client resumes pushing, the service's counter climbs back past the old
-- mark while nothing whatsoever has been recovered. The revision comparison
-- can only be trusted at the moment it is made, so the moment is what is
-- stored.
--
-- Deliberately not self-clearing, unlike a rejection (§9.1). A rejection ends
-- when the record and the service agree again, which is a thing that can be
-- observed. This does not end until a person decides what to do about the
-- records the service no longer holds, and no comparison the client can make
-- will tell it that has happened.

ALTER TABLE sync_state ADD COLUMN history_reset_at TEXT;
ALTER TABLE sync_state ADD COLUMN history_reset_server_revision INTEGER;
ALTER TABLE sync_state ADD COLUMN history_reset_local_revision INTEGER;
