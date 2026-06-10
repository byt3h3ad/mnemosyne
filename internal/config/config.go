package config

import (
	"errors"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	RaindropToken    string `yaml:"raindrop_token"`
	WaybackAccessKey string `yaml:"wayback_access_key"`
	WaybackSecretKey string `yaml:"wayback_secret_key"`
	DBPath           string `yaml:"db_path"`
	RateLimitMs      int    `yaml:"rate_limit_ms"`
	// SkipArchivedWithinDays reuses an existing Wayback capture instead of
	// making a new one if it is at most this many days old. 0 disables.
	SkipArchivedWithinDays int `yaml:"skip_archived_within_days"`
}

func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cfg Config
	if err := yaml.NewDecoder(f).Decode(&cfg); err != nil {
		return nil, err
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *Config) validate() error {
	if c.RaindropToken == "" {
		return errors.New("raindrop_token is required")
	}
	if c.WaybackAccessKey == "" {
		return errors.New("wayback_access_key is required")
	}
	if c.WaybackSecretKey == "" {
		return errors.New("wayback_secret_key is required")
	}
	if c.DBPath == "" {
		c.DBPath = "./archive.db"
	}
	if c.RateLimitMs <= 0 {
		c.RateLimitMs = 2000
	}
	if c.SkipArchivedWithinDays < 0 {
		return errors.New("skip_archived_within_days must be >= 0")
	}
	return nil
}
