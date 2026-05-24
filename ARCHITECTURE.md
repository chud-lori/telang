# Architecture

This document explains *why* Telang is shaped the way it is. The README
covers what it does; this file covers what we considered and rejected so
future changes can be made on top of the same logic instead of guessing.

```
┌──────────────────┐  S3 HTTP (sigv4)   ┌────────────────────────────┐
│  S3 clients      │ ──────────────────▶│  telang daemon (Go)        │
│  aws-sdk, rclone │                    │  ┌──────────────────────┐  │
└──────────────────┘                    │  │ HTTP / S3 verbs      │  │   ┌────────┐
                                        │  │ sigv4, multipart     │  │   │ Tele-  │
                                        │  ├──────────────────────┤  │   │ gram   │
                                        │  │ AES-GCM enc / dec    │  │   │ private│
                                        │  ├──────────────────────┤  │   │ chan-  │
                                        │  │ storage adapter      │◀─┼──▶│ nel    │
                                        │  │  - bot mode (HTTP)   │  │   │        │
                                        │  │  - mtproto mode      │  │   └────────┘
                                        │  │  - disk LRU cache    │  │
                                        │  ├──────────────────────┤  │
                                        │  │ metadata: SQLite     │  │
                                        │  └──────────────────────┘  │
                                        └────────────────────────────┘
```

## Storage layout

### One private channel = one install. Not one channel per bucket.

A Telang install puts every object into a single Telegram channel. The
S3 bucket name is metadata-only grouping; it does **not** map to a
separate Telegram channel.

The alternative — one channel per bucket — was considered and rejected.
It adds operational overhead (each `CreateBucket` would need to create
a channel and grant the bot/user admin), buys nothing on the addressing
side (`(channel, message_id)` is already unique), and would make the
mode-switching story worse. The S3 bucket abstraction is already cheap
to enforce in SQLite.

### One object = one Telegram message.

For whole-object PUTs, the upload becomes one `sendDocument` /
`messages.SendMedia` call. For multipart uploads, parts are staged
locally and the final assembly is uploaded as **one** message on
`CompleteMultipartUpload`. From Telegram's view every S3 object is
always exactly one message — this keeps the addressing scheme uniform
and lets `fsck` work with a simple "does message_id still exist?" probe.

Per-object identifiers stored in SQLite:

- `message_id` — Telegram's stable per-channel message identifier;
  primary addressing.
- `file_id` — Telegram's session-scoped document identifier; cached
  for fast download. If the session rotates the `file_id` can be
  re-resolved from `message_id`, so we don't have to store anything
  more durable.

## Encryption

### AES-256-GCM in 64 KB frames, per-bucket key.

The plaintext object is split into 64 KB frames and each frame is its
own GCM seal. Per-frame nonces are derived from a random per-object
base nonce XOR'd with the frame index; the frame index is also passed
as Additional Authenticated Data, so reordering frames or splicing one
in from another object breaks decryption.

Why frames at all (instead of one big `Seal`):

- **Range reads.** Only the frames overlapping the requested byte
  range need to be fetched and decrypted. A 1 KB range GET on a 100 MB
  object decrypts one frame, not the whole thing.
- **Streaming uploads.** Each frame is sealed as bytes flow through;
  we never need the whole plaintext in memory or on disk.
- **Tamper detection.** GCM tags are per-frame, so corruption is
  caught at the smallest practical granularity.

Why 64 KB specifically: small enough that a single-frame range read is
fast, large enough that the 16-byte GCM tag overhead per frame is
under 0.03%.

Why per-bucket (not per-object, not per-install) keys: per-install
makes a single key-compromise total; per-object makes the metadata
table huge and operationally fragile (export/import has to carry
keys). Per-bucket is the natural unit users already understand and
matches how rotation, backup, and disaster recovery naturally
operate.

### Ciphertext-side cache, not plaintext-side.

The disk LRU cache stores **ciphertext** blobs keyed by message_id.
Decryption happens after the cache lookup.

Plaintext caching was rejected for two reasons. First, it would mean
the cache directory contains decrypted bytes — a more dangerous
on-disk footprint than encrypted bytes. Second, it forces a hard
choice about what to do on a key rotation (invalidate everything?
re-encrypt?). Ciphertext-side caching keeps the encryption boundary
clean and lets the cache work without ever touching plaintext.

