package archiver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/byt3h3ad/mnemosyne/internal/config"
	"github.com/byt3h3ad/mnemosyne/internal/db"
	"github.com/byt3h3ad/mnemosyne/internal/raindrop"
	"github.com/byt3h3ad/mnemosyne/internal/wayback"
)

type Summary struct {
	Fetched         int
	Archived        int // new captures made this run
	Reused          int // existing recent captures reused (skip_archived_within_days)
	FailedPermanent int
	FailedTransient int
	SyncedBack      int
}

func (s Summary) Print() {
	fmt.Printf("Fetched:     %4d bookmarks\n", s.Fetched)
	if s.Reused > 0 {
		fmt.Printf("Archived:    %4d  (%d new, %d reused)\n", s.Archived+s.Reused, s.Archived, s.Reused)
	} else {
		fmt.Printf("Archived:    %4d\n", s.Archived)
	}
	fmt.Printf("Failed:      %4d  (%d permanent, %d transient)\n",
		s.FailedPermanent+s.FailedTransient, s.FailedPermanent, s.FailedTransient)
	fmt.Printf("Synced back: %4d\n", s.SyncedBack)
}

type Archiver struct {
	cfg      *config.Config
	db       *db.DB
	raindrop *raindrop.Client
	wayback  *wayback.Client
}

func New(cfg *config.Config, database *db.DB, rd *raindrop.Client, wb *wayback.Client) *Archiver {
	return &Archiver{cfg: cfg, db: database, raindrop: rd, wayback: wb}
}

// SyncBack writes archive URLs to Raindrop notes for all unsynced archived rows.
// Returns the number of bookmarks successfully synced.
func (a *Archiver) SyncBack(ctx context.Context) (int, error) {
	return a.doSyncBack(ctx)
}

