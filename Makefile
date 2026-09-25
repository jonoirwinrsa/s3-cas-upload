ENDPOINT ?= http://localhost:9000
KEY      ?= minioadmin
SECRET   ?= minioadmin
SIGNER   ?= http://localhost:8080
ROOT     ?= tree
FILES    ?= 5000
MB       ?= 500
CHUNK    ?= 8388608
STRATEGY ?= head
S3       := -endpoint $(ENDPOINT) -key $(KEY) -secret $(SECRET)

.PHONY: up tree bench crossover tamper verify down clean

out/cas: $(wildcard *.go)
	go build -o out/cas .

# MinIO left Docker Hub in September 2026; the quay.io build needs a licence.
up: out/cas
	docker inspect cas-minio >/dev/null 2>&1 || docker run -d --name cas-minio -p 9000:9000 \
	  -e MINIO_ROOT_USER=$(KEY) -e MINIO_ROOT_PASSWORD=$(SECRET) \
	  cgr.dev/chainguard/minio server /tmp/data >/dev/null
	sleep 3
	pkill -f 'cas serve' 2>/dev/null || true
	./out/cas serve $(S3) -strategy $(STRATEGY) > out/signer.log 2>&1 & sleep 1
	echo "minio $(ENDPOINT), signer $(SIGNER)"

tree: out/cas
	./out/cas tree -root $(ROOT) -files $(FILES) -mb $(MB)

bench: out/cas
	./out/cas bench $(S3) -root $(ROOT) -server $(SIGNER) -chunk $(CHUNK)

crossover: out/cas
	./out/cas crossover $(S3) -server $(SIGNER)

tamper: out/cas
	./out/cas probe $(S3)

verify: out/cas
	./out/cas verify $(S3) -manifest $(MANIFEST)

down:
	pkill -f 'cas serve' 2>/dev/null || true
	docker rm -f cas-minio 2>/dev/null || true

clean:
	rm -rf out $(ROOT) .cas-cache.json
