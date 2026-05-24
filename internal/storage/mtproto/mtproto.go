// Package mtproto is Telang's MTProto storage backend.
//
// Unlike bot mode, MTProto talks to Telegram with a user-account session.
// Limits: 2 GB per object up and down. The session file is persisted with
// chmod 600 via gotd/td's session.FileStorage.
//
// Lifecycle is non-trivial: telegram.Client.Run is a long-running RPC
// callback. We launch it in a goroutine and block daemon operations until
// the connection reports ready; on Close we cancel the run context and
// wait for the goroutine to settle.
package mtproto

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"

	"github.com/telang/telang/internal/storage"
)

// MaxObjectSize: 2 GB in MTProto mode. Telegram's hard limit is actually
// 2000 MB; we round to 2 GB.
const MaxObjectSize int64 = 2000 * 1024 * 1024

type Options struct {
	APIID            int
	APIHash          string
	SessionFile      string
	ChannelID        int64 // canonical Telegram channel id (positive form)
	ChannelAccessHash int64
}

// Backend implements storage.Backend over a long-running MTProto session.
type Backend struct {
	opts      Options
	client    *telegram.Client
	api       *tg.Client
	up        *uploader.Uploader
	dl        *downloader.Downloader
	channel   *tg.InputChannel
	peer      *tg.InputPeerChannel

	cancel  context.CancelFunc
	runDone chan error
}

// Open authenticates against the existing session and starts the long-running
// client loop. If the session is missing or expired the caller should run
// `telang reauth` and then call Open again.
func Open(parent context.Context, opts Options) (*Backend, error) {
	if opts.APIID == 0 || opts.APIHash == "" {
		return nil, errors.New("mtproto: api_id and api_hash are required")
	}
	if opts.SessionFile == "" {
		return nil, errors.New("mtproto: session_file is required")
	}
	if opts.ChannelID == 0 {
		return nil, errors.New("mtproto: channel_id is required")
	}

	storage := &session.FileStorage{Path: opts.SessionFile}
	cli := telegram.NewClient(opts.APIID, opts.APIHash, telegram.Options{
		SessionStorage: storage,
		RetryInterval:  time.Second,
		MaxRetries:     5,
	})

	runCtx, cancel := context.WithCancel(parent)
	b := &Backend{
		opts:    opts,
		client:  cli,
		cancel:  cancel,
		runDone: make(chan error, 1),
	}

	ready := make(chan error, 1)
	go func() {
		b.runDone <- cli.Run(runCtx, func(ctx context.Context) error {
			status, err := cli.Auth().Status(ctx)
			if err != nil {
				ready <- fmt.Errorf("mtproto: auth status: %w", err)
				return err
			}
			if !status.Authorized {
				err := errors.New("mtproto: session is not authorized — run `telang reauth`")
				ready <- err
				return err
			}
			b.api = cli.API()
			b.up = uploader.NewUploader(b.api)
			b.dl = downloader.NewDownloader()
			b.channel = &tg.InputChannel{
				ChannelID:  opts.ChannelID,
				AccessHash: opts.ChannelAccessHash,
			}
			b.peer = &tg.InputPeerChannel{
				ChannelID:  opts.ChannelID,
				AccessHash: opts.ChannelAccessHash,
			}
			ready <- nil
			<-ctx.Done()
			return ctx.Err()
		})
		close(ready)
	}()

	select {
	case err := <-ready:
		if err != nil {
			cancel()
			<-b.runDone
			return nil, err
		}
	case <-parent.Done():
		cancel()
		<-b.runDone
		return nil, parent.Err()
	}
	return b, nil
}

// Close cancels the client and waits for the goroutine to exit.
func (b *Backend) Close() error {
	b.cancel()
	err := <-b.runDone
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (b *Backend) MaxObjectSize() int64 { return MaxObjectSize }

func (b *Backend) Put(ctx context.Context, name string, size int64, r io.Reader) (storage.PutResult, error) {
	if size > MaxObjectSize {
		return storage.PutResult{}, storage.ErrTooLarge
	}
	if name == "" {
		name = "blob"
	}

	inputFile, err := b.up.Upload(ctx, uploader.NewUpload(name, r, size))
	if err != nil {
		return storage.PutResult{}, fmt.Errorf("mtproto: upload: %w", err)
	}

	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return storage.PutResult{}, err
	}
	randomID := int64(binary.BigEndian.Uint64(random[:]))

	updates, err := b.api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Peer: b.peer,
		Media: &tg.InputMediaUploadedDocument{
			File:     inputFile,
			MimeType: "application/octet-stream",
			Attributes: []tg.DocumentAttributeClass{
				&tg.DocumentAttributeFilename{FileName: name},
			},
		},
		Message:  "",
		RandomID: randomID,
	})
	if err != nil {
		return storage.PutResult{}, fmt.Errorf("mtproto: send media: %w", err)
	}

	msgID, docID, ok := extractDocumentMessage(updates)
	if !ok {
		return storage.PutResult{}, errors.New("mtproto: response did not contain a document message")
	}
	return storage.PutResult{
		Ref:  storage.Ref{MessageID: int64(msgID), FileID: fmt.Sprintf("%d", docID)},
		Size: size,
	}, nil
}

