package mtproto

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
)

// AuthOptions parameterise the interactive auth flow used by `telang init`
// and `telang reauth`.
type AuthOptions struct {
	APIID       int
	APIHash     string
	SessionFile string

	Stdin  io.Reader
	Stdout io.Writer
}

// ChannelResolution carries the result of resolving a channel by @username
// after a successful login. The returned access hash is what storage code
// needs to talk to the channel directly via raw API.
type ChannelResolution struct {
	ChannelID  int64
	AccessHash int64
	Title      string
}

// InteractiveAuth runs the user-session auth flow, persists the session
// file, then resolves the channel the caller asks for. The session file is
// written with chmod 600 by gotd/td (see session.FileStorage.StoreSession).
//
// If channelUsername is empty, the resolution step is skipped and the caller
// is expected to wire ChannelResolution{} into config later.
func InteractiveAuth(parent context.Context, opts AuthOptions, channelUsername string) (*ChannelResolution, error) {
	if opts.APIID == 0 || opts.APIHash == "" {
		return nil, errors.New("mtproto: api_id and api_hash are required")
	}
	if opts.SessionFile == "" {
		return nil, errors.New("mtproto: session_file is required")
	}
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if err := os.MkdirAll(filepath.Dir(opts.SessionFile), 0o700); err != nil {
		return nil, fmt.Errorf("mtproto: mkdir session dir: %w", err)
	}

	br := bufio.NewReader(opts.Stdin)
	prompt := func(label string) (string, error) {
		fmt.Fprintf(opts.Stdout, "%s: ", label)
		line, err := br.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}

	authenticator := terminalAuthenticator{prompt: prompt}

	storage := &session.FileStorage{Path: opts.SessionFile}
	cli := telegram.NewClient(opts.APIID, opts.APIHash, telegram.Options{
		SessionStorage: storage,
	})

	var resolved *ChannelResolution
	err := cli.Run(parent, func(ctx context.Context) error {
		flow := auth.NewFlow(authenticator, auth.SendCodeOptions{})
		if err := cli.Auth().IfNecessary(ctx, flow); err != nil {
			return fmt.Errorf("auth: %w", err)
		}
		if channelUsername == "" {
			return nil
		}
		username := strings.TrimPrefix(channelUsername, "@")
		username = strings.TrimPrefix(username, "https://t.me/")
		api := cli.API()
		res, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
		if err != nil {
			return fmt.Errorf("resolve %q: %w", channelUsername, err)
		}
		for _, c := range res.Chats {
			ch, ok := c.(*tg.Channel)
			if !ok {
				continue
			}
			resolved = &ChannelResolution{
				ChannelID:  ch.ID,
				AccessHash: ch.AccessHash,
				Title:      ch.Title,
			}
			return nil
		}
		return fmt.Errorf("resolve %q: no channel in response", channelUsername)
	})
	if err != nil {
		return nil, err
	}
	return resolved, nil
}

// terminalAuthenticator drives the gotd auth flow from the supplied prompt
// function so the same logic works in CLI use and in tests with canned input.
type terminalAuthenticator struct {
	prompt func(label string) (string, error)
}

func (t terminalAuthenticator) Phone(ctx context.Context) (string, error) {
	return t.prompt("phone number (E.164, e.g. +14155550100)")
}

func (t terminalAuthenticator) Password(ctx context.Context) (string, error) {
	return t.prompt("2FA password (leave blank if none)")
}

func (t terminalAuthenticator) Code(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
	return t.prompt("verification code from Telegram")
}

func (t terminalAuthenticator) AcceptTermsOfService(ctx context.Context, tos tg.HelpTermsOfService) error {
	return nil
}

func (t terminalAuthenticator) SignUp(ctx context.Context) (auth.UserInfo, error) {
	return auth.UserInfo{}, errors.New("mtproto: account does not exist; sign up via the Telegram app first")
}
