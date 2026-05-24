package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/telang/telang/internal/config"
)

func TestInitWritesConfigAndKeysFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	keysPath := filepath.Join(dir, "keys.toml")
	dataDir := filepath.Join(dir, "data")

	// Canned input: mode, bot_token, channel_id, listen address.
	input := strings.NewReader("\n12345:abcdef\n-1001234567890\n\n")
	var out bytes.Buffer

	err := runInit([]string{
		"--config", cfgPath,
		"--keys", keysPath,
		"--data-dir", dataDir,
	}, input, &out)
	if err != nil {
		t.Fatalf("runInit: %v\noutput:\n%s", err, out.String())
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Telegram.Mode != config.ModeBot {
		t.Fatalf("mode: %q", cfg.Telegram.Mode)
	}
	if cfg.Telegram.BotToken != "12345:abcdef" {
		t.Fatalf("bot token: %q", cfg.Telegram.BotToken)
	}
	if cfg.Telegram.ChannelID != -1001234567890 {
		t.Fatalf("channel id: %d", cfg.Telegram.ChannelID)
	}
	if cfg.S3.AccessKey == "" || cfg.S3.SecretKey == "" {
		t.Fatal("S3 credentials missing")
	}

	// keys.toml must exist with chmod 600.
	info, err := os.Stat(keysPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("keys file perm: %o", info.Mode().Perm())
	}

	// config.toml must also be chmod 600 (carries the bot token).
	info, err = os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config file perm: %o", info.Mode().Perm())
	}

	// The S3 credentials should be echoed back in the output for the user
	// to record (they aren't stored elsewhere).
	if !strings.Contains(out.String(), "access_key = "+cfg.S3.AccessKey) {
		t.Fatalf("output missing access_key echo:\n%s", out.String())
	}
}

func TestInitRejectsMTProto(t *testing.T) {
	dir := t.TempDir()
	input := strings.NewReader("mtproto\n")
	var out bytes.Buffer
	err := runInit([]string{
		"--config", filepath.Join(dir, "c.toml"),
		"--keys", filepath.Join(dir, "k.toml"),
		"--data-dir", filepath.Join(dir, "d"),
	}, input, &out)
	if err == nil || !strings.Contains(err.Error(), "only bot mode") {
		t.Fatalf("want mtproto-rejection, got err=%v", err)
	}
}
