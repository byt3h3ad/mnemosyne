package archiver

import (
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
	Fetched        int
	Archived       int
	FailedPermanent int
	FailedTransient int
	SyncedBack     int
}

func (s Summary) Print() {
	fmt.Printf("Fetched:     %4d bookmarks\n", s.Fetched)
	fmt.Printf("Archived:    %4d\n", s.Archived)
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
func (a *Archiver) SyncBack() (int, error) {
	unsynced, err := a.db.ListUnsynced()
	if err != nil {
		return 0, fmt.Errorf("list unsynced: %w", err)
	}

	log.Printf("%d bookmarks to sync back", len(unsynced))

	synced := 0
	for i, b := range unsynced {
		log.Printf("  syncing back raindrop %d", b.RaindropID)
		if err := a.raindrop.AppendNote(b.RaindropID, b.ArchiveURL); err != nil {
			log.Printf("  sync-back failed (will retry next run): %v", err)
		} else {
			if err := a.db.MarkSynced(b.RaindropID); err != nil {
				log.Printf("  db mark synced error: %v", err)
			} else {
				synced++
			}
		}
		if i < len(unsynced)-1 {
			time.Sleep(time.Duration(a.cfg.RateLimitMs) * time.Millisecond)
		}
	}

	return synced, nil
}

func (a *Archiver) Run() (Summary, error) {
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

	// Reset any transient failures from the previous run so they're retried.
	if err := a.db.ResetTransient(); err != nil {
		return Summary{}, fmt.Errorf("reset transient: %w", err)
	}

	// --- 2. Fetch bookmarks ---
	var bookmarks []raindrop.Bookmark
	if isFirstRun {
		log.Println("first run: fetching all bookmarks")
		bookmarks, err = a.raindrop.FetchAll()
	} else {
		log.Printf("incremental run: fetching bookmarks since %s", lastRunAt.Format(time.RFC3339))
		bookmarks, err = a.raindrop.FetchSince(lastRunAt)
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

	for i, b := range pending {
		log.Printf("[%d/%d] archiving %s", i+1, len(pending), b.OriginalURL)

		result, archiveErr := a.wayback.Archive(b.OriginalURL)
		if archiveErr != nil {
			var permErr *wayback.PermanentError
			if errors.As(archiveErr, &permErr) {
				log.Printf("  permanent failure: %s", permErr.StatusExt)
				if err := a.db.MarkFailed(b.RaindropID, true, permErr.StatusExt); err != nil {
					log.Printf("  db error: %v", err)
				}
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
			}
		} else {
			log.Printf("  archived: %s", result.ArchiveURL)
			if err := a.db.MarkArchived(b.RaindropID, result.ArchiveURL); err != nil {
				log.Printf("  db error: %v", err)
			}
		}

		// Rate limit between submissions, not between polls.
		if i < len(pending)-1 {
			time.Sleep(time.Duration(a.cfg.RateLimitMs) * time.Millisecond)
		}
	}

	// --- 4. Sync archive URLs back to Raindrop ---
	unsynced, err := a.db.ListUnsynced()
	if err != nil {
		return Summary{}, fmt.Errorf("list unsynced: %w", err)
	}

	log.Printf("%d bookmarks to sync back", len(unsynced))

	for i, b := range unsynced {
		log.Printf("  syncing back raindrop %d", b.RaindropID)
		if err := a.raindrop.AppendNote(b.RaindropID, b.ArchiveURL); err != nil {
			log.Printf("  sync-back failed (will retry next run): %v", err)
		} else {
			if err := a.db.MarkSynced(b.RaindropID); err != nil {
				log.Printf("  db mark synced error: %v", err)
			}
		}
		// Raindrop allows 120 req/min; AppendNote costs 2 (GET + PUT).
		if i < len(unsynced)-1 {
			time.Sleep(time.Duration(a.cfg.RateLimitMs) * time.Millisecond)
		}
	}

	// --- 5. Finalise run state ---

	now := time.Now().UTC().Format(time.RFC3339)
	if err := a.db.SetState("last_run_at", now); err != nil {
		return Summary{}, fmt.Errorf("set last_run_at: %w", err)
	}
	if err := a.db.SetState("first_run", "0"); err != nil {
		return Summary{}, fmt.Errorf("set first_run: %w", err)
	}

	// --- 6. Build summary from DB ---
	archived, failedPerm, failedTrans, err := a.db.Counts()
	if err != nil {
		return Summary{}, fmt.Errorf("counts: %w", err)
	}
	synced, err := a.db.CountSynced()
	if err != nil {
		return Summary{}, fmt.Errorf("count synced: %w", err)
	}

	return Summary{
		Fetched:         fetched,
		Archived:        archived,
		FailedPermanent: failedPerm,
		FailedTransient: failedTrans,
		SyncedBack:      synced,
	}, nil
}