Decryption is cheap relative to a Telegram round-trip, so doing it on
every cache hit costs essentially nothing.

## Multipart upload

S3's multipart contract allows parts to be uploaded in any order,
re-uploaded under the same `partNumber` (overwriting), and combined
later. Mapping that directly onto Telegram's chunked upload protocol
would mean a lot of bookkeeping for half-finished objects.

Instead each `UploadPart` writes its bytes to a local staging file
(`{staging_dir}/{upload_id}/{partNumber}.part`, atomically via
tempfile+rename so a half-uploaded part is never observed).
`CompleteMultipartUpload` concatenates `*.part` files in `partNumber`
order, streams the result through the encryption layer, uploads the
ciphertext to Telegram as one document, then deletes the staging
directory.

The tradeoff: peak local disk usage equals the size of the largest
in-flight multipart object. With the 2 GB per-object ceiling that's
bounded; the operator just needs to size `staging_dir` accordingly.

In exchange, the multipart flow is atomic from the S3 client's view
(the object appears on the final POST and never partially) and the
Telegram side never has to know multipart exists.

## Auth surface

Sigv4 verification supports all four payload modes the major SDKs
emit:

- `UNSIGNED-PAYLOAD` — body hash not signed
- hex sha256 — body hash signed
- `STREAMING-AWS4-HMAC-SHA256-PAYLOAD` — chunked with per-chunk
  signatures verified against a rolling HMAC chain seeded by the
  Authorization header signature
- `STREAMING-UNSIGNED-PAYLOAD-TRAILER` — chunked, unsigned chunks,
  trailers stripped

Plus query-string presigned URLs (`X-Amz-*`). All comparisons are
constant-time.

Single static access-key / secret-key pair per install. Multi-tenant
credentials are deliberately out of scope — operators who need them
put a reverse proxy in front. The whole tool is single-tenant by
design.

## Implementation choices

### Go, with the standard library where possible.

- `net/http` for the server. No gin / echo / fiber.
- `database/sql` + `modernc.org/sqlite` (pure Go) for metadata. CGO
  off, single static binary.
- `crypto/aes` + `crypto/cipher` for encryption. No third-party crypto.
- `html/template` for the browser UI. No JS build step.
- `gotd/td` for MTProto. The only heavy dependency; required because
  reimplementing MTProto is not in scope.
- AWS SDK is **not** a runtime dependency; we implement sigv4
  verification ourselves so we can support all four payload modes.

### SQLite for metadata.

It's a single file, ships pure-Go, supports WAL for concurrent reads,
and `STRICT` mode catches schema mistakes. The dominant read pattern
is `ListObjectsV2` prefix scans, which is exactly what a B-tree on
`(bucket, key)` is good at.

## License

**MIT**, not AGPL.

AGPL was the natural alternative (it's what MinIO uses). It was
rejected because Telang is designed to be embedded — someone running
it next to their app, modifying it for their setup, exposing it over
the network for their own clients. AGPL would force every such
deployer to publish their modifications, which is hostile to that
use case. The closest analog tool, `rclone`, is MIT for the same
reason.

## Things deliberately not built

### Channel auto-provisioning during `telang init`.

The doc considered making `init` call `channels.CreateChannel`
automatically and grant the bot/user admin. Punted because there is
no bot-mode equivalent of the MTProto call — the asymmetry between
modes wasn't worth shipping. Operators create the channel manually
and paste its ID. Revisit if there is demand.

### A MinIO-Console-style admin SPA.

The browser UI is intentionally ~200 lines of Go plus two
`html/template` files. A full single-page admin app was scoped and
deferred. The "I can see what's in there without installing
anything" use case is covered by the minimal listing; anything
beyond that should be a real S3 client (rclone, Cyberduck, etc.).

### S3 features beyond core CRUD.

Versioning, ACLs, CORS, lifecycle, replication, object lock, tagging
— none of these are implemented. Each one would either need a
Telegram-side mechanism that doesn't exist (versioning, replication)
or would re-implement what an S3 reverse proxy does better (ACLs,
CORS). The handler returns `NotImplemented` with the correct S3 error
code so clients fall back gracefully.

### Production-grade reliability.

Single-tenant. No HA. No replication. Telegram can ban the account
and the bytes are gone. Sized for hobby and dev workloads only — see
the warnings in `README.md`.
