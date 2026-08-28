-- A mutation the server refuses is not the end of the story, because the
-- local database keeps the effect of the write it refused. The two have
-- genuinely diverged at that point, and the divergence has to outlive the
-- sync that caused it — otherwise the queue empties, `ark status` reads
-- clean, and the client is confidently wrong about a repository the server
-- will never agree with (elk-work/ark#46).
--
-- The mutation row already survives: status goes to 'rejected' and
-- error_message holds the server's reason (spec §8). What was missing is a
-- way to say the divergence is *over*, and that matters more than it looks.
-- A count that can only ever go up is a warning light nobody reads, which is
-- the same silence in a different costume. resolved_at is stamped when the
-- record next reaches agreement with the server — a later mutation on it
-- applies, or a pull brings the server's copy down — so `ark status` reports
-- outstanding rejections and stops reporting repaired ones.
--
-- The column mirrors conflicts.resolved_at deliberately: an unresolved
-- rejection and an unresolved conflict are the same kind of fact about this
-- checkout, and they should be shaped the same way.

ALTER TABLE mutations ADD COLUMN resolved_at TEXT;

-- `ark status` runs the outstanding-rejection count on every invocation, and
-- the existing index is on (status, created_at), which does not serve it.
CREATE INDEX idx_mutations_unresolved ON mutations(repository_id, status, resolved_at);
