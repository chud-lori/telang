package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server     ServerConfig     `toml:"server"`
	S3         S3Config         `toml:"s3"`
	Telegram   TelegramConfig   `toml:"telegram"`
	Storage    StorageConfig    `toml:"storage"`
	Encryption EncryptionConfig `toml:"encryption"`
	BrowserUI  BrowserUIConfig  `toml:"browser_ui"`
}

type BrowserUIConfig struct {
	Enabled  bool   `toml:"enabled"`
	Password string `toml:"password"`
}

type ServerConfig struct {
	Listen string `toml:"listen"`
}

type S3Config struct {
	AccessKey string `toml:"access_key"`
	SecretKey string `toml:"secret_key"`
	Region    string `toml:"region"`
}

type TelegramMode string

const (
	ModeBot     TelegramMode = "bot"
	ModeMTProto TelegramMode = "mtproto"
)

type TelegramConfig struct {
	Mode TelegramMode `toml:"mode"`

	BotToken  string `toml:"bot_token"`
	ChannelID int64  `toml:"channel_id"`

	SessionFile       string `toml:"session_file"`
	APIID             int    `toml:"api_id"`
	APIHash           string `toml:"api_hash"`
	ChannelAccessHash int64  `toml:"channel_access_hash"`
}

type StorageConfig struct {
	CacheDir   string `toml:"cache_dir"`
	CacheSize  string `toml:"cache_size"`
	StagingDir string `toml:"staging_dir"`
	DBPath     string `toml:"db_path"`
}

type EncryptionConfig struct {
	KeysFile string `toml:"keys_file"`
}

func Load(path string) (*Config, error) {
	var c Config
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := c.applyDefaults(); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() error {
	if c.Server.Listen == "" {
		c.Server.Listen = ":9000"
	}
	if c.S3.Region == "" {
		c.S3.Region = "tg-1"
	}
	if c.Storage.CacheDir == "" {
		c.Storage.CacheDir = "/var/lib/telang/cache"
	}
	if c.Storage.StagingDir == "" {
		c.Storage.StagingDir = "/var/lib/telang/staging"
	}
	if c.Storage.DBPath == "" {
		c.Storage.DBPath = "/var/lib/telang/telang.db"
	}
	if c.Storage.CacheSize == "" {
		c.Storage.CacheSize = "5GB"
	}
	if c.Encryption.KeysFile == "" {
		c.Encryption.KeysFile = "/etc/telang/keys.toml"
	}
	// BrowserUI defaults to enabled; an empty password means read-only.
	if !c.BrowserUI.Enabled && c.BrowserUI.Password == "" {
		c.BrowserUI.Enabled = true
	}
	return nil
}

func (c *Config) validate() error {
	if c.S3.AccessKey == "" || c.S3.SecretKey == "" {
		return errors.New("config: s3.access_key and s3.secret_key are required")
	}
	switch c.Telegram.Mode {
	case ModeBot:
		if c.Telegram.BotToken == "" {
			return errors.New("config: telegram.bot_token required in bot mode")
		}
		if c.Telegram.ChannelID == 0 {
			return errors.New("config: telegram.channel_id required in bot mode")
		}
	case ModeMTProto:
		if c.Telegram.SessionFile == "" {
			return errors.New("config: telegram.session_file required in mtproto mode")
		}
		if c.Telegram.APIID == 0 || c.Telegram.APIHash == "" {
			return errors.New("config: telegram.api_id and api_hash required in mtproto mode")
		}
		if c.Telegram.ChannelID == 0 {
			return errors.New("config: telegram.channel_id required in mtproto mode")
		}
		if c.Telegram.ChannelAccessHash == 0 {
			return errors.New("config: telegram.channel_access_hash required in mtproto mode (set during `telang init`)")
		}
	case "":
		return errors.New("config: telegram.mode is required (\"bot\" or \"mtproto\")")
	default:
		return fmt.Errorf("config: telegram.mode %q is not supported", c.Telegram.Mode)
	}
	if _, err := ParseSize(c.Storage.CacheSize); err != nil {
		return fmt.Errorf("config: storage.cache_size: %w", err)
	}
	return nil
}

// ParseSize parses sizes like "5GB", "512MiB", "1024", and returns bytes.
// Both decimal (KB/MB/GB/TB = 1000^n) and binary (KiB/MiB/GiB/TiB = 1024^n)
// units are accepted. A bare number is interpreted as bytes.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty size")
	}
	cut := len(s)
	for i, r := range s {
		if !(unicode.IsDigit(r) || r == '.') {
			cut = i
			break
		}
	}
	numPart := strings.TrimSpace(s[:cut])
	unitPart := strings.TrimSpace(strings.ToLower(s[cut:]))
	if numPart == "" {
		return 0, fmt.Errorf("size %q has no numeric component", s)
	}
	n, err := strconv.ParseFloat(numPart, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("size %q must be non-negative", s)
	}
	var mult float64
	switch unitPart {
	case "", "b":
		mult = 1
	case "k", "kb":
		mult = 1e3
	case "m", "mb":
		mult = 1e6
	case "g", "gb":
		mult = 1e9
	case "t", "tb":
		mult = 1e12
	case "kib":
		mult = 1 << 10
	case "mib":
		mult = 1 << 20
	case "gib":
		mult = 1 << 30
	case "tib":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("size %q has unknown unit %q", s, unitPart)
	}
	out := n * mult
	if out > float64(1<<62) {
		return 0, fmt.Errorf("size %q overflows int64", s)
	}
	return int64(out), nil
}

// LoadKeys reads a keys.toml of the form `bucket = "base64key"`.
// Returns a map suitable for passing to the encryption layer.
func LoadKeys(path string) (map[string]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("keys: stat %s: %w", path, err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("keys: %s must be chmod 600 (got %o)", path, info.Mode().Perm())
	}
	var m map[string]string
	if _, err := toml.DecodeFile(path, &m); err != nil {
		return nil, fmt.Errorf("keys: read %s: %w", path, err)
	}
	return m, nil
}
