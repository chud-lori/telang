package main

import (
	"bufio"
	"context"
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
	"github.com/telang/telang/internal/storage/mtproto"
)

type initInputs struct {
	configPath string
	dataDir    string
	keysPath   string
	listen     string

	stdin  io.Reader
	stdout io.Writer
}

func runInit(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	configPath := fs.String("config", "/etc/telang/config.toml", "path to write config.toml")
	dataDir := fs.String("data-dir", "/var/lib/telang", "base data directory for cache/staging/db")
	keysPath := fs.String("keys", "/etc/telang/keys.toml", "path to write keys.toml")
	listen := fs.String("listen", ":9000", "server listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	in := initInputs{
		configPath: *configPath,
		dataDir:    *dataDir,
		keysPath:   *keysPath,
		listen:     *listen,
		stdin:      stdin,
		stdout:     stdout,
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
	fmt.Fprintln(stdout, "   bot      — up to 20 MB per object; no phone number needed.")
	fmt.Fprintln(stdout, "   mtproto  — up to 2 GB per object; uses a user account session.")
	mode, err := prompt("mode", "bot")
	if err != nil {
		return err
	}
	switch mode {
	case "bot":
		return runInitBot(in, prompt, br)
	case "mtproto":
		return runInitMTProto(in, prompt, br)
	default:
		return fmt.Errorf("unsupported mode %q (want \"bot\" or \"mtproto\")", mode)
	}
}

func runInitBot(in initInputs, prompt func(string, string) (string, error), _ *bufio.Reader) error {
	fmt.Fprintln(in.stdout, "")
	fmt.Fprintln(in.stdout, "2) Telegram credentials.")
	fmt.Fprintln(in.stdout, "   - Talk to @BotFather to create a bot and paste its token below.")
	fmt.Fprintln(in.stdout, "   - Create a private Telegram channel, add the bot as admin, and copy its ID")
	fmt.Fprintln(in.stdout, "     (numeric, usually starts with -100).")
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
	return finishInit(in, prompt, func(addr, ak, sk, db, staging, cache, keysPath string) string {
		return renderBotConfig(addr, ak, sk, botToken, channelID, db, staging, cache, keysPath)
	})
}

func runInitMTProto(in initInputs, prompt func(string, string) (string, error), _ *bufio.Reader) error {
	fmt.Fprintln(in.stdout, "")
	fmt.Fprintln(in.stdout, "2) Telegram API credentials.")
	fmt.Fprintln(in.stdout, "   Get an api_id / api_hash pair at https://my.telegram.org → API development tools.")
	apiIDStr, err := prompt("api_id", "")
	if err != nil {
		return err
	}
	apiID, err := strconv.Atoi(apiIDStr)
	if err != nil || apiID == 0 {
		return fmt.Errorf("api_id must be a non-zero integer (got %q)", apiIDStr)
	}
	apiHash, err := prompt("api_hash", "")
	if err != nil {
		return err
	}
	if apiHash == "" {
		return errors.New("api_hash is required")
	}

	sessionFile := filepath.Join(in.dataDir, "session")
	if err := os.MkdirAll(in.dataDir, 0o700); err != nil {
		return fmt.Errorf("mkdir data dir: %w", err)
	}

	fmt.Fprintln(in.stdout, "")
	fmt.Fprintln(in.stdout, "3) Sign in.")
	fmt.Fprintln(in.stdout, "   Telegram will text or in-app message a one-time code.")

	fmt.Fprintln(in.stdout, "")
	fmt.Fprintln(in.stdout, "4) Storage channel.")
	fmt.Fprintln(in.stdout, "   Create a private channel in the Telegram app, give it a public-looking")
	fmt.Fprintln(in.stdout, "   @username (you can keep the channel itself private — Telegram still allows")
	fmt.Fprintln(in.stdout, "   resolution by name), then paste its handle below.")
	channelUsername, err := prompt("channel @username", "")
	if err != nil {
		return err
	}
	if channelUsername == "" {
		return errors.New("channel username is required to resolve the channel access hash")
	}

	resolution, err := mtproto.InteractiveAuth(context.Background(), mtproto.AuthOptions{
		APIID:       apiID,
		APIHash:     apiHash,
		SessionFile: sessionFile,
		Stdin:       in.stdin,
		Stdout:      in.stdout,
	}, channelUsername)
	if err != nil {
		return err
	}
	if resolution == nil {
		return errors.New("channel resolution returned nothing")
	}

	return finishInit(in, prompt, func(addr, ak, sk, db, staging, cache, keysPath string) string {
		return renderMTProtoConfig(addr, ak, sk, apiID, apiHash, sessionFile, resolution.ChannelID, resolution.AccessHash, db, staging, cache, keysPath)
	})
}

func finishInit(in initInputs, prompt func(string, string) (string, error), render func(addr, ak, sk, db, staging, cache, keysPath string) string) error {
	fmt.Fprintln(in.stdout, "")
	fmt.Fprintln(in.stdout, "5) Server.")
	addr, err := prompt("listen address", in.listen)
	if err != nil {
		return err
	}

	ak, sk, err := newS3Credentials()
	if err != nil {
		return err
	}

	cacheDir := filepath.Join(in.dataDir, "cache")
	stagingDir := filepath.Join(in.dataDir, "staging")
	dbPath := filepath.Join(in.dataDir, "telang.db")
	for _, d := range []string{in.dataDir, cacheDir, stagingDir, filepath.Dir(dbPath), filepath.Dir(in.configPath), filepath.Dir(in.keysPath)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	body := render(addr, ak, sk, dbPath, stagingDir, cacheDir, in.keysPath)
	if err := writeFileAtomic(in.configPath, []byte(body), 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if _, err := keys.Load(in.keysPath); err != nil {
		return fmt.Errorf("init keys file: %w", err)
	}

	fmt.Fprintln(in.stdout, "")
	fmt.Fprintln(in.stdout, "✓ Setup complete.")
	fmt.Fprintf(in.stdout, "  config:   %s\n", in.configPath)
	fmt.Fprintf(in.stdout, "  keys:     %s   (BACK THIS UP OUT OF BAND — losing it = losing the data)\n", in.keysPath)
	fmt.Fprintf(in.stdout, "  data dir: %s\n", in.dataDir)
	fmt.Fprintln(in.stdout, "")
	fmt.Fprintln(in.stdout, "S3 credentials (save these — they are not stored anywhere else):")
	fmt.Fprintf(in.stdout, "  access_key = %s\n", ak)
	fmt.Fprintf(in.stdout, "  secret_key = %s\n", sk)
	fmt.Fprintln(in.stdout, "")
	fmt.Fprintln(in.stdout, "Start the daemon:")
	fmt.Fprintf(in.stdout, "  telang serve --config %s\n", in.configPath)
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

func renderBotConfig(listen, ak, sk, botToken string, channelID int64, db, staging, cache, keysPath string) string {
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

func renderMTProtoConfig(listen, ak, sk string, apiID int, apiHash, sessionFile string, channelID, accessHash int64, db, staging, cache, keysPath string) string {
	return fmt.Sprintf(`[server]
listen = %q

[s3]
access_key = %q
secret_key = %q
region     = "tg-1"

[telegram]
mode                  = "mtproto"
api_id                = %d
api_hash              = %q
session_file          = %q
channel_id            = %d
channel_access_hash   = %d

[storage]
db_path     = %q
cache_dir   = %q
cache_size  = "5GB"
staging_dir = %q

[encryption]
keys_file = %q
`, listen, ak, sk, apiID, apiHash, sessionFile, channelID, accessHash, db, cache, staging, keysPath)
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
