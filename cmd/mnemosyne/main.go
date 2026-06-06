package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/byt3h3ad/mnemosyne/internal/archiver"
	"github.com/byt3h3ad/mnemosyne/internal/config"
	"github.com/byt3h3ad/mnemosyne/internal/db"
	"github.com/byt3h3ad/mnemosyne/internal/raindrop"
	"github.com/byt3h3ad/mnemosyne/internal/wayback"
)

func main() {
	configPath := flag.String("config", "./config.yaml", "path to config.yaml")
	syncOnly := flag.Bool("sync-only", false, "write archive URLs to Raindrop notes without re-archiving")
	flag.Parse()

	log.SetFlags(log.Ltime)

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
		raindrop.NewClient(cfg.RaindropToken),
		wayback.NewClient(cfg.WaybackAccessKey, cfg.WaybackSecretKey),
	)

	if *syncOnly {
		n, err := a.SyncBack()
		if err != nil {
			fmt.Fprintln(os.Stderr, "sync-back failed:", err)
			os.Exit(1)
		}
		fmt.Printf("\nSynced back: %d\n", n)
		return
	}

	summary, err := a.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "run failed:", err)
		os.Exit(1)
	}

	fmt.Println()
	summary.Print()
}
