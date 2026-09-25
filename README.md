# s3-cas-upload

Upload a source tree to S3 so that a repeat upload transfers only what changed, and so that a blob
stored under a digest contains the bytes that hash to it.

Runs against MinIO in a container. No AWS account needed.

## Run it

```
make up      # MinIO on :9000, signing server on :8080, bucket created
make tree    # generate a 500 MB test tree
make bench   # four uploads under different conditions
make crossover   # HeadObject vs ListObjectsV2 across tree sizes
make tamper  # send bytes that do not match the digest they were signed for
make down
```

MinIO's images left Docker Hub in September 2026 and the quay.io replacement needs a licence, so
`make up` pulls `cgr.dev/chainguard/minio`.

`FILES=5000 MB=500 make tree` sets the shape of the tree. `RTT=40ms make bench` adds a fixed delay
per request. `CHUNK=8388608` sets the size above which files are split.

## Method

The client walks the tree and hashes it, and holds no credentials. The signing server holds them,
answers the existence check, and returns one presigned PUT per missing digest.

`make bench` runs:

1. Generate the tree: a set file count, a heavy-tailed size distribution, some duplicates.
2. Empty the bucket and the hash cache.
3. Cold upload. Hash every file, ask which digests are missing, upload those, write the manifest.
4. Rewrite one file at the same length, upload again. The cache is in place, so the other
   files are not read.
5. Drop the cache, upload again. Everything is rehashed, and only the server's answer saves work.
6. Delete one file, upload again. Only the manifest changes.
7. Report bytes moved, requests made, files hashed and wall clock for each upload.

Steps 4 and 5 are reported separately so the cache is not credited with work the store did.

MinIO runs on loopback, so wall clock is reported after bytes and requests. `RTT` stands in for a
network.

The cache key is path, size, mtime and inode. A file rewritten to the same length within one mtime
tick is missed. Step 4 asserts that an ordinary edit is detected.

`make crossover` compares the two ways the server can answer an existence check: N parallel
`HeadObject` calls, or one paginated `ListObjectsV2`. The first scales with files uploaded, the
second with objects already in the bucket.

## Security

The server signs the key and `x-amz-checksum-sha256` into each presigned PUT, both set to the same
digest. The client can alter neither, and S3 rejects a body whose SHA-256 differs, so a client can
only write bytes under their own digest. The server never reads content and never hashes.

A client that holds credentials and sets the checksum itself gets no such guarantee. S3 checks the
header against the body and nothing more. It does not know the key is a digest, and no bucket
policy condition key covers checksums, so a client choosing both can store any bytes under any
digest.

The guarantee depends on the store enforcing signed headers, which not every S3 implementation
does. `make tamper` checks four cases against whatever endpoint it is given:

| Case | MinIO | SeaweedFS |
|---|---|---|
| correct bytes | accepted | accepted |
| wrong bytes, signed checksum sent | rejected | rejected |
| checksum header omitted | rejected | **accepted** |
| checksum header replaced | rejected | rejected |

SeaweedFS accepts a presigned request that leaves out a signed header, so a client can drop the
checksum and store any bytes under any digest. AWS requires signed headers to be present, and
MinIO matches it, which is why MinIO is the default target.

SHA-256 is used because it is the only cryptographic hash S3 verifies. CRC32, CRC32C and
CRC64NVME detect corruption but not deliberate substitution, SHA-1 is broken, and BLAKE3 is faster
but unsupported.

Manifest paths are validated on read as well as on write, since `../` in a stored name escapes the
restore directory. Existence checks reveal whether a blob was already uploaded by someone else;
scoping them per tenant removes that and removes cross-tenant dedup.

## Large files

A single PUT is capped at 5 GiB, so files above `CHUNK` are split into fixed-size chunks. Each
chunk is a blob at `blobs/sha256/<chunk-digest>`. The manifest records a file's chunk digests in
order plus its own SHA-256. A small file is a one-chunk file.

Multipart is not used. S3 stores a checksum of checksums for multipart objects, so the
whole-object SHA-256 is not verified and the object's verified identity is not the file's digest.

Chunking dedups at chunk granularity, which `CHUNK` trades against request count. An insertion
near the start of a large file shifts every later boundary and re-uploads the whole file.
Content-defined chunking would avoid that and is not implemented.

## Results

MinIO and the signing server on one laptop, 5000 files totalling 513 MB of which 505 are
duplicates, 8 MiB chunks.

| | cold | changed, cached | changed, rehashed | deleted |
|---|---|---|---|---|
| bytes uploaded | 474,371,615 | 1,044,399 | 0 | 1,024,766 |
| PUT requests | 4,496 | 2 | 0 | 1 |
| existence checks | 4,496 | 4,496 | 4,496 | 4,495 |
| files hashed | 5,000 | 1 | 5,000 | 0 |
| bytes read | 526,658,960 | 19,427 | 526,658,960 | 0 |
| wall clock | 9.09s | 643ms | 1.288s | 1.001s |

The cold upload moved 474 MB of the 527 MB it read. The 53 MB difference is the duplicate files,
which hash to digests another file already covered.

Changing one file moved 1,044,399 bytes in two PUTs, and only 19,427 of those are the file. The
rest is the manifest, which lists all 5000 entries and is rewritten whole whenever anything
changes. At this tree size the manifest is the floor on a warm upload and costs fifty times the
edit that triggered it. Deleting a file shows the same floor with nothing else in it: one PUT,
1,024,766 bytes, no files hashed, and no space reclaimed.

The two warm runs differ by four orders of magnitude in bytes read, 19 KB against 527 MB, and
upload the same amount. The cache buys reads and nothing else.

### Existence check

| digests asked | head | list |
|---|---|---|
| 10 | 3ms | 132ms |
| 50 | 7ms | 133ms |
| 100 | 13ms | 131ms |
| 500 | 50ms | 131ms |
| 1000 | 104ms | 143ms |
| 2000 | 192ms | 130ms |

Against a bucket holding 4499 blobs. `list` is flat in the number of digests asked, since it pages
the whole prefix either way, and `head` is flat in the size of the bucket. They cross between 1000
and 2000 digests, at roughly a third of the objects stored.
