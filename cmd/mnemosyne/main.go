package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/byt3h3ad/mnemosyne/internal/archiver"
	"github.com/byt3h3ad/mnemosyne/internal/config"
	"github.com/byt3h3ad/mnemosyne/internal/db"
	"github.com/byt3h3ad/mnemosyne/internal/raindrop"
	"github.com/byt3h3ad/mnemosyne/internal/wayback"
)

// version is set at build time via -ldflags="-X 'main.version=<tag>'".
var version = "dev"

func main() {
	// Optional subcommand comes before flags: mnemo [version|status] [flags]
	args := os.Args[1:]
	command := ""
	if len(args) > 0 && (args[0] == "version" || args[0] == "status") {
		command = args[0]
		args = args[1:]
	}

	if command == "version" {
		fmt.Println(version)
		return
	}

	configPath := flag.String("config", "./config.yaml", "path to config.yaml")
	syncOnly := flag.Bool("sync-only", false, "write archive URLs to Raindrop notes without re-archiving")
	retryFailed := flag.Bool("retry-failed", false, "also retry previously failed (transient) bookmarks")
	dryRun := flag.Bool("dry-run", false, "report what would be archived without writing anything")
	flag.CommandLine.Parse(args)

	if *dryRun && *syncOnly {
		fmt.Fprintln(os.Stderr, "--dry-run cannot be combined with --sync-only")
		os.Exit(1)
	}

	log.SetFlags(log.Ltime)

	// On SIGINT/SIGTERM the context is cancelled: in-flight work stops at the
	// next checkpoint, state is finalised, and deferred closes run normally.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}

	database, err := db.Open(cfg.DBPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "db:", err)
		os.Exit(1)
	}
	defer database.Close()

	if command == "status" {
		if err := printStatus(database); err != nil {
			fmt.Fprintln(os.Stderr, "status:", err)
			os.Exit(1)
		}
		return
	}

	a := archiver.New(
		cfg,
		database,
		raindrop.NewClient(cfg.RaindropToken, cfg.RateLimitMs),
		wayback.NewClient(cfg.WaybackAccessKey, cfg.WaybackSecretKey),
	)

	if *dryRun {
		if err := a.DryRun(ctx, *retryFailed); err != nil {
			fmt.Fprintln(os.Stderr, "dry run failed:", err)
			os.Exit(1)
		}
		return
	}

	if *syncOnly {
		n, err := a.SyncBack(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, "sync-back failed:", err)
			os.Exit(1)
		}
		fmt.Printf("\nSynced back: %d\n", n)
		return
	}

	summary, err := a.Run(ctx, *retryFailed)
	if err != nil {
		fmt.Fprintln(os.Stderr, "run failed:", err)
		os.Exit(1)
	}

	fmt.Println()
	summary.Print()
}

// printStatus reports DB state without making any API calls.
func printStatus(database *db.DB) error {
	stats, err := database.Stats()
	if err != nil {
		return err
	}
	lastRun, err := database.GetState("last_run_at")
	if err != nil {
		return err
	}
	firstRun, err := database.GetState("first_run")
	if err != nil {
		return err
	}

	fmt.Printf("Pending:          %4d\n", stats.Pending)
	fmt.Printf("Archived:         %4d\n", stats.Archived)
	fmt.Printf("  synced back:    %4d\n", stats.SyncedBack)
	fmt.Printf("  unsynced:       %4d\n", stats.Unsynced)
	fmt.Printf("  unsyncable:     %4d\n", stats.Unsyncable)
	fmt.Printf("Failed permanent: %4d\n", stats.FailedPermanent)
	fmt.Printf("Failed transient: %4d\n", stats.FailedTransient)

	switch {
	case lastRun != "":
		fmt.Printf("Last run:         %s\n", lastRun)
	case firstRun != "0":
		fmt.Println("Last run:         never (first run pending)")
	}
	return nil
}