func (a *Archiver) Run(ctx context.Context, retryFailed bool) (Summary, error) {
	// Record start time before any work so bookmarks created during the run
	// are captured by the next incremental run.
	runStart := time.Now().UTC()

	// --- 1. Read run state ---
	firstRunVal, err := a.db.GetState("first_run")
	if err != nil {
		return Summary{}, fmt.Errorf("read first_run: %w", err)
	}
	isFirstRun := firstRunVal != "0"

	lastRunVal, err := a.db.GetState("last_run_at")
	if err != nil {
		return Summary{}, fmt.Errorf("read last_run_at: %w", err)
	}
	var lastRunAt time.Time
	if lastRunVal != "" {
		lastRunAt, err = time.Parse(time.RFC3339, lastRunVal)
		if err != nil {
			return Summary{}, fmt.Errorf("parse last_run_at: %w", err)
		}
	}

	// Only reset transient failures when explicitly requested.
	if retryFailed {
		if err := a.db.ResetTransient(); err != nil {
			return Summary{}, fmt.Errorf("reset transient: %w", err)
		}
	}

	// --- 2. Fetch bookmarks ---
	var bookmarks []raindrop.Bookmark
	if isFirstRun {
		log.Println("first run: fetching all bookmarks")
		bookmarks, err = a.raindrop.FetchAll(ctx)
	} else {
		log.Printf("incremental run: fetching bookmarks since %s", lastRunAt.Format(time.RFC3339))
		bookmarks, err = a.raindrop.FetchSince(ctx, lastRunAt)
	}
	if err != nil {
		return Summary{}, fmt.Errorf("fetch bookmarks: %w", err)
	}

	fetched := len(bookmarks)
	log.Printf("fetched %d bookmarks", fetched)

	for _, b := range bookmarks {
		if err := a.db.UpsertPending(b.ID, b.URL); err != nil {
			return Summary{}, fmt.Errorf("upsert bookmark %d: %w", b.ID, err)
		}
	}

	// --- 3. Archive loop ---
	pending, err := a.db.ListPending()
	if err != nil {
		return Summary{}, fmt.Errorf("list pending: %w", err)
	}

	log.Printf("%d URLs to archive", len(pending))

	var archivedCount, reusedCount, failedPermCount, failedTransCount int
	maxCaptureAge := time.Duration(a.cfg.SkipArchivedWithinDays) * 24 * time.Hour

	for i, b := range pending {
		if ctx.Err() != nil {
			log.Println("interrupted — remaining bookmarks stay pending for the next run")
			break
		}

		log.Printf("[%d/%d] archiving %s", i+1, len(pending), b.OriginalURL)

		if maxCaptureAge > 0 {
			if existing, ok := a.wayback.FindRecent(ctx, b.OriginalURL, maxCaptureAge); ok {
				log.Printf("  reusing existing capture: %s", existing)
				if err := a.db.MarkArchived(b.RaindropID, existing); err != nil {
					log.Printf("  db error: %v", err)
				}
				reusedCount++
				if i < len(pending)-1 {
					sleepCtx(ctx, time.Duration(a.cfg.RateLimitMs)*time.Millisecond)
				}
				continue
			}
		}

		result, archiveErr := a.wayback.Archive(ctx, b.OriginalURL)
		if archiveErr != nil {
			// Don't record an interrupted attempt as a failure — leave it pending.
			if ctx.Err() != nil {
				log.Println("interrupted — remaining bookmarks stay pending for the next run")
				break
			}
			var permErr *wayback.PermanentError
			if errors.As(archiveErr, &permErr) {
				log.Printf("  permanent failure: %s", permErr.StatusExt)
				if err := a.db.MarkFailed(b.RaindropID, true, permErr.StatusExt); err != nil {
					log.Printf("  db error: %v", err)
				}
				failedPermCount++
			} else {
				var transErr *wayback.TransientError
				errors.As(archiveErr, &transErr)
				msg := archiveErr.Error()
				ext := ""
				if transErr != nil {
					ext = transErr.StatusExt
					msg = transErr.Message
				}
				log.Printf("  transient failure: %s", msg)
				if err := a.db.MarkFailed(b.RaindropID, false, ext); err != nil {
					log.Printf("  db error: %v", err)
				}
				failedTransCount++
			}
		} else {
			log.Printf("  archived: %s", result.ArchiveURL)
			if err := a.db.MarkArchived(b.RaindropID, result.ArchiveURL); err != nil {
				log.Printf("  db error: %v", err)
			}
			archivedCount++
		}

		if i < len(pending)-1 {
			sleepCtx(ctx, time.Duration(a.cfg.RateLimitMs)*time.Millisecond)
		}
	}

	// --- 4. Sync archive URLs back to Raindrop ---
	var syncedCount int
	if ctx.Err() == nil {
		syncedCount, err = a.doSyncBack(ctx)
		if err != nil {
			return Summary{}, err
		}
	}

	// --- 5. Finalise run state ---
	// Safe even when interrupted: everything up to runStart was fetched and
	// upserted, and unprocessed rows stay pending for the next run.
	if err := a.db.SetState("last_run_at", runStart.Format(time.RFC3339)); err != nil {
		return Summary{}, fmt.Errorf("set last_run_at: %w", err)
	}
	if err := a.db.SetState("first_run", "0"); err != nil {
		return Summary{}, fmt.Errorf("set first_run: %w", err)
	}

	return Summary{
		Fetched:         fetched,
		Archived:        archivedCount,
		Reused:          reusedCount,
		FailedPermanent: failedPermCount,
		FailedTransient: failedTransCount,
		SyncedBack:      syncedCount,
	}, nil
}

