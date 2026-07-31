package db

import (
	"testing"
	"time"
)

func TestDB(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()
	createdAt := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)

	// run_state round-trip
	if err := d.SetState("first_run", "1"); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	val, err := d.GetState("first_run")
	if err != nil || val != "1" {
		t.Fatalf("GetState: got %q, %v", val, err)
	}

	// upsert two pending bookmarks
	if err := d.UpsertPending(101, "https://example.com", createdAt); err != nil {
		t.Fatalf("UpsertPending: %v", err)
	}
	if err := d.UpsertPending(102, "https://example.org", createdAt); err != nil {
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
	if err := d.MarkFailed(102, false, "error:cannot-fetch", "capture could not be fetched"); err != nil {
		t.Fatalf("MarkFailed transient: %v", err)
	}
	var storedError string
	if err := d.conn.QueryRow(`SELECT error FROM archived_bookmarks WHERE raindrop_id = 102`).Scan(&storedError); err != nil {
		t.Fatalf("read stored failure: %v", err)
	}
	if storedError != "capture could not be fetched" {
		t.Fatalf("stored failure = %q, want capture could not be fetched", storedError)
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
	if err := d.UpsertPending(102, "https://example.org/edited", createdAt); err != nil {
		t.Fatalf("UpsertPending re-upsert: %v", err)
	}
	pending, _ = d.ListPending()
	if len(pending) != 1 || pending[0].OriginalURL != "https://example.org/edited" {
		t.Fatalf("expected refreshed URL for row 102, got %v", pending)
	}

	// ...but leaves archived rows untouched
	if err := d.UpsertPending(101, "https://example.com/edited", createdAt); err != nil {
		t.Fatalf("UpsertPending on archived row: %v", err)
	}
	pending, _ = d.ListPending()
	if len(pending) != 1 {
		t.Fatalf("archived row 101 must not become pending again, got %v", pending)
	}

	// permanent sync failure removes the row from the unsynced list
	if err := d.UpsertPending(103, "https://example.net", createdAt); err != nil {
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

	// status lookup
	status, err := d.StatusOf(101)
	if err != nil || status != "archived" {
		t.Fatalf("StatusOf(101): got %q, %v", status, err)
	}
	status, err = d.StatusOf(999)
	if err != nil || status != "" {
		t.Fatalf("StatusOf(999): expected empty for unknown ID, got %q, %v", status, err)
	}

	// transient listing
	if err := d.MarkFailed(102, false, "error:cannot-fetch", "capture could not be fetched"); err != nil {
		t.Fatalf("MarkFailed 102: %v", err)
	}
	transient, err := d.ListTransient()
	if err != nil || len(transient) != 1 || transient[0].RaindropID != 102 {
		t.Fatalf("ListTransient: got %v, %v", transient, err)
	}

	// stats
	// 101 archived+synced, 102 failed_transient, 103 archived+unsyncable
	stats, err := d.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	want := Stats{
		Pending: 0, Archived: 2, FailedPermanent: 0, FailedTransient: 1,
		SyncedBack: 1, Unsynced: 0, Unsyncable: 1,
	}
	if stats != want {
		t.Fatalf("Stats: got %+v, want %+v", stats, want)
	}

	t.Log("all db assertions passed")
}

func TestListPendingNewestFirst(t *testing.T) {
	d, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	for _, bookmark := range []struct {
		id      int64
		created time.Time
	}{
		{101, time.Date(2026, time.July, 29, 0, 0, 0, 0, time.UTC)},
		{102, time.Date(2026, time.July, 30, 0, 0, 0, 0, time.UTC)},
		{103, time.Date(2026, time.July, 28, 0, 0, 0, 0, time.UTC)},
	} {
		if err := d.UpsertPending(bookmark.id, "https://example.com", bookmark.created); err != nil {
			t.Fatalf("insert %d: %v", bookmark.id, err)
		}
	}

	pending, err := d.ListPendingNewestFirst()
	if err != nil {
		t.Fatalf("ListPendingNewestFirst: %v", err)
	}
	got := []int64{pending[0].RaindropID, pending[1].RaindropID, pending[2].RaindropID}
	want := []int64{102, 101, 103}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pending order = %v, want %v", got, want)
		}
	}
}
