# Benchmarks

These numbers measure **only the daemon's overhead** (sigv4 + AES-GCM frame
encryption + LRU cache + HTTP plumbing) against an in-process fake Telegram.
They do not predict throughput against the real Telegram API. They exist so
regressions in the encryption / signing / cache hot paths get caught early.

## Running the benchmarks

```bash
go test ./internal/s3api -run XXX -bench '.' -benchtime 3s
```

The `-run XXX` filter skips functional tests; `-benchtime 3s` makes the
numbers more stable than the default 1 s.

## Reference run

Hardware: Apple M1, 8 cores, Go 1.25.5, darwin/arm64. Object size: 1 MiB.

| Benchmark | ns/op | MB/s |
|---|---|---|
| `BenchmarkPutObject` | 4.30 ms | ~244 MB/s |
| `BenchmarkGetObjectWarm` (cache hit) | 0.84 ms | ~1.24 GB/s |
| `BenchmarkRangeGet1KB` (single frame) | 0.17 ms | ~6 MB/s |

### How to read these

- **`BenchmarkPutObject`** is bottlenecked by AES-GCM encryption + the HTTP
  multipart envelope to the fake server. Real Telegram bot mode is
  bottlenecked by `sendDocument` round-trips; expect roughly Telegram's own
  upload rate (usually well below this number).
- **`BenchmarkGetObjectWarm`** is the cache hit path: read the on-disk
  ciphertext, AES-GCM decrypt frame-by-frame, write to the response. Sub-ms
  per MiB.
- **`BenchmarkRangeGet1KB`** is dominated by setup, not data transfer:
  open the cache file, parse the 17-byte header, decrypt one 64 KiB frame,
  slice 1 KiB out. The MB/s figure is misleading (a single frame is the
  smallest unit of work) — read `ns/op` instead.

## What's NOT measured

- Cold cache (fetch-from-Telegram) — that path's latency belongs to
  Telegram, not to Telang.
- Concurrent multipart uploads — added in a future revision.
- p95/p99 latency under concurrent load — same.

## Notes on the harness

`internal/s3api/integration_test.go::setupServer` builds an in-memory
fake Telegram via `httptest.Server` so each iteration round-trips through
the full request pipeline: sigv4 verification, encryption, cache, fake
HTTP upload/download. The numbers reflect the daemon's *own* steady-state
overhead.
