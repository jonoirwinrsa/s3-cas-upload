# s3-cas-upload

Upload a source tree to S3 so that a repeat upload transfers only what changed, and so that a blob
stored under a digest contains the bytes that hash to it.

Runs against SeaweedFS in a container. No AWS account needed.

## Run it

```
make up      # SeaweedFS S3 on :8333, signing server on :8080, bucket created
make tree    # generate a 500 MB test tree
make bench   # four uploads under different conditions
make crossover   # HeadObject vs ListObjectsV2 across tree sizes
make tamper  # send bytes that do not match the digest they were signed for
make down
```

`FILES=5000 MB=500 make tree` sets the shape of the tree. `RTT=40ms make bench` adds a fixed delay
per request. `CHUNK=8388608` sets the size above which files are split.

## Method

The client walks the tree and hashes it, and holds no credentials. The signing server holds them,
answers the existence check, and returns one presigned PUT per missing digest.

`make bench` runs:

1. Generate the tree: a set file count, a mixed size distribution, some duplicates.
2. Empty the bucket and the hash cache.
3. Cold upload. Hash every file, ask which digests are missing, upload those, write the manifest.
4. Touch one file, upload again. The cache is in place, so unchanged files are not read.
5. Drop the cache, upload again. Everything is rehashed, and only the server's answer saves work.
6. Delete one file, upload again. Only the manifest changes.
7. Report bytes moved, requests made, files hashed and wall clock for each upload.

Steps 4 and 5 are reported separately so the cache is not credited with work the store did.

SeaweedFS runs on loopback, so wall clock is reported after bytes and requests. `RTT` stands in
for a network.

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

Not measured yet.
