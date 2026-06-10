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
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}

	configPath := flag.String("config", "./config.yaml", "path to config.yaml")
	syncOnly := flag.Bool("sync-only", false, "write archive URLs to Raindrop notes without re-archiving")
	retryFailed := flag.Bool("retry-failed", false, "also retry previously failed (transient) bookmarks")
	flag.Parse()

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

	a := archiver.New(
		cfg,
		database,
		raindrop.NewClient(cfg.RaindropToken, cfg.RateLimitMs),
		wayback.NewClient(cfg.WaybackAccessKey, cfg.WaybackSecretKey),
	)

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
