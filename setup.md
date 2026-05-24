# Telang setup guide

Detailed install + configure walkthrough. The short version lives in
[README.md](README.md).

---

## 1. Decide on a storage mode

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

Switching modes after data is written requires migration (out of scope
in v1).

---

## 2. Install the binary

```bash
go install github.com/telang/telang/cmd/telang@latest
```

The binary is self-contained (no shared libs, no runtime). Place it
wherever you run daemons — `/usr/local/bin/telang` is fine.

---

## 3. Get Telegram credentials

### Bot mode

1. In the Telegram app, message `@BotFather` and run `/newbot`. Follow
   the prompts; copy the bot **token** (looks like `1234:abcdef...`).
2. Create a **private channel** in the Telegram app. Open it →
   *Manage channel* → *Administrators* → *Add admin* → search for your
   bot's username → grant *Post messages* (the default permissions are
   fine; nothing else needs to be on).
3. Forward any message from the channel to `@RawDataBot` (or any
   chat-info bot) and copy the **channel ID** — a negative number
   beginning with `-100`.

### MTProto mode

1. Visit https://my.telegram.org → log in with your phone → *API
   development tools* → register an app → copy `api_id` and `api_hash`.
2. Create a **private channel** in the Telegram app. Give it a public
   `@username` (you can later strip the username back off if you want;
   Telang resolves it once during init).
3. Have your phone nearby — Telegram will text or in-app message a
   one-time code during `telang init`.

---

## 4. Run `telang init`

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

---

## 5. Run the daemon

```bash
telang serve --config ./config.toml
```

Telang will listen on the configured port and emit one structured log
line per request. SIGINT or SIGTERM drains in-flight requests for up to
30 seconds before exiting.

---

## 6. Point a client at it

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

```go
cfg, _ := config.LoadDefaultConfig(ctx,
    config.WithRegion("tg-1"),
    config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("AKIA...", "...", "")),
    config.WithBaseEndpoint("http://localhost:9000"),
)
c := s3.NewFromConfig(cfg, func(o *s3.Options) {
    o.UsePathStyle = true
})
```

### boto3 (Python)

```python
import boto3
s3 = boto3.client(
    "s3",
    endpoint_url="http://localhost:9000",
    aws_access_key_id="AKIA...",
    aws_secret_access_key="...",
    region_name="tg-1",
)
```

---

## 7. Operating the daemon

### Refresh an expired MTProto session

```bash
telang reauth --config ./config.toml
```

This refreshes `<data-dir>/session` in place. The old session is moved
aside until the new one is written successfully, so a half-finished
re-auth can't strand you.

### Where logs live

Telang logs to stdout in `key=value` text. Pipe it into journald,
syslog, or a file as you would any other daemon.

### Throughput expectations

- Telegram rate-limits aggressive callers. Telang respects
  `FLOOD_WAIT` and exponentially backs off on 5xx — that means a sudden
  spike is silently slowed, not failed.
- Cold reads (cache miss) are bounded by Telegram download throughput.
- Warm reads (cache hit) are bounded by your local disk.
- Concurrent multipart uploads are bounded by the staging directory's
  free space — each in-flight upload reserves up to the object size.

### Backups

| Thing | How |
|---|---|
| `keys.toml` | copy off-host the moment you create a new bucket |
| `config.toml` | back up after `init` (mostly so you don't lose `secret_key`) |
| `session` (mtproto) | optional; `telang reauth` rebuilds it |
| `telang.db` | back up periodically; needed to recover after disk loss |

---

## 8. Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `keys: ... must be chmod 600` on start | someone touched the file's perms | `chmod 600 keys.toml` |
| `mtproto: session is not authorized — run telang reauth` | session blob is missing or expired | `telang reauth --config ...` |
| `403 InvalidAccessKeyId` from a client | client's AK doesn't match config | recheck the access_key in the client config |
| `416 InvalidRange` on a range GET | range past end of object | client bug; check object size with HEAD |
| `EntityTooLarge` on PutObject in bot mode | object > 20 MB | switch to mtproto mode |
| `503 SlowDown` | Telegram rate-limited us | client should retry; we already honor `retry_after` |
