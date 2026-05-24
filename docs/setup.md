---
layout: page
title: Setup & Usage
eyebrow: Documentation
lede: From a fresh box to a running daemon, with copy-pasteable client configs for aws-cli, rclone, Cyberduck, aws-sdk-go-v2, and boto3.
permalink: /setup/
---

## Decide on a storage mode

| | bot mode | mtproto mode |
|---|---|---|
| Per-object limit | 20 MB | 2 GB |
| Setup ceremony | paste a bot token | log in with phone number + SMS code |
| Telegram account | not used | a real user account (yours) |
| Risk profile | bot can be revoked | account can be banned |
| Right for | avatars, thumbnails, small docs, app state, JSON | video, large datasets, backups |

### Pick **bot** if you want

- "I just need cheap blob storage for an app where every file is under
  20 MB."
- Zero phone-number paperwork.
- Lowest-risk setup (a bot is replaceable; a user account isn't).

### Pick **mtproto** if you want

- "I need to back up a directory of multi-hundred-MB video files."
- Object sizes between 20 MB and 2 GB.
- You accept that Telegram can ban the account with no recourse, and
  you're OK keeping a backup of `keys.toml` plus a `telang
  export-metadata` dump so you can re-fetch the data on a fresh account
  if it ever happens.

Switching modes after data is written requires migration.

## Install the binary

```bash
go install github.com/{{ site.github.repository_nwo | default: 'chud-lori/telang' }}/cmd/telang@latest
```

The binary is self-contained (no shared libs, no runtime). Place it
wherever you run daemons — `/usr/local/bin/telang` is fine.

A Docker image is also available; see the
[Dockerfile](https://github.com/{{ site.github.repository_nwo | default: 'chud-lori/telang' }}/blob/main/Dockerfile)
in the repo.

## Get Telegram credentials

### Bot mode

1. In the Telegram app, message `@BotFather` and run `/newbot`. Follow
   the prompts; copy the bot **token** (looks like `1234:abcdef...`).
2. Create a **private channel** in the Telegram app. Open it →
   *Manage channel* → *Administrators* → *Add admin* → search for your
   bot's username → grant *Post messages*.
3. Forward any message from the channel to `@RawDataBot` (or any
   chat-info bot) and copy the **channel ID** — a negative number
   beginning with `-100`.

### MTProto mode

1. Visit [my.telegram.org](https://my.telegram.org) → log in with your
   phone → *API development tools* → register an app → copy `api_id`
   and `api_hash`.
2. Create a **private channel** in the Telegram app. Give it a public
   `@username` (you can later strip the username back off if you want;
   Telang resolves it once during init).
3. Have your phone nearby — Telegram will text or in-app message a
   one-time code during `telang init`.

## Run `telang init`

```bash
telang init \
  --config   ./config.toml \
  --keys     ./keys.toml \
  --data-dir ./var
```

The flow:

1. **Mode** — type `bot` or `mtproto`.
2. **Credentials**
   - Bot: paste the BotFather token and the channel ID.
   - MTProto: paste `api_id` and `api_hash`, then go through the live
     phone-number / code / optional 2FA prompt. The channel `@username`
     gets resolved into a channel id + access hash, both stored in
     `config.toml`.
3. **Server** — listen address (default `:9000`).

`init` prints two strings you must save **right then**, because Telang
does not store them anywhere except in `config.toml`:

```
access_key = AKIA...
secret_key = ...
```

After this you have:

| File | Mode | Permissions | What |
|---|---|---|---|
| `config.toml` | both | 0600 | server + Telegram + storage paths |
| `keys.toml`   | both | 0600 | per-bucket AES keys (empty at first) |
| `<data-dir>/session` | mtproto | 0600 | gotd/td session blob |

Back up `keys.toml` somewhere off this machine. **Losing it = losing
the data**, full stop. Back up `session` only if you want to skip
`telang reauth` on a rebuild.

## Run the daemon

```bash
telang serve --config ./config.toml
```

Telang will listen on the configured port and emit one structured log
line per request. SIGINT or SIGTERM drains in-flight requests for up to
30 seconds before exiting.

## Point a client at it

### AWS CLI

```bash
aws configure --profile telang
# (paste the access_key / secret_key from init)
# default region:  tg-1
# output format:   json

aws --endpoint-url http://localhost:9000 --profile telang s3 mb s3://photos
aws --endpoint-url http://localhost:9000 --profile telang s3 cp ./holiday.jpg s3://photos/
aws --endpoint-url http://localhost:9000 --profile telang s3 ls s3://photos/
```

### rclone

`~/.config/rclone/rclone.conf`:

```ini
[telang]
type           = s3
provider       = Other
endpoint       = http://localhost:9000
access_key_id  = AKIA...
secret_access_key = ...
region         = tg-1
force_path_style = true
```

```bash
rclone mkdir telang:photos
rclone copy ./holiday.jpg telang:photos/
rclone ls   telang:photos
rclone mount telang:photos /mnt/telang-photos    # acts like a local folder
rclone serve webdav telang:photos --addr :8080   # mount on hosts without FUSE
```

### Cyberduck

*File* → *Open Connection*:

| Field | Value |
|---|---|
| Protocol | Amazon S3 |
| Server | `localhost` |
| Port | `9000` |
| Access Key ID | `AKIA...` |
| Secret Access Key | `...` |
| *More Options* → Path Style | ☑ |
| *More Options* → Region | `tg-1` |

### aws-sdk-go-v2 (Go)

Full lifecycle — create a bucket, upload, head, download, range read,
list, delete. Drop this in a `main.go`, run `go mod init` /
`go get github.com/aws/aws-sdk-go-v2/{config,credentials,service/s3}`,
and `go run .`:

```go
package main

import (
    "bytes"
    "context"
    "crypto/sha256"
    "fmt"
    "io"

    "github.com/aws/aws-sdk-go-v2/aws"
    "github.com/aws/aws-sdk-go-v2/config"
    "github.com/aws/aws-sdk-go-v2/credentials"
    "github.com/aws/aws-sdk-go-v2/service/s3"
)

func main() {
    ctx := context.Background()

    cfg, err := config.LoadDefaultConfig(ctx,
        config.WithRegion("tg-1"),
        config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
            "AKIA...", "...", "", // from `telang init`
        )),
        config.WithBaseEndpoint("http://localhost:9000"),
    )
    if err != nil {
        panic(err)
    }
    c := s3.NewFromConfig(cfg, func(o *s3.Options) { o.UsePathStyle = true })

    bucket, key := aws.String("photos"), aws.String("holiday.jpg")
    payload := []byte("...your bytes here...")

    // 1. Create the bucket.
    if _, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: bucket}); err != nil {
        panic(err)
    }

    // 2. Upload an object.
    if _, err := c.PutObject(ctx, &s3.PutObjectInput{
        Bucket: bucket, Key: key,
        Body:        bytes.NewReader(payload),
        ContentType: aws.String("image/jpeg"),
    }); err != nil {
        panic(err)
    }

    // 3. HEAD it.
    h, _ := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: key})
    fmt.Printf("size=%d etag=%s\n", *h.ContentLength, *h.ETag)

    // 4. GET it back and verify.
    out, _ := c.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: key})
    got, _ := io.ReadAll(out.Body)
    out.Body.Close()
    fmt.Printf("sha256 match=%v\n",
        sha256.Sum256(got) == sha256.Sum256(payload))

    // 5. Range GET (first 1 KiB).
    partial, _ := c.GetObject(ctx, &s3.GetObjectInput{
        Bucket: bucket, Key: key,
        Range: aws.String("bytes=0-1023"),
    })
    head1k, _ := io.ReadAll(partial.Body)
    partial.Body.Close()
    fmt.Printf("range bytes=%d\n", len(head1k))

    // 6. List.
    list, _ := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bucket})
    for _, o := range list.Contents {
        fmt.Printf("  %s\t%d\t%s\n", *o.Key, *o.Size, *o.ETag)
    }

    // 7. Generate a presigned GET (good for 10 min, no AWS creds needed
    //    to open the URL).
    pre := s3.NewPresignClient(c)
    req, _ := pre.PresignGetObject(ctx, &s3.GetObjectInput{
        Bucket: bucket, Key: key,
    }, s3.WithPresignExpires(10*60_000_000_000))
    fmt.Println("presigned:", req.URL)

    // 8. Clean up.
    _, _ = c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: key})
    _, _ = c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: bucket})
}
```

### boto3 (Python)

```python
import hashlib
import boto3
from botocore.config import Config

