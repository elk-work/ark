package store

import (
	"context"
	"database/sql"
	"strings"

	"github.com/ijroth/ark/internal/records"
)

// Promotion records that a version — a merge commit and/or an artifact
// checksum — became active in an environment. It is the deployment anchor
// for observability tooling. See docs/v1-spec.md §6.10.
type Promotion struct {
	ID             string `json:"id"`
	RepositoryID   string `json:"repository_id"`
	Environment    string `json:"environment"`
	Service        string `json:"service,omitempty"`
	MergeCommitSHA string `json:"merge_commit_sha,omitempty"`
	ArtifactSHA256 string `json:"artifact_sha256,omitempty"`
	PullRequestID  string `json:"pull_request_id,omitempty"`
	ActivatedAt    string `json:"activated_at"`
	EndedAt        string `json:"ended_at,omitempty"`
	MetadataJSON   string `json:"metadata_json,omitempty"`
	CreatedAt      string `json:"created_at"`
	CreatedBy      string `json:"created_by"`
	CreatedByType  string `json:"created_by_type"`
}

// PromotionEnd describes the close of a promotion's active period.
type PromotionEnd struct {
	EndedAt string `json:"ended_at"`
}

// CreatePromotion records a new active promotion. Any promotion still active
// for the same (environment, service) is superseded in the same transaction:
// its ended_at becomes the new promotion's activated_at.
func (s *Store) CreatePromotion(ctx context.Context, p *Promotion) (*Promotion, error) {
	if strings.TrimSpace(p.Environment) == "" {
		return nil, records.Validationf("environment is required")
	}
	if p.MergeCommitSHA == "" && p.ArtifactSHA256 == "" {
		return nil, records.Validationf("a promotion needs a merge commit SHA or an artifact sha256")
	}
	now := records.Now()
	p.ID = records.NewID()
	p.RepositoryID = s.RepoID
	p.ActivatedAt = now
	p.EndedAt = ""
	p.CreatedAt = now
	p.CreatedBy = s.Actor.ID
	p.CreatedByType = string(s.Actor.Type)
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// Supersede whatever is still active for this environment/service.
		prior, err := activePromotions(tx, s.RepoID, p.Environment, p.Service)
		if err != nil {
			return err
		}
		for _, pr := range prior {
			if _, err := tx.Exec(`UPDATE promotions SET ended_at = ? WHERE id = ?`,
				p.ActivatedAt, pr.id); err != nil {
				return records.DBErr("end prior promotion", err)
			}
			if err := s.logMutation(tx, records.TypePromotion, pr.id, "update", pr.serverRevision,
				PromotionEnd{EndedAt: p.ActivatedAt}); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`INSERT INTO promotions
			(id, repository_id, environment, service, merge_commit_sha, artifact_sha256,
			 pull_request_id, activated_at, metadata_json, created_at, created_by,
			 created_by_type, sync_state)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'local')`,
			p.ID, p.RepositoryID, p.Environment, p.Service, p.MergeCommitSHA,
			p.ArtifactSHA256, nullable(p.PullRequestID), p.ActivatedAt, p.MetadataJSON,
			p.CreatedAt, p.CreatedBy, p.CreatedByType); err != nil {
			return records.DBErr("create promotion", err)
		}
		return s.logMutation(tx, records.TypePromotion, p.ID, "create", 0, p)
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// activePromotion is the id and sync base of one still-active promotion.
type activePromotion struct {
	id             string
	serverRevision int64
}

// activePromotions returns every promotion still active for one
// (environment, service), in activation order.
func activePromotions(tx *sql.Tx, repoID, environment, service string) ([]activePromotion, error) {
	rows, err := tx.Query(`SELECT id, server_revision FROM promotions
		WHERE repository_id = ? AND environment = ? AND service = ?
		AND ended_at IS NULL AND deleted_at IS NULL ORDER BY activated_at, id`,
		repoID, environment, service)
	if err != nil {
		return nil, records.DBErr("find active promotions", err)
	}
	defer rows.Close()
	var out []activePromotion
	for rows.Next() {
		var p activePromotion
		if err := rows.Scan(&p.id, &p.serverRevision); err != nil {
			return nil, records.DBErr("scan promotion id", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// EndPromotion explicitly closes an active promotion (e.g. the environment
// was torn down rather than superseded by a newer promotion).
func (s *Store) EndPromotion(ctx context.Context, ref string) (*Promotion, error) {
	p, err := s.ResolvePromotion(ctx, ref)
	if err != nil {
		return nil, err
	}
	if p.EndedAt != "" {
		return nil, records.Validationf("promotion %s already ended at %s", p.ID, p.EndedAt)
	}
	end := PromotionEnd{EndedAt: records.Now()}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE promotions SET ended_at = ? WHERE id = ?`,
			end.EndedAt, p.ID); err != nil {
			return records.DBErr("end promotion", err)
		}
		var rev int64
		if err := tx.QueryRow(`SELECT server_revision FROM promotions WHERE id = ?`, p.ID).Scan(&rev); err != nil {
			return records.DBErr("read revision", err)
		}
		return s.logMutation(tx, records.TypePromotion, p.ID, "update", rev, end)
	})
	if err != nil {
		return nil, err
	}
	p.EndedAt = end.EndedAt
	return p, nil
}

const promotionCols = `id, repository_id, environment, service, merge_commit_sha,
	artifact_sha256, pull_request_id, activated_at, ended_at, metadata_json,
	created_at, created_by, created_by_type`

func scanPromotion(row interface{ Scan(...any) error }) (*Promotion, error) {
	var p Promotion
	var prID, endedAt sql.NullString
	err := row.Scan(&p.ID, &p.RepositoryID, &p.Environment, &p.Service, &p.MergeCommitSHA,
		&p.ArtifactSHA256, &prID, &p.ActivatedAt, &endedAt, &p.MetadataJSON,
		&p.CreatedAt, &p.CreatedBy, &p.CreatedByType)
	if err != nil {
		return nil, err
	}
	p.PullRequestID = prID.String
	p.EndedAt = endedAt.String
	return &p, nil
}

// ResolvePromotion loads a promotion by ULID or prefix.
func (s *Store) ResolvePromotion(ctx context.Context, ref string) (*Promotion, error) {
	id, err := s.resolveID(s.DB, "promotions", ref)
	if err != nil {
		return nil, err
	}
	p, err := scanPromotion(s.DB.QueryRowContext(ctx,
		`SELECT `+promotionCols+` FROM promotions WHERE id = ?`, id))
	if err != nil {
		return nil, records.DBErr("load promotion", err)
	}
	return p, nil
}

// ListPromotions returns promotions, optionally filtered to one environment
// and/or to those still active.
func (s *Store) ListPromotions(ctx context.Context, environment string, activeOnly bool) ([]*Promotion, error) {
	q := `SELECT ` + promotionCols + ` FROM promotions WHERE repository_id = ? AND deleted_at IS NULL`
	args := []any{s.RepoID}
	if environment != "" {
		q += ` AND environment = ?`
		args = append(args, environment)
	}
	if activeOnly {
		q += ` AND ended_at IS NULL`
	}
	q += ` ORDER BY activated_at, id`
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, records.DBErr("list promotions", err)
	}
	defer rows.Close()
	var out []*Promotion
	for rows.Next() {
		p, err := scanPromotion(rows)
		if err != nil {
			return nil, records.DBErr("scan promotion", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