func (b *Backend) Get(ctx context.Context, ref storage.Ref) (io.ReadCloser, error) {
	doc, err := b.resolveDocument(ctx, int(ref.MessageID))
	if err != nil {
		return nil, err
	}
	loc := &tg.InputDocumentFileLocation{
		ID:            doc.ID,
		AccessHash:    doc.AccessHash,
		FileReference: doc.FileReference,
	}
	pr, pw := io.Pipe()
	go func() {
		_, err := b.dl.Download(b.api, loc).Stream(ctx, pw)
		pw.CloseWithError(err)
	}()
	return pr, nil
}

// Exists reports whether the Telegram message still has a document. A
// missing or non-document message both register as false; transport errors
// are surfaced to the caller.
func (b *Backend) Exists(ctx context.Context, ref storage.Ref) (bool, error) {
	_, err := b.resolveDocument(ctx, int(ref.MessageID))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (b *Backend) Delete(ctx context.Context, ref storage.Ref) error {
	_, err := b.api.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{
		Channel: b.channel,
		ID:      []int{int(ref.MessageID)},
	})
	if err != nil {
		// "MESSAGE_ID_INVALID" or similar means already gone.
		if strings.Contains(err.Error(), "MESSAGE_ID_INVALID") {
			return nil
		}
		return err
	}
	return nil
}

// resolveDocument fetches a single message from the channel and returns the
// embedded *tg.Document, refreshing file_reference each time so expirations
// don't break GETs on long-lived objects.
func (b *Backend) resolveDocument(ctx context.Context, msgID int) (*tg.Document, error) {
	res, err := b.api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: b.channel,
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}},
	})
	if err != nil {
		return nil, fmt.Errorf("mtproto: get message: %w", err)
	}
	var msgs []tg.MessageClass
	switch v := res.(type) {
	case *tg.MessagesMessages:
		msgs = v.Messages
	case *tg.MessagesMessagesSlice:
		msgs = v.Messages
	case *tg.MessagesChannelMessages:
		msgs = v.Messages
	default:
		return nil, fmt.Errorf("mtproto: unexpected messages.getMessages response %T", res)
	}
	for _, m := range msgs {
		msg, ok := m.(*tg.Message)
		if !ok {
			continue
		}
		media, ok := msg.Media.(*tg.MessageMediaDocument)
		if !ok {
			continue
		}
		doc, ok := media.Document.(*tg.Document)
		if !ok {
			continue
		}
		return doc, nil
	}
	return nil, storage.ErrNotFound
}

// extractDocumentMessage pulls (message_id, document_id) out of an Updates
// response. The Telegram API can return UpdateNewChannelMessage or
// UpdateMessageID; we accept both.
func extractDocumentMessage(updates tg.UpdatesClass) (msgID int, docID int64, ok bool) {
	var list []tg.UpdateClass
	switch u := updates.(type) {
	case *tg.Updates:
		list = u.Updates
	case *tg.UpdatesCombined:
		list = u.Updates
	case *tg.UpdateShort:
		list = []tg.UpdateClass{u.Update}
	default:
		return 0, 0, false
	}
	for _, up := range list {
		newMsg, isNew := up.(*tg.UpdateNewChannelMessage)
		if !isNew {
			continue
		}
		msg, ok := newMsg.Message.(*tg.Message)
		if !ok {
			continue
		}
		media, ok := msg.Media.(*tg.MessageMediaDocument)
		if !ok {
			continue
		}
		doc, ok := media.Document.(*tg.Document)
		if !ok {
			continue
		}
		return msg.ID, doc.ID, true
	}
	return 0, 0, false
}