s3 = boto3.client(
    "s3",
    endpoint_url="http://localhost:9000",
    aws_access_key_id="AKIA...",      # from `telang init`
    aws_secret_access_key="...",       # from `telang init`
    region_name="tg-1",
    config=Config(
        signature_version="s3v4",
        s3={"addressing_style": "path"},
    ),
)

bucket, key = "photos", "holiday.jpg"
payload = b"...your bytes here..."

# 1. Create the bucket.
s3.create_bucket(Bucket=bucket)

# 2. Upload an object.
s3.put_object(Bucket=bucket, Key=key, Body=payload, ContentType="image/jpeg")

# 3. HEAD it.
head = s3.head_object(Bucket=bucket, Key=key)
print("size=", head["ContentLength"], "etag=", head["ETag"])

# 4. GET it back and verify.
got = s3.get_object(Bucket=bucket, Key=key)["Body"].read()
print("sha256 match=", hashlib.sha256(got).digest() == hashlib.sha256(payload).digest())

# 5. Range GET (first 1 KiB).
partial = s3.get_object(Bucket=bucket, Key=key, Range="bytes=0-1023")
print("range bytes=", len(partial["Body"].read()))

# 6. List, with pagination handled by the paginator.
for page in s3.get_paginator("list_objects_v2").paginate(Bucket=bucket):
    for o in page.get("Contents", []):
        print(" ", o["Key"], o["Size"], o["ETag"])

