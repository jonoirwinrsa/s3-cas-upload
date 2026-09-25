package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"time"
)

var codeRE = regexp.MustCompile(`<Code>([^<]+)</Code>`)

func probe(endpoint, bucket string, c creds) {
	body := []byte("presigned upload probe\n")
	h := sha256.Sum256(body)
	digest := hex.EncodeToString(h[:])
	key := "blobs/sha256/" + digest

	url, checksum, err := c.presign(endpoint, bucket, key, digest, 10*time.Minute, time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	report := func(name string, code int, errCode string) {
		fmt.Printf("%-44s %d %s\n", name, code, errCode)
	}
	code, ec, _ := putSigned(http.DefaultClient, url, checksum, body)
	report("correct bytes", code, ec)

	code, ec, _ = putSigned(http.DefaultClient, url, checksum, []byte("different bytes entirely\n"))
	report("wrong bytes, signed checksum kept", code, ec)

	code, ec, _ = putSigned(http.DefaultClient, url, "", body)
	report("checksum header omitted, correct bytes", code, ec)

	evil := []byte("bytes that are not the digest\n")
	code, ec, _ = putSigned(http.DefaultClient, url, "", evil)
	report("checksum header omitted, WRONG bytes", code, ec)

	other := sha256.Sum256([]byte("x"))
	code, ec, _ = putSigned(http.DefaultClient, url, hex.EncodeToString(other[:]), body)
	report("checksum header replaced", code, ec)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: cas serve|upload|tree|bench|crossover|verify|probe|reset [flags]")
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	os.Args = os.Args[1:]

	endpoint := flag.String("endpoint", "http://localhost:8333", "S3 endpoint")
	bucket := flag.String("bucket", "cas", "bucket")
	key := flag.String("key", "test", "access key")
	secret := flag.String("secret", "test", "secret key")
	region := flag.String("region", "us-east-1", "region")
	addr := flag.String("addr", ":8080", "signing server address")
	strategy := flag.String("strategy", "head", "existence check: head or list")
	root := flag.String("root", "tree", "directory to upload")
	signer := flag.String("server", "http://localhost:8080", "signing server")
	chunk := flag.Int64("chunk", 8<<20, "split files above this size")
	cachePath := flag.String("cache", ".cas-cache.json", "hash cache file")
	nocache := flag.Bool("nocache", false, "ignore the hash cache")
	manifestID := flag.String("manifest", "", "manifest digest to verify")
	files := flag.Int("files", 5000, "files in the generated tree")
	mb := flag.Int64("mb", 500, "approximate tree size in MB")
	dup := flag.Float64("dup", 0.1, "share of files that duplicate an earlier one")
	seed := flag.Int64("seed", 1, "tree seed")
	flag.Parse()

	c := creds{*key, *secret, *region}
	if cmd == "tree" {
		if err := genTree(*root, *files, *mb<<20, *dup, *seed); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if cmd == "upload" {
		cl := &client{*signer, *chunk, loadCache(*cachePath, !*nocache), http.DefaultClient}
		id, st, err := cl.Upload(*root)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("manifest   %s\n", id)
		fmt.Printf("files      %d (%d hashed, %d from cache)\n", st.Files, st.Hashed, cl.cache.Hits)
		fmt.Printf("chunks     %d (%d missing)\n", st.Chunks, st.Missing)
		fmt.Printf("read       %d bytes\n", st.BytesRead)
		fmt.Printf("uploaded   %d bytes in %d PUTs\n", st.BytesUp, st.Puts)
		fmt.Printf("checks     %d\n", st.Checks)
		fmt.Printf("wall       %s\n", st.Wall.Round(time.Millisecond))
		return
	}

	store := newS3(*endpoint, *bucket, *key, *secret, *region, 0)
	if err := store.makeBucket(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	switch cmd {
	case "reset":
		n, err := store.empty(blobPrefix)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("deleted %d blobs\n", n)
	case "verify":
		if err := verify(store, *manifestID); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "bench":
		if err := benchmark(store, *signer, *root, *cachePath, *chunk, *seed); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "crossover":
		if err := crossover(store, *signer, []int{10, 50, 100, 500, 1000, 2000, 5000}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "serve":
		sv := &server{store, c, *endpoint, *bucket, *strategy, 15 * time.Minute}
		fmt.Println("signing server on", *addr, "strategy", *strategy)
		if err := sv.serve(*addr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "probe":
		probe(*endpoint, *bucket, c)
	default:
		usage()
	}
}
