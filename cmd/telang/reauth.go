package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/telang/telang/internal/config"
	"github.com/telang/telang/internal/storage/mtproto"
)

// runReauth refreshes the MTProto session file for an existing config. The
// channel ID + access hash already in the config are kept as-is; we only
// re-prove the user account to Telegram and overwrite session_file.
func runReauth(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("reauth", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/telang/config.toml", "path to existing config.toml")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.Telegram.Mode != config.ModeMTProto {
		return errors.New("reauth is only meaningful in mtproto mode")
	}

	// Move the existing session aside before running auth so a failed
	// re-login can't half-corrupt the on-disk file.
	if _, err := os.Stat(cfg.Telegram.SessionFile); err == nil {
		if err := os.Rename(cfg.Telegram.SessionFile, cfg.Telegram.SessionFile+".bak"); err != nil {
			return fmt.Errorf("backup old session: %w", err)
		}
		defer func() {
			// Roll back if the new file is missing (auth failed mid-flow).
			if _, err := os.Stat(cfg.Telegram.SessionFile); err != nil {
				_ = os.Rename(cfg.Telegram.SessionFile+".bak", cfg.Telegram.SessionFile)
			} else {
				_ = os.Remove(cfg.Telegram.SessionFile + ".bak")
			}
		}()
	}

	fmt.Fprintln(stdout, "telang reauth — refresh MTProto session.")
	fmt.Fprintln(stdout, "Telegram will text or in-app message a one-time code.")
	if _, err := mtproto.InteractiveAuth(context.Background(), mtproto.AuthOptions{
		APIID:       cfg.Telegram.APIID,
		APIHash:     cfg.Telegram.APIHash,
		SessionFile: cfg.Telegram.SessionFile,
		Stdin:       stdin,
		Stdout:      stdout,
	}, ""); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "✓ Session refreshed at", cfg.Telegram.SessionFile)
	return nil
}
