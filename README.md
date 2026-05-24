# Telang

📖 **Docs site:** [Home](https://your-org.github.io/telang/) · [Setup &
Usage](https://your-org.github.io/telang/setup/) — built from
[`docs/`](docs/) via GitHub Pages.

**Free, effectively-unlimited object storage you self-host, using a
private Telegram channel as the actual disk.** Run a single binary on
your own box with your own Telegram credentials; the bytes live as
encrypted messages inside Telegram. Telang speaks the standard S3 wire
protocol on the front, so anything that already talks to S3 (`aws s3`,
`rclone`, `boto3`, Cyberduck, aws-sdk-go-v2, …) Just Works against
`http://localhost:9000` — no Telang-specific client needed.

Good for hobby projects, internal tools, and dev / staging. **Not for
production, customer data, or high-throughput public asset serving.**
Telegram can ban the account; your data can vanish. That is the deal.

## Status

| Phase | Status |
|---|---|
| v0.1 — walking skeleton (bot mode, PUT/GET/HEAD/DELETE) | ✅ |
| v0.2 — feature complete (encryption, cache, multipart, ListObjectsV2, range, `telang init`) | ✅ |
| v0.3 — MTProto adapter (`telang reauth`, up to 2 GB per object) | ✅ |
| v0.4 — browser UI, `telang fsck`, metadata export/import, Docker image | ⏳ |
| v1.0 — presigned-URL docs, compat matrix, benchmarks | ⏳ |

## What it does

- Speaks the AWS S3 wire protocol, including Signature V4, unsigned and
  streaming payloads, single-range GET, ListObjectsV2 with prefix +
  delimiter + pagination, and multipart upload.
- Encrypts every object with **AES-256-GCM** in 64 KB frames before
  uploading; Telegram only ever sees ciphertext.
- Uses a per-bucket key stored in a `keys.toml` file with chmod 600.
  Losing the file means losing the data — back it up out of band.
- Two storage modes: **bot** (Telegram Bot API, 20 MB per object,
  no phone number needed) and **mtproto** (Telegram user-account
  session, 2 GB per object).
- Disk LRU cache so warm reads don't round-trip to Telegram.

## What it does not do

- Versioning, ACLs, CORS, lifecycle, replication, object lock, tagging.
- SSE-C / SSE-KMS server-side encryption variants.
- Multi-tenant credentials. One static access-key / secret-key pair per
  install. Put a reverse proxy in front if you need more.
- Act as a CDN. Telegram is not a CDN.

## Quick install

```bash
go install github.com/telang/telang/cmd/telang@latest
telang init --config ./config.toml --keys ./keys.toml --data-dir ./var
telang serve --config ./config.toml
```

See [setup.md](setup.md) for the long version: choosing a mode,
registering credentials, and configuring `aws-cli` / `rclone` /
Cyberduck against the daemon.

## Using it

Once `telang serve` is running, any S3 client works. Example with the
official AWS CLI:

```bash
export AWS_ACCESS_KEY_ID=AKIA...    # printed by telang init
export AWS_SECRET_ACCESS_KEY=...     # printed by telang init
export AWS_DEFAULT_REGION=tg-1

aws --endpoint-url http://localhost:9000 s3 mb s3://photos
aws --endpoint-url http://localhost:9000 s3 cp ./holiday.jpg s3://photos/
aws --endpoint-url http://localhost:9000 s3 ls s3://photos/
```

`rclone` with a `[telang]` remote (see setup.md for the full snippet):

```bash
rclone copy ./holiday.jpg telang:photos/
rclone mount telang:photos /mnt/telang-photos
```

Any aws-sdk-go-v2 / boto3 / aws-sdk-js-v3 program targeting Telang's
endpoint also works without any Telang-specific client.

## Commands

| Command | Purpose |
|---|---|
| `telang init` | interactive setup (mode, credentials, channel, S3 keys) |
| `telang reauth` | re-login when an MTProto session expires |
| `telang serve --config PATH` | run the daemon |

## Files Telang owns

| File | What |
|---|---|
| `config.toml` | server, S3, Telegram mode, paths |
| `keys.toml` | per-bucket AES keys — back this up |
| `telang.db` | SQLite index of buckets, objects, multipart state |
| `cache/` | LRU ciphertext cache |
| `staging/` | tmp dirs for in-flight multipart uploads |
| `session` (mtproto mode only) | gotd/td session blob |

## Risks worth re-reading

1. **Account ban.** Telegram tolerates this kind of usage at small scale
   (rclone's Telegram backend, TgFileStream, etc. all operate this way)
   but does not promise to. If they ban the account, every object becomes
   unreadable until you `telang export-metadata`, log into a fresh
   account, re-fetch the files, and import. That is the existential risk
   and it is why this is not for production.
2. **`keys.toml` loss.** No keys, no plaintext. Telang refuses to start
   if the file isn't chmod 600. Back it up.
3. **Throughput.** Telegram rate-limits the kinds of operations Telang
   makes. Use this for hobby workloads.

## License

MIT. See `LICENSE` (rationale: maximum compatibility for embed / fork
use cases; matches `rclone`).