// DryRun reports what a real run would do without writing to the DB,
// the Wayback Machine, or Raindrop. Only read-only Raindrop calls are made.
func (a *Archiver) DryRun(ctx context.Context, retryFailed bool) error {
	firstRunVal, err := a.db.GetState("first_run")
	if err != nil {
		return fmt.Errorf("read first_run: %w", err)
	}
	isFirstRun := firstRunVal != "0"

	lastRunVal, err := a.db.GetState("last_run_at")
	if err != nil {
		return fmt.Errorf("read last_run_at: %w", err)
	}
	var lastRunAt time.Time
	if lastRunVal != "" {
		lastRunAt, err = time.Parse(time.RFC3339, lastRunVal)
		if err != nil {
			return fmt.Errorf("parse last_run_at: %w", err)
		}
	}

	var bookmarks []raindrop.Bookmark
	if isFirstRun {
		log.Println("first run: fetching all bookmarks")
		bookmarks, err = a.raindrop.FetchAll(ctx)
	} else {
		log.Printf("incremental run: fetching bookmarks since %s", lastRunAt.Format(time.RFC3339))
		bookmarks, err = a.raindrop.FetchSince(ctx, lastRunAt)
	}
	if err != nil {
		return fmt.Errorf("fetch bookmarks: %w", err)
	}

	// Classify what a real run would archive, deduplicated by raindrop ID.
	type candidate struct {
		url    string
		reason string
	}
	seen := map[int64]bool{}
	var wouldArchive []candidate
	var alreadyArchived, skippedPermanent int

	add := func(id int64, url, reason string) {
		if !seen[id] {
			seen[id] = true
			wouldArchive = append(wouldArchive, candidate{url: url, reason: reason})
		}
	}

	for _, b := range bookmarks {
		status, err := a.db.StatusOf(b.ID)
		if err != nil {
			return fmt.Errorf("status of bookmark %d: %w", b.ID, err)
		}
		switch status {
		case "":
			add(b.ID, b.URL, "new")
		case "pending":
			add(b.ID, b.URL, "pending from previous run")
		case "failed_transient":
			// UpsertPending resets re-fetched transients even without --retry-failed.
			add(b.ID, b.URL, "transient retry")
		case "archived":
			alreadyArchived++
		case "failed_permanent":
			skippedPermanent++
		}
	}

	pending, err := a.db.ListPending()
	if err != nil {
		return fmt.Errorf("list pending: %w", err)
	}
	for _, b := range pending {
		add(b.RaindropID, b.OriginalURL, "pending from previous run")
	}

	if retryFailed {
		transient, err := a.db.ListTransient()
		if err != nil {
			return fmt.Errorf("list transient: %w", err)
		}
		for _, b := range transient {
			add(b.RaindropID, b.OriginalURL, "transient retry")
		}
	}

	unsynced, err := a.db.ListUnsynced()
	if err != nil {
		return fmt.Errorf("list unsynced: %w", err)
	}

	fmt.Println("\nDRY RUN — nothing was written")
	fmt.Printf("Fetched:        %4d bookmarks\n", len(bookmarks))
	fmt.Printf("Would archive:  %4d URLs\n", len(wouldArchive))
	for _, c := range wouldArchive {
		fmt.Printf("  %s  (%s)\n", c.url, c.reason)
	}
	fmt.Printf("Would sync back: %3d bookmarks\n", len(unsynced))
	if alreadyArchived > 0 || skippedPermanent > 0 {
		fmt.Printf("Skipped:        %4d  (%d already archived, %d permanently failed)\n",
			alreadyArchived+skippedPermanent, alreadyArchived, skippedPermanent)
	}
	if a.cfg.SkipArchivedWithinDays > 0 {
		fmt.Printf("Note: captures newer than %d days will be reused instead of re-archived\n",
			a.cfg.SkipArchivedWithinDays)
	}
	return nil
}

// doSyncBack is the shared implementation used by both Run and SyncBack.
func (a *Archiver) doSyncBack(ctx context.Context) (int, error) {
	unsynced, err := a.db.ListUnsynced()
	if err != nil {
		return 0, fmt.Errorf("list unsynced: %w", err)
	}

	log.Printf("%d bookmarks to sync back", len(unsynced))

	synced := 0
	for i, b := range unsynced {
		if ctx.Err() != nil {
			log.Println("interrupted — remaining sync-backs will be retried next run")
			break
		}

		log.Printf("  syncing back raindrop %d", b.RaindropID)
		if err := a.raindrop.AppendNote(ctx, b.RaindropID, b.ArchiveURL); err != nil {
			var permErr *raindrop.PermanentSyncError
			switch {
			case errors.As(err, &permErr):
				log.Printf("  sync-back permanently failed (%s) — will not retry", permErr.Reason)
				if dbErr := a.db.MarkSyncFailedPermanent(b.RaindropID, permErr.Reason); dbErr != nil {
					log.Printf("  db error: %v", dbErr)
				}
			case ctx.Err() != nil:
				log.Println("interrupted — remaining sync-backs will be retried next run")
			default:
				log.Printf("  sync-back failed (will retry next run): %v", err)
			}
		} else {
			if err := a.db.MarkSynced(b.RaindropID); err != nil {
				log.Printf("  db mark synced error: %v", err)
			} else {
				synced++
			}
		}
		// Raindrop allows 120 req/min; AppendNote costs 2 (GET + PUT).
		if i < len(unsynced)-1 {
			sleepCtx(ctx, time.Duration(a.cfg.RateLimitMs)*time.Millisecond)
		}
	}

	return synced, nil
}

// sleepCtx sleeps for d or until ctx is cancelled, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
