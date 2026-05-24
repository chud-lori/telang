package main

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/telang/telang/internal/keys"
)

func runInit(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	configPath := fs.String("config", "/etc/telang/config.toml", "path to write config.toml")
	dataDir := fs.String("data-dir", "/var/lib/telang", "base data directory for cache/staging/db")
	keysPath := fs.String("keys", "/etc/telang/keys.toml", "path to write keys.toml")
	listen := fs.String("listen", ":9000", "server listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}

	br := bufio.NewReader(stdin)
	prompt := func(label, def string) (string, error) {
		fmt.Fprintf(stdout, "%s", label)
		if def != "" {
			fmt.Fprintf(stdout, " [%s]", def)
		}
		fmt.Fprint(stdout, ": ")
		line, err := br.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			return def, nil
		}
		return line, nil
	}

	fmt.Fprintln(stdout, "telang init — Telegram-backed S3 setup")
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "1) Choose a storage mode.")
	fmt.Fprintln(stdout, "   bot      — up to 20 MB per object, simplest setup (recommended for v0.2).")
	fmt.Fprintln(stdout, "   mtproto  — up to 2 GB per object (coming in v0.3, not yet supported).")
	mode, err := prompt("mode", "bot")
	if err != nil {
		return err
	}
	if mode != "bot" {
		return fmt.Errorf("only bot mode is supported in this release (got %q)", mode)
	}

	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "2) Telegram credentials.")
	fmt.Fprintln(stdout, "   - Talk to @BotFather to create a bot and paste its token below.")
	fmt.Fprintln(stdout, "   - Create a private Telegram channel, add the bot as admin, and copy its ID")
	fmt.Fprintln(stdout, "     (numeric, usually starts with -100).")
	botToken, err := prompt("bot token", "")
	if err != nil {
		return err
	}
	if botToken == "" {
		return errors.New("bot token is required")
	}
	channelStr, err := prompt("channel id", "")
	if err != nil {
		return err
	}
	channelID, err := strconv.ParseInt(channelStr, 10, 64)
	if err != nil || channelID == 0 {
		return fmt.Errorf("channel id must be a non-zero integer (got %q)", channelStr)
	}

	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "3) Server.")
	addr, err := prompt("listen address", *listen)
	if err != nil {
		return err
	}

	ak, sk, err := newS3Credentials()
	if err != nil {
		return err
	}

	cacheDir := filepath.Join(*dataDir, "cache")
	stagingDir := filepath.Join(*dataDir, "staging")
	dbPath := filepath.Join(*dataDir, "telang.db")

	for _, d := range []string{*dataDir, cacheDir, stagingDir, filepath.Dir(dbPath), filepath.Dir(*configPath), filepath.Dir(*keysPath)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	configBody := renderConfig(addr, ak, sk, botToken, channelID, dbPath, stagingDir, cacheDir, *keysPath)
	if err := writeFileAtomic(*configPath, []byte(configBody), 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	// Pre-create keys.toml with secure perms; the first bucket key is
	// generated on demand the first time a bucket is created via the S3 API.
	if _, err := keys.Load(*keysPath); err != nil {
		return fmt.Errorf("init keys file: %w", err)
	}

	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "✓ Setup complete.")
	fmt.Fprintf(stdout, "  config:   %s\n", *configPath)
	fmt.Fprintf(stdout, "  keys:     %s   (BACK THIS UP OUT OF BAND — losing it = losing the data)\n", *keysPath)
	fmt.Fprintf(stdout, "  data dir: %s\n", *dataDir)
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "S3 credentials (save these — they are not stored anywhere else):")
	fmt.Fprintf(stdout, "  access_key = %s\n", ak)
	fmt.Fprintf(stdout, "  secret_key = %s\n", sk)
	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "Start the daemon:")
	fmt.Fprintf(stdout, "  telang serve --config %s\n", *configPath)
	return nil
}

func newS3Credentials() (string, string, error) {
	akRaw := make([]byte, 12)
	if _, err := rand.Read(akRaw); err != nil {
		return "", "", err
	}
	skRaw := make([]byte, 30)
	if _, err := rand.Read(skRaw); err != nil {
		return "", "", err
	}
	ak := "AKIA" + strings.ToUpper(strings.TrimRight(base64.StdEncoding.EncodeToString(akRaw), "="))
	if len(ak) > 20 {
		ak = ak[:20]
	}
	sk := strings.TrimRight(base64.StdEncoding.EncodeToString(skRaw), "=")
	return ak, sk, nil
}

func renderConfig(listen, ak, sk, botToken string, channelID int64, db, staging, cache, keysPath string) string {
	return fmt.Sprintf(`[server]
listen = %q

[s3]
access_key = %q
secret_key = %q
region     = "tg-1"

[telegram]
mode       = "bot"
bot_token  = %q
channel_id = %d

[storage]
db_path     = %q
cache_dir   = %q
cache_size  = "5GB"
staging_dir = %q

[encryption]
keys_file = %q
`, listen, ak, sk, botToken, channelID, db, cache, staging, keysPath)
}

// writeFileAtomic writes data to path via tmp+rename so an interrupted init
// can't leave a half-written config.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "telang-init-*.tmp")
	if err != nil {
		return err
	}
	cleanup := func() { _ = os.Remove(tmp.Name()) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		cleanup()
		return err
	}
	return nil
}
