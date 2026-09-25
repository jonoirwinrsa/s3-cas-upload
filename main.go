package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"time"
)

var codeRE = regexp.MustCompile(`<Code>([^<]+)</Code>`)

// putSigned is what a credential-less client does: PUT the bytes to the URL it
// was given, with the checksum header it was told to send.
func putSigned(url string, checksum string, body []byte) (int, string) {
	req, err := http.NewRequest("PUT", url, bytes.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}
	if checksum != "" {
		req.Header.Set("X-Amz-Checksum-Sha256", checksum)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if m := codeRE.FindSubmatch(b); m != nil {
		return resp.StatusCode, string(m[1])
	}
	return resp.StatusCode, ""
}

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
	code, ec := putSigned(url, checksum, body)
	report("correct bytes", code, ec)

	code, ec = putSigned(url, checksum, []byte("different bytes entirely\n"))
	report("wrong bytes, signed checksum kept", code, ec)

	code, ec = putSigned(url, "", body)
	report("checksum header omitted, correct bytes", code, ec)

	evil := []byte("bytes that are not the digest\n")
	code, ec = putSigned(url, "", evil)
	report("checksum header omitted, WRONG bytes", code, ec)

	other := sha256.Sum256([]byte("x"))
	code, ec = putSigned(url, hex.EncodeToString(other[:]), body)
	report("checksum header replaced", code, ec)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: s3-cas-upload serve|probe [flags]")
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
	flag.Parse()

	c := creds{*key, *secret, *region}
	store := newS3(*endpoint, *bucket, *key, *secret, *region, 0)
	if err := store.makeBucket(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	switch cmd {
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