# 7. Presigned GET — anyone with the URL can fetch for the next 10 minutes.
url = s3.generate_presigned_url(
    "get_object",
    Params={"Bucket": bucket, "Key": key},
    ExpiresIn=600,
)
print("presigned:", url)

# 8. Clean up.
s3.delete_object(Bucket=bucket, Key=key)
s3.delete_bucket(Bucket=bucket)
```

## Browser UI

Telang ships a minimal server-rendered admin panel — no JS build step,
no SPA. Visit `http://localhost:9000/<bucket>/` in a browser:

- **Listing**: every object with size, content-type, last modified.
- **Download**: click any row.
- **Upload + delete**: gated behind a password set in `config.toml`:

  ```toml
  [browser_ui]
  enabled  = true
  password = "your-admin-password"
  ```

  When `password` is empty, the UI is read-only (no login, no writes).
  When set, browser writes require logging in at `/_browse/_login`.

## Operating the daemon

### Refresh an expired MTProto session

```bash
telang reauth --config ./config.toml
```

The old session is moved aside before the new one is written, so a
half-finished re-auth can't strand you.

### Detect orphaned objects

```bash
telang fsck --config ./config.toml          # report only
telang fsck --config ./config.toml --fix    # also delete metadata rows
```

Walks every object in the metadata DB and asks Telegram whether the
underlying message still exists. Anything missing is reported (or
removed with `--fix`).

### Disaster-recovery dumps

```bash
telang export-metadata --config ./config.toml > meta.jsonl
telang import-metadata --config ./config.toml < meta.jsonl
```

`import-metadata` refuses to overwrite a populated DB, so a misfired
restore can't corrupt an in-use deployment. Combine with a backup of
`keys.toml` to recover from a lost SQLite file.

### Presigned URLs

```go
pre := s3.NewPresignClient(c)
req, _ := pre.PresignGetObject(ctx, &s3.GetObjectInput{
    Bucket: aws.String("photos"),
    Key:    aws.String("big.jpg"),
}, s3.WithPresignExpires(10*time.Minute))
fmt.Println(req.URL)
```

The URL is openable by any HTTP client with no AWS credentials. Telang
enforces the 7-day signing-window cap and a 15-minute clock-skew window.

### Where logs live

Telang logs to stdout in `key=value` text. Pipe it into journald,
syslog, or a file as you would any other daemon.

### Throughput expectations

- Telegram rate-limits aggressive callers. Telang respects
  `FLOOD_WAIT` and exponentially backs off on 5xx — a sudden spike is
  silently slowed, not failed.
- Cold reads (cache miss) are bounded by Telegram download throughput.
- Warm reads (cache hit) are bounded by your local disk.
- Concurrent multipart uploads are bounded by the staging directory's
  free space.

For the daemon's intrinsic overhead numbers (encryption, sigv4, cache)
see [BENCHMARKS.md](https://github.com/{{ site.github.repository_nwo | default: 'chud-lori/telang' }}/blob/main/BENCHMARKS.md).

### Backups

| Thing | How |
|---|---|
| `keys.toml` | copy off-host the moment you create a new bucket |
| `config.toml` | back up after `init` (mostly so you don't lose `secret_key`) |
| `session` (mtproto) | optional; `telang reauth` rebuilds it |
| `telang.db` | back up periodically; needed to recover after disk loss |

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `keys: ... must be chmod 600` on start | someone touched the file's perms | `chmod 600 keys.toml` |
| `mtproto: session is not authorized — run telang reauth` | session blob is missing or expired | `telang reauth --config ...` |
| `403 InvalidAccessKeyId` from a client | client's AK doesn't match config | recheck the access_key in the client config |
| `416 InvalidRange` on a range GET | range past end of object | client bug; check object size with HEAD |
| `EntityTooLarge` on PutObject in bot mode | object > 20 MB | switch to mtproto mode |
| `503 SlowDown` | Telegram rate-limited us | client should retry; Telang already honours `retry_after` |
