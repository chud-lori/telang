package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/telang/telang/internal/config"
	"github.com/telang/telang/internal/metadata"
	"github.com/telang/telang/internal/storage"
)

// runFsck walks every object in the metadata DB and asks the backend whether
// the underlying Telegram message still exists. Rows for missing messages are
// reported as orphans. Nothing is mutated unless --fix is passed.
func runFsck(args []string, stdin io.Reader, stdout io.Writer) error {
	_ = stdin // fsck does not read user input
	fs := flag.NewFlagSet("fsck", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/telang/config.toml", "path to config.toml")
	fix := fs.Bool("fix", false, "delete metadata rows for orphaned messages")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	meta, err := metadata.Open(ctx, cfg.Storage.DBPath)
	if err != nil {
		return err
	}
	defer meta.Close()
	backend, err := buildBackend(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeBackend(backend)

	var (
		checked   int
		orphans   int
		removed   int
		queryErrs int
	)
	type orphan struct {
		bucket, key string
		messageID   int64
	}
	var orphanList []orphan

	err = meta.ForEachObject(ctx, func(o *metadata.Object) error {
		checked++
		ref := storage.Ref{MessageID: o.MessageID, FileID: o.FileID}
		exists, err := backend.Exists(ctx, ref)
		if err != nil {
			queryErrs++
			fmt.Fprintf(os.Stderr, "  ! %s/%s message_id=%d: %v\n", o.Bucket, o.Key, o.MessageID, err)
			return nil
		}
		if !exists {
			orphans++
			orphanList = append(orphanList, orphan{o.Bucket, o.Key, o.MessageID})
			fmt.Fprintf(stdout, "  ✗ orphan: %s/%s message_id=%d\n", o.Bucket, o.Key, o.MessageID)
		}
		return nil
	})
	if err != nil {
		return err
	}

	if *fix && len(orphanList) > 0 {
		for _, o := range orphanList {
			if _, err := meta.DeleteObject(ctx, o.bucket, o.key); err != nil {
				fmt.Fprintf(os.Stderr, "  ! could not remove orphan %s/%s: %v\n", o.bucket, o.key, err)
				continue
			}
			removed++
		}
	}

	fmt.Fprintln(stdout, "")
	fmt.Fprintf(stdout, "checked=%d orphans=%d query_errors=%d", checked, orphans, queryErrs)
	if *fix {
		fmt.Fprintf(stdout, " removed=%d", removed)
	}
	fmt.Fprintln(stdout)
	if queryErrs > 0 {
		return fmt.Errorf("fsck: %d backend errors", queryErrs)
	}
	return nil
}
