package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

type s3 struct {
	endpoint string
	bucket   string
	c        creds
	http     *http.Client
	rtt      time.Duration

	puts, heads, lists atomic.Int64
	bytesUp            atomic.Int64
}

func newS3(endpoint, bucket, key, secret, region string, rtt time.Duration) *s3 {
	return &s3{
		endpoint: strings.TrimSuffix(endpoint, "/"),
		bucket:   bucket,
		c:        creds{key, secret, region},
		http:     &http.Client{Timeout: 60 * time.Second},
		rtt:      rtt,
	}
}

func (s *s3) do(method, key string, q url.Values, body []byte, payloadHash string,
	hdr map[string]string) (*http.Response, []byte, error) {

	u := s.endpoint + "/" + s.bucket
	if key != "" {
		u += "/" + key
	}
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequest(method, u, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.ContentLength = int64(len(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	s.c.sign(req, payloadHash, time.Now())

	if s.rtt > 0 {
		time.Sleep(s.rtt)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp, b, err
}

const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func (s *s3) makeBucket() error {
	resp, b, err := s.do("PUT", "", nil, nil, emptyHash, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 && !bytes.Contains(b, []byte("BucketAlreadyOwnedByYou")) {
		return fmt.Errorf("create bucket: %s: %s", resp.Status, b)
	}
	return nil
}

// put stores one blob. digest is the hex SHA-256 of body, and it goes out twice:
// as the SigV4 payload hash, and base64-encoded as x-amz-checksum-sha256 so the
// server recomputes it and refuses bytes that do not match the key.
func (s *s3) put(key, digest string, body []byte) error {
	return s.putAs(key, digest, digest, body)
}

// putAs separates the two digests a PUT carries so they can be varied
// independently: payloadDigest is what SigV4 signs over, checksumDigest is what
// the server is being asked to verify against the body.
func (s *s3) putAs(key, payloadDigest, checksumDigest string, body []byte) error {
	raw, err := hex.DecodeString(checksumDigest)
	if err != nil {
		return err
	}
	resp, b, err := s.do("PUT", key, nil, body, payloadDigest, map[string]string{
		"X-Amz-Checksum-Sha256": base64.StdEncoding.EncodeToString(raw),
		"Content-Type":          "application/octet-stream",
	})
	s.puts.Add(1)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("put %s: %s: %s", key, resp.Status, strings.TrimSpace(string(b)))
	}
	s.bytesUp.Add(int64(len(body)))
	return nil
}

func (s *s3) head(key string) (bool, error) {
	resp, _, err := s.do("HEAD", key, nil, nil, emptyHash, nil)
	s.heads.Add(1)
	if err != nil {
		return false, err
	}
	switch {
	case resp.StatusCode == 404:
		return false, nil
	case resp.StatusCode >= 400:
		return false, fmt.Errorf("head %s: %s", key, resp.Status)
	}
	return true, nil
}

type listResult struct {
	Contents []struct {
		Key string `xml:"Key"`
	} `xml:"Contents"`
	NextContinuationToken string `xml:"NextContinuationToken"`
	IsTruncated           bool   `xml:"IsTruncated"`
}

// listPrefix walks every key under prefix. It is the other half of the
// existence-check tradeoff: this costs one request per 1000 objects already in
// the bucket, where head costs one per file being uploaded.
func (s *s3) listPrefix(prefix string) (map[string]bool, error) {
	have := map[string]bool{}
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}, "max-keys": {"1000"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, b, err := s.do("GET", "", q, nil, emptyHash, nil)
		s.lists.Add(1)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("list: %s: %s", resp.Status, b)
		}
		var r listResult
		if err := xml.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		for _, c := range r.Contents {
			have[c.Key] = true
		}
		if !r.IsTruncated || r.NextContinuationToken == "" {
			return have, nil
		}
		token = r.NextContinuationToken
	}
}
