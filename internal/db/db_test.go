package db

import (
	"testing"
)

func TestDB(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	// run_state round-trip
	if err := d.SetState("first_run", "1"); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	val, err := d.GetState("first_run")
	if err != nil || val != "1" {
		t.Fatalf("GetState: got %q, %v", val, err)
	}

	// upsert two pending bookmarks
	if err := d.UpsertPending(101, "https://example.com"); err != nil {
		t.Fatalf("UpsertPending: %v", err)
	}
	if err := d.UpsertPending(102, "https://example.org"); err != nil {
		t.Fatalf("UpsertPending: %v", err)
	}

	pending, err := d.ListPending()
	if err != nil || len(pending) != 2 {
		t.Fatalf("ListPending: got %d rows, %v", len(pending), err)
	}

	// archive one, fail the other (transient)
	if err := d.MarkArchived(101, "https://web.archive.org/web/20240101/https://example.com"); err != nil {
		t.Fatalf("MarkArchived: %v", err)
	}
	if err := d.MarkFailed(102, false, "error:cannot-fetch"); err != nil {
		t.Fatalf("MarkFailed transient: %v", err)
	}

	// pending should now be empty
	pending, _ = d.ListPending()
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending after archive/fail, got %d", len(pending))
	}

	// unsynced should have the archived row
	unsynced, err := d.ListUnsynced()
	if err != nil || len(unsynced) != 1 || unsynced[0].RaindropID != 101 {
		t.Fatalf("ListUnsynced: got %v, %v", unsynced, err)
	}

	// sync it back
	if err := d.MarkSynced(101); err != nil {
		t.Fatalf("MarkSynced: %v", err)
	}
	unsynced, _ = d.ListUnsynced()
	if len(unsynced) != 0 {
		t.Fatalf("expected 0 unsynced after MarkSynced, got %d", len(unsynced))
	}

	// reset transient → pending
	if err := d.ResetTransient(); err != nil {
		t.Fatalf("ResetTransient: %v", err)
	}
	pending, _ = d.ListPending()
	if len(pending) != 1 || pending[0].RaindropID != 102 {
		t.Fatalf("expected row 102 back as pending, got %v", pending)
	}

	// upsert refreshes the URL for non-archived rows...
	if err := d.UpsertPending(102, "https://example.org/edited"); err != nil {
		t.Fatalf("UpsertPending re-upsert: %v", err)
	}
	pending, _ = d.ListPending()
	if len(pending) != 1 || pending[0].OriginalURL != "https://example.org/edited" {
		t.Fatalf("expected refreshed URL for row 102, got %v", pending)
	}

	// ...but leaves archived rows untouched
	if err := d.UpsertPending(101, "https://example.com/edited"); err != nil {
		t.Fatalf("UpsertPending on archived row: %v", err)
	}
	pending, _ = d.ListPending()
	if len(pending) != 1 {
		t.Fatalf("archived row 101 must not become pending again, got %v", pending)
	}

	// permanent sync failure removes the row from the unsynced list
	if err := d.UpsertPending(103, "https://example.net"); err != nil {
		t.Fatalf("UpsertPending 103: %v", err)
	}
	if err := d.MarkArchived(103, "https://web.archive.org/web/20240102/https://example.net"); err != nil {
		t.Fatalf("MarkArchived 103: %v", err)
	}
	unsynced, _ = d.ListUnsynced()
	if len(unsynced) != 1 || unsynced[0].RaindropID != 103 {
		t.Fatalf("expected row 103 unsynced, got %v", unsynced)
	}
	if err := d.MarkSyncFailedPermanent(103, "bookmark no longer exists in Raindrop"); err != nil {
		t.Fatalf("MarkSyncFailedPermanent: %v", err)
	}
	unsynced, _ = d.ListUnsynced()
	if len(unsynced) != 0 {
		t.Fatalf("expected 0 unsynced after MarkSyncFailedPermanent, got %d", len(unsynced))
	}

	// counts
	archived, failedPerm, failedTrans, err := d.Counts()
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	// 101 and 103 archived, 102 now pending (not failed), 0 permanent
	if archived != 2 || failedPerm != 0 || failedTrans != 0 {
		t.Fatalf("Counts: archived=%d perm=%d trans=%d", archived, failedPerm, failedTrans)
	}

	synced, _ := d.CountSynced()
	if synced != 1 {
		t.Fatalf("CountSynced: got %d", synced)
	}

	t.Log("all db assertions passed")
}
