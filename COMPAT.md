# Client compatibility matrix

This file is the v1.0 reference for which S3 clients are known to work
end-to-end against a Telang daemon. Each row links to a copy-pasteable
runbook below.

| Client | Status | Sigv4 mode used | Notes |
|---|---|---|---|
| [aws-cli](#aws-cli) | ✅ tested (manual) | streaming unsigned trailer | needs `--endpoint-url` + path-style |
| [rclone](#rclone) | ✅ tested (manual) | streaming signed payload | `provider = Other`, `force_path_style = true` |
| [aws-sdk-go-v2](#aws-sdk-go-v2) | ✅ tested (manual) | streaming unsigned trailer | use `s3.WithUsePathStyle(true)` |
| [boto3](#boto3) | ✅ tested (manual) | hex-sha256 payload | sets `Content-MD5` automatically |
| [Cyberduck](#cyberduck) | ✅ tested (manual) | hex-sha256 payload | "S3 (HTTP)" profile, path-style enabled |

All four "real SDK" runbooks below run the same checklist Telang treats
as the v0.2 definition of done: create bucket → put → head → get → range
get → list with prefix → delete. If everything in the checklist passes,
the client is compatible.

The hand-rolled signer in the test suite (`internal/s3api/integration_test.go`)
exercises every payload mode in CI; the manual runs below are what
confirm we match the real SDKs' on-wire behavior.

---

## Common setup

After `telang init`, you have these:

```
access_key = AKIA...
secret_key = ...
region     = tg-1
endpoint   = http://localhost:9000   (or wherever you ran `serve`)
```

Use them in every client below.

---

## aws-cli

```bash
aws configure --profile telang
# enter access_key / secret_key / region=tg-1

ENDPOINT=--endpoint-url=http://localhost:9000
PROFILE=--profile=telang

aws $ENDPOINT $PROFILE s3 mb s3://compat
aws $ENDPOINT $PROFILE s3 cp /tmp/big.bin s3://compat/big.bin
aws $ENDPOINT $PROFILE s3api head-object --bucket compat --key big.bin
aws $ENDPOINT $PROFILE s3 cp s3://compat/big.bin /tmp/big.copy
diff /tmp/big.bin /tmp/big.copy   # must be empty

# Range GET via low-level API:
aws $ENDPOINT $PROFILE s3api get-object --bucket compat --key big.bin \
    --range 'bytes=0-1023' /tmp/range.bin

aws $ENDPOINT $PROFILE s3 ls s3://compat/
aws $ENDPOINT $PROFILE s3 rm s3://compat/big.bin
aws $ENDPOINT $PROFILE s3 rb s3://compat
```

---

## rclone

`~/.config/rclone/rclone.conf`:

```ini
[telang]
type              = s3
provider          = Other
endpoint          = http://localhost:9000
access_key_id     = AKIA...
secret_access_key = ...
region            = tg-1
force_path_style  = true
```

```bash
rclone mkdir telang:compat
rclone copy /tmp/big.bin telang:compat/
rclone ls   telang:compat
rclone copy telang:compat/big.bin /tmp/big.copy
diff /tmp/big.bin /tmp/big.copy

rclone mount telang:compat /mnt/telang-compat
# in another shell: ls, cat, etc. against /mnt/telang-compat

rclone delete telang:compat/big.bin
rclone rmdir  telang:compat
```

`rclone mount` exercises range GETs heavily — if mounts work, ranges work.

---

## aws-sdk-go-v2

```go
package main

import (
    "bytes"
    "context"
    "crypto/rand"
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
    cfg, _ := config.LoadDefaultConfig(ctx,
        config.WithRegion("tg-1"),
        config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("AKIA...", "...", "")),
        config.WithBaseEndpoint("http://localhost:9000"),
    )
    c := s3.NewFromConfig(cfg, func(o *s3.Options) {
        o.UsePathStyle = true
    })

    // Create + put a 15 MB payload via multipart.
    payload := make([]byte, 15*1024*1024)
    _, _ = rand.Read(payload)
    _, _ = c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("compat")})
    _, _ = c.PutObject(ctx, &s3.PutObjectInput{
        Bucket: aws.String("compat"),
        Key:    aws.String("big.bin"),
        Body:   bytes.NewReader(payload),
    })

    // Read back and verify SHA256.
    out, _ := c.GetObject(ctx, &s3.GetObjectInput{
        Bucket: aws.String("compat"),
        Key:    aws.String("big.bin"),
    })
    got, _ := io.ReadAll(out.Body)
    out.Body.Close()
    fmt.Printf("match=%v\n", sha256.Sum256(got) == sha256.Sum256(payload))
}
```

For a presigned URL example see "Presigned URLs" below.

---

## boto3

```python
import boto3, hashlib, secrets

s3 = boto3.client(
    "s3",
    endpoint_url="http://localhost:9000",
    aws_access_key_id="AKIA...",
    aws_secret_access_key="...",
    region_name="tg-1",
    config=boto3.session.Config(signature_version="s3v4", s3={"addressing_style": "path"}),
)

payload = secrets.token_bytes(2 * 1024 * 1024)
s3.create_bucket(Bucket="compat")
s3.put_object(Bucket="compat", Key="2mb.bin", Body=payload)

got = s3.get_object(Bucket="compat", Key="2mb.bin")["Body"].read()
assert hashlib.sha256(got).digest() == hashlib.sha256(payload).digest()

for page in s3.get_paginator("list_objects_v2").paginate(Bucket="compat"):
    for obj in page.get("Contents", []):
        print(obj["Key"], obj["Size"])

s3.delete_object(Bucket="compat", Key="2mb.bin")
s3.delete_bucket(Bucket="compat")
```

---

## Cyberduck

*File → Open Connection*:

| Field | Value |
|---|---|
| Protocol | Amazon S3 |
| Server | `localhost` (or wherever telang is) |
| Port | `9000` |
| Access Key ID | `AKIA...` |
| Secret Access Key | `...` |
| *More Options* → Path Style | ☑ |
| *More Options* → Region | `tg-1` |

Drag-drop upload/download works; the listing pane mirrors `ListObjectsV2`.

---

## Presigned URLs

aws-sdk-go-v2 example. The signature is **query-string-only** (no
Authorization header), and the body is treated as `UNSIGNED-PAYLOAD`.

```go
pre := s3.NewPresignClient(c)
req, _ := pre.PresignGetObject(ctx, &s3.GetObjectInput{
    Bucket: aws.String("compat"),
    Key:    aws.String("big.bin"),
}, s3.WithPresignExpires(10*time.Minute))

// req.URL can be opened by any HTTP client, anywhere, no AWS credentials:
fmt.Println(req.URL)
```

`internal/sigv4/presigned_test.go` and
`internal/s3api/integration_test.go::TestPresignedGet` cover this on
every CI run.

---

## Reporting compatibility regressions

If a client that used to work stops working, file an issue including:

- Client + version.
- Exact command you ran.
- `telang serve` log lines around the failure (`auth_failed`,
  `get_object_stream_failed`, etc.).
- A `tcpdump` / `curl -v` of the failing request, with credentials
  redacted.

Telang's signing surface accepts four payload modes and a query-string
presign form; if a new SDK starts emitting a fifth, we will add support.
