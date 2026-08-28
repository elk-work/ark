package server

// Reading the ledger of references this service accepted while holding
// nothing at the other end (docs/v1-spec.md §9.1, elk-work/ark#74).
//
// #56 made the service stop being silent about an orphan it had just created,
// and #77 wrote every one of them down. Nothing read the table. The
// outstanding set was defined by a query in the spec and there was no way to
// run it against a live service: an operator learned that a repository was
// serving an orphan by copying `repos/<id>.db` out of the bucket. That is a
// warning light behind a locked door, which is most of the way back to the
// silence the ledger exists to end.
//
// **Read, not admin.** A dangling reference names record ids in one
// repository, so reading it is reading the repository, and `read` is the
// level that pulls records — seeing which pointers failed to resolve is
// strictly less than seeing the records themselves. `ark repo grants` is
// `admin` for a reason that does not apply here: that list is a roster of
// email addresses, and the people who can see one are the people who can
// change it. Putting a defect back behind an `admin` door would be reinventing
// the locked room in a smaller size.

import (
	"database/sql"
	"net/http"
	"strconv"

	"github.com/elk-work/ark/pkg/api"
)

// outstandingPredicate is §9.1's definition of the set that matters, in the
// one place it is written. A reference is outstanding when the record it
// names is still not here — existence, not liveness, because a tombstone
// updates the row rather than removing it (internal/store/sync.go), so a
// deleted parent satisfies the reference exactly as recordDangling decided it
// does when it wrote the entry.
const outstandingPredicate = `NOT EXISTS (SELECT 1 FROM records r
	WHERE r.record_type = d.parent_type AND r.record_id = d.parent_id)`

// danglingQuery lists entries, oldest first.
//
// The ordering is oldest-first on purpose, and the `substr` is not a
// micro-optimisation. `first_seen_at` is RFC3339Nano text, which SQLite
// compares byte by byte and which therefore does not sort chronologically
// (§9.1, records.TimeCompare): a stamp with no fractional part ends in `Z`,
// which sorts after `.` — so two entries in the same second can come back in
// the wrong order. Nineteen characters is the fixed-width prefix through
// seconds, which does compare correctly, and the record key breaks the tie
// inside a second deterministically.
//
// Oldest-first because that is the order of alarm. A reference recorded
// minutes ago is the ordinary skew that ends on its own; one that has been
// outstanding for a week is a record that is never coming. When `limit` cuts
// the listing short, the entries that survive should be the ones worth
// looking at.
const danglingQuery = `SELECT d.record_type, d.record_id, d.field,
	d.parent_type, d.parent_id, d.mutation_id, d.first_seen_at,
	NOT (` + outstandingPredicate + `) AS resolved
	FROM dangling_references d `

const danglingOrder = ` ORDER BY substr(d.first_seen_at, 1, 19),
	d.record_type, d.record_id, d.field LIMIT ?`

// handleDangling answers what is dangling in one repository.
func (s *Server) handleDangling(w http.ResponseWriter, r *http.Request) {
	repoID := r.PathValue("repo")
	if !s.allow(w, r, repoID, api.GrantRead) {
		return
	}
	all, fault := boolParam(r, "all")
	if fault != nil {
		writeErr(w, fault.status, fault.code, fault.msg)
		return
	}
	limit, fault := limitParam(r)
	if fault != nil {
		writeErr(w, fault.status, fault.code, fault.msg)
		return
	}

	ctx := r.Context()
	resp := api.DanglingResponse{RepositoryID: repoID, References: []api.DanglingReference{}}
	err := s.Repos.View(ctx, repoID, func(db *sql.DB) error {
		// The counts describe the repository and not the listing, so they
		// are taken whatever `all` and `limit` say. A truncated answer must
		// not be able to make a repository look healthier than it is.
		if err := db.QueryRowContext(ctx, `SELECT
			COUNT(*), COALESCE(SUM(`+outstandingPredicate+`), 0)
			FROM dangling_references d`).Scan(&resp.Recorded, &resp.Outstanding); err != nil {
			return err
		}
		if err := db.QueryRowContext(ctx,
			`SELECT revision FROM meta WHERE id = 1`).Scan(&resp.ServerRevision); err != nil {
			return err
		}

		where := " WHERE " + outstandingPredicate
		if all {
			where = ""
		}
		// One more than asked for, so truncation is observed rather than
		// inferred from a count that a concurrent push could have moved.
		rows, err := db.QueryContext(ctx, danglingQuery+where+danglingOrder, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ref api.DanglingReference
			if err := rows.Scan(&ref.RecordType, &ref.RecordID, &ref.Field,
				&ref.ParentType, &ref.ParentID, &ref.MutationID,
				&ref.FirstSeenAt, &ref.Resolved); err != nil {
				return err
			}
			if len(resp.References) == limit {
				resp.Truncated = true
				break
			}
			resp.References = append(resp.References, ref)
		}
		return rows.Err()
	})
	// No `sql.ErrNoRows` case for the `meta` read above: repodb.View has
	// already established that the stored database holds a repository row
	// before it runs the closure, and answers `not_found` itself when it does
	// not — with the better message, which distinguishes an object that is
	// absent from one that is present and zero-length (#85).
	if err != nil {
		s.respond(w, "list dangling references", err)
		return
	}
	writeJSON(w, resp)
}

// boolParam reads a query parameter that is present-or-absent to a person and
// true-or-false to a program. Both spellings work — `?all` and `?all=true` —
// because both are what a caller writes by hand, and anything else is a typo
// that must not be read as false: `?all=ture` silently listing the wrong set
// is the failure this rejects.
func boolParam(r *http.Request, name string) (bool, *writeFault) {
	q := r.URL.Query()
	if !q.Has(name) {
		return false, nil
	}
	raw := q.Get(name)
	if raw == "" {
		return true, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, faultValidation(name + " takes true or false, not " + strconv.Quote(raw))
	}
	return v, nil
}

// limitParam reads how many entries to list, defaulting rather than
// requiring one.
func limitParam(r *http.Request) (int, *writeFault) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return api.DanglingDefaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > api.DanglingMaxLimit {
		return 0, faultValidation("limit must be a number from 1 to " +
			strconv.Itoa(api.DanglingMaxLimit))
	}
	return n, nil
}
