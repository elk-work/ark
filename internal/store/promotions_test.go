package store

import (
	"context"
	"errors"
	"testing"

	"github.com/ijroth/ark/internal/records"
)

func TestPromotionLifecycle(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	p1, err := s.CreatePromotion(ctx, &Promotion{Environment: "production", MergeCommitSHA: "abc123"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p1.ActivatedAt == "" || p1.EndedAt != "" {
		t.Errorf("first promotion: %+v", p1)
	}
	// One create mutation logged.
	if n := mutationCount(t, s); n != 1 {
		t.Errorf("mutations = %d, want 1", n)
	}

	// A second promotion in the same environment supersedes the first: its
	// ended_at becomes the successor's activated_at, in the same transaction.
	p2, err := s.CreatePromotion(ctx, &Promotion{Environment: "production", MergeCommitSHA: "def456"})
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	ended, err := s.ResolvePromotion(ctx, p1.ID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ended.EndedAt != p2.ActivatedAt {
		t.Errorf("prior ended_at = %q, want %q", ended.EndedAt, p2.ActivatedAt)
	}
	// The supersede logged an update alongside the create: 3 mutations total.
	if n := mutationCount(t, s); n != 3 {
		t.Errorf("mutations = %d, want 3", n)
	}

	// A different environment or service does not supersede production.
	p3, _ := s.CreatePromotion(ctx, &Promotion{Environment: "staging", MergeCommitSHA: "abc123"})
	p4, _ := s.CreatePromotion(ctx, &Promotion{Environment: "production", Service: "api", ArtifactSHA256: "feed"})
	current, _ := s.ResolvePromotion(ctx, p2.ID)
	if current.EndedAt != "" {
		t.Errorf("p2 superseded by unrelated promotions: %+v", current)
	}

	// List filters by environment and active state.
	all, err := s.ListPromotions(ctx, "", false)
	if err != nil || len(all) != 4 {
		t.Fatalf("all promotions = %d (%v), want 4", len(all), err)
	}
	prod, _ := s.ListPromotions(ctx, "production", false)
	if len(prod) != 3 {
		t.Errorf("production promotions = %d, want 3", len(prod))
	}
	active, _ := s.ListPromotions(ctx, "production", true)
	if len(active) != 2 || active[0].ID != p2.ID || active[1].ID != p4.ID {
		t.Errorf("active production promotions: %+v", active)
	}
	if staging, _ := s.ListPromotions(ctx, "staging", true); len(staging) != 1 || staging[0].ID != p3.ID {
		t.Errorf("staging promotions: %+v", staging)
	}
}

func TestPromotionExplicitEnd(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	p, _ := s.CreatePromotion(ctx, &Promotion{Environment: "staging", ArtifactSHA256: "cafe"})
	ended, err := s.EndPromotion(ctx, p.ID[:10])
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	if ended.EndedAt == "" {
		t.Errorf("ended promotion: %+v", ended)
	}
	// Double end rejected.
	if _, err := s.EndPromotion(ctx, p.ID); err == nil {
		t.Error("double end accepted")
	}
	// create + update logged.
	if n := mutationCount(t, s); n != 2 {
		t.Errorf("mutations = %d, want 2", n)
	}
	if active, _ := s.ListPromotions(ctx, "staging", true); len(active) != 0 {
		t.Errorf("active after end: %+v", active)
	}
}

func TestPromotionValidation(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	wantValidation := func(desc string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s accepted", desc)
			return
		}
		var ae *records.Error
		if !errors.As(err, &ae) || ae.Kind != records.KindValidation {
			t.Errorf("%s: want validation error, got %v", desc, err)
		}
	}
	_, err := s.CreatePromotion(ctx, &Promotion{MergeCommitSHA: "abc123"})
	wantValidation("missing environment", err)
	_, err = s.CreatePromotion(ctx, &Promotion{Environment: "  ", MergeCommitSHA: "abc123"})
	wantValidation("blank environment", err)
	_, err = s.CreatePromotion(ctx, &Promotion{Environment: "production"})
	wantValidation("no version", err)

	// Unknown reference is a typed not-found.
	_, err = s.ResolvePromotion(ctx, "01ZZZZZZZZ")
	var ae *records.Error
	if !errors.As(err, &ae) || ae.Kind != records.KindNotFound {
		t.Errorf("unknown promotion: want not-found error, got %v", err)
	}
}

func TestPromotionMutationPayloads(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	p1, _ := s.CreatePromotion(ctx, &Promotion{Environment: "production", MergeCommitSHA: "abc123"})
	p2, _ := s.CreatePromotion(ctx, &Promotion{Environment: "production", MergeCommitSHA: "def456"})

	rows, err := s.DB.Query(`SELECT record_id, operation, payload_json FROM mutations
		WHERE record_type = 'promotion' ORDER BY created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type mut struct{ id, op, payload string }
	var muts []mut
	for rows.Next() {
		var m mut
		rows.Scan(&m.id, &m.op, &m.payload)
		muts = append(muts, m)
	}
	if len(muts) != 3 {
		t.Fatalf("mutations = %d, want 3", len(muts))
	}
	if muts[0].id != p1.ID || muts[0].op != "create" {
		t.Errorf("first mutation: %+v", muts[0])
	}
	// The supersede is an update of the prior record carrying only ended_at.
	if muts[1].id != p1.ID || muts[1].op != "update" ||
		muts[1].payload != `{"ended_at":"`+p2.ActivatedAt+`"}` {
		t.Errorf("supersede mutation: %+v", muts[1])
	}
	if muts[2].id != p2.ID || muts[2].op != "create" {
		t.Errorf("create mutation: %+v", muts[2])
	}
}
