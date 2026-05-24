---
title: Telang
permalink: /
---

{% include nav.html %}

S3-compatible object storage that keeps the bytes in a private Telegram
channel. Run it on your own box with your own Telegram credentials. Point
any S3 client (`aws s3`, `rclone`, `boto3`, Cyberduck, the aws-sdk-go-v2,
…) at `http://localhost:9000` and you get a free, effectively-unlimited
object store that is fine for hobby projects, internal tools, and dev /
staging — and **not** for production, customer data, or high-throughput
public asset serving. Telegram can ban the account; your data can vanish.
That is the deal.

## Status

| Phase | Status |
|---|---|
| v0.1 — walking skeleton (bot mode, PUT/GET/HEAD/DELETE) | ✅ |
| v0.2 — feature complete (encryption, cache, multipart, ListObjectsV2, range, `telang init`) | ✅ |
| v0.3 — MTProto adapter (`telang reauth`, up to 2 GB per object) | ✅ |
| v0.4 — browser UI, `telang fsck`, metadata export/import, Docker image | ✅ |
| v1.0 — presigned URLs, compat matrix, benchmarks | ✅ |

## What it does

- Speaks the AWS S3 wire protocol: Signature V4 (header **and** presigned
  query-string), unsigned and streaming payloads, single-range GET,
  ListObjectsV2 with prefix + delimiter + pagination, multipart upload.
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

See **[Setup & Usage]({{ '/setup/' | relative_url }})** for the long
version: choosing a mode, registering credentials, and configuring
`aws-cli` / `rclone` / Cyberduck against the daemon.

## Using it

Once `telang serve` is running, any S3 client works:

```bash
export AWS_ACCESS_KEY_ID=AKIA...    # printed by telang init
export AWS_SECRET_ACCESS_KEY=...     # printed by telang init
export AWS_DEFAULT_REGION=tg-1

aws --endpoint-url http://localhost:9000 s3 mb s3://photos
aws --endpoint-url http://localhost:9000 s3 cp ./holiday.jpg s3://photos/
aws --endpoint-url http://localhost:9000 s3 ls s3://photos/
```

`rclone` with a `[telang]` remote (full snippet on the
[setup page]({{ '/setup/' | relative_url }})):

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
| `telang fsck [--fix]` | walk objects, report (or remove) orphans |
| `telang export-metadata > meta.jsonl` | JSONL dump for disaster recovery |
| `telang import-metadata < meta.jsonl` | restore from dump |

## Risks worth re-reading

1. **Account ban.** Telegram tolerates this usage at small scale
   (rclone's Telegram backend, TgFileStream, etc. all operate this way)
   but does not promise to. If they ban the account, every object becomes
   unreadable until you `telang export-metadata`, log into a fresh
   account, re-fetch the files, and import.
2. **`keys.toml` loss.** No keys, no plaintext. Telang refuses to start
   if the file isn't chmod 600. Back it up out of band.
3. **Throughput.** Telegram rate-limits the kinds of operations Telang
   makes. Use this for hobby workloads.

## License

MIT. See [`LICENSE`](https://github.com/{{ site.github.repository_nwo | default: 'your-org/telang' }}/blob/main/LICENSE) (rationale:
maximum compatibility for embed / fork use cases; matches `rclone`).
