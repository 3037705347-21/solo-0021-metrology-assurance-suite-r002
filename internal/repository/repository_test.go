package repository_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"example.com/solo-0021-metrology-assurance-suite/internal/domain"
	"example.com/solo-0021-metrology-assurance-suite/internal/repository"
)

func TestMutateRollbackLeavesCommittedStateUntouched(t *testing.T) {
	t.Parallel()

	repo := repository.New()
	ctx := context.Background()
	at := time.Unix(1700000000, 0).UTC()

	occupancy, err := domain.NewCaseOccupancy("device-1", "case-1", at)
	if err != nil {
		t.Fatalf("new occupancy: %v", err)
	}
	if err := repo.Mutate(ctx, func(m *repository.Mutation) error {
		return m.PutOccupancy(occupancy)
	}); err != nil {
		t.Fatalf("commit occupancy: %v", err)
	}

	// A mutation that modifies the clone and then fails must not leak any of
	// its writes into the committed state.
	rollbackErr := errors.New("boom")
	err = repo.Mutate(ctx, func(m *repository.Mutation) error {
		if err := m.ReleaseOccupancy("device-1", "case-1"); err != nil {
			return err
		}
		m.NextID("case")
		m.NextID("case")
		m.NextID("event")
		return rollbackErr
	})
	if !errors.Is(err, rollbackErr) {
		t.Fatalf("expected rollback error, got %v", err)
	}

	err = repo.Read(ctx, func(snapshot repository.Snapshot) error {
		committed, exists := snapshot.Occupancy("device-1")
		if !exists {
			return errors.New("occupancy vanished after a rolled-back mutation")
		}
		if committed.CaseID != "case-1" {
			return errors.New("occupancy was altered by a rolled-back mutation")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("state after rollback: %v", err)
	}

	// IDs consumed inside the rolled-back mutation must be consumed again.
	if err := repo.Mutate(ctx, func(m *repository.Mutation) error {
		if got := m.NextID("case"); got != "case-0001" {
			t.Fatalf("counter did not roll back: got %q", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("post-rollback mutate: %v", err)
	}
}

func TestReleaseOccupancyRequiresOwnership(t *testing.T) {
	t.Parallel()

	repo := repository.New()
	ctx := context.Background()
	at := time.Unix(1700000000, 0).UTC()

	occupancy, err := domain.NewCaseOccupancy("device-1", "case-1", at)
	if err != nil {
		t.Fatalf("new occupancy: %v", err)
	}
	if err := repo.Mutate(ctx, func(m *repository.Mutation) error {
		return m.PutOccupancy(occupancy)
	}); err != nil {
		t.Fatalf("commit occupancy: %v", err)
	}

	err = repo.Mutate(ctx, func(m *repository.Mutation) error {
		return m.ReleaseOccupancy("device-1", "case-other")
	})
	if !errors.Is(err, domain.ErrStateConflict) {
		t.Fatalf("expected state conflict for foreign release, got %v", err)
	}
	err = repo.Mutate(ctx, func(m *repository.Mutation) error {
		return m.ReleaseOccupancy("missing-device", "case-1")
	})
	if !errors.Is(err, domain.ErrStateConflict) {
		t.Fatalf("expected state conflict for missing occupancy, got %v", err)
	}

	err = repo.Read(ctx, func(snapshot repository.Snapshot) error {
		if _, exists := snapshot.Occupancy("device-1"); !exists {
			return errors.New("occupancy lost after rejected releases")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUpdateOccupancyRequiresOwnership(t *testing.T) {
	t.Parallel()

	repo := repository.New()
	ctx := context.Background()
	at := time.Unix(1700000000, 0).UTC()

	occupancy, err := domain.NewCaseOccupancy("device-1", "case-1", at)
	if err != nil {
		t.Fatalf("new occupancy: %v", err)
	}
	if err := repo.Mutate(ctx, func(m *repository.Mutation) error {
		return m.PutOccupancy(occupancy)
	}); err != nil {
		t.Fatalf("commit occupancy: %v", err)
	}

	foreign := domain.CaseOccupancy{
		DeviceID:  "device-1",
		CaseID:    "case-other",
		State:     domain.CaseReviewReady,
		Version:   2,
		OpenedAt:  at,
		UpdatedAt: at.Add(time.Second),
	}
	err = repo.Mutate(ctx, func(m *repository.Mutation) error {
		return m.UpdateOccupancy(foreign)
	})
	if !errors.Is(err, domain.ErrStateConflict) {
		t.Fatalf("expected state conflict for foreign update, got %v", err)
	}
}
