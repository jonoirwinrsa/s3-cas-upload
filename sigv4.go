package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const algorithm = "AWS4-HMAC-SHA256"

type creds struct {
	key, secret, region string
}

func mac(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// escapePath encodes a key for the canonical request. S3 wants every byte
// outside the unreserved set percent-encoded, with the separators left alone.
func escapePath(p string) string {
	var b strings.Builder
	for i := range len(p) {
		c := p[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~', c == '/':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func (c creds) signingKey(day string) []byte {
	k := mac([]byte("AWS4"+c.secret), []byte(day))
	for _, p := range []string{c.region, "s3", "aws4_request"} {
		k = mac(k, []byte(p))
	}
	return k
}

// sign adds the SigV4 Authorization header. payloadHash is the hex SHA-256 of
// the body, which S3 requires in x-amz-content-sha256 and which this tool has
// already computed in order to content-address the file.
func (c creds) sign(req *http.Request, payloadHash string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	req.Header.Set("Host", req.URL.Host)

	names := make([]string, 0, len(req.Header))
	for k := range req.Header {
		names = append(names, strings.ToLower(k))
	}
	sort.Strings(names)

	var canonHeaders strings.Builder
	for _, n := range names {
		fmt.Fprintf(&canonHeaders, "%s:%s\n", n, strings.TrimSpace(req.Header.Get(n)))
	}
	signedHeaders := strings.Join(names, ";")

	canonReq := strings.Join([]string{
		req.Method,
		escapePath(req.URL.Path),
		req.URL.RawQuery,
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{day, c.region, "s3", "aws4_request"}, "/")
	sum := sha256.Sum256([]byte(canonReq))
	toSign := strings.Join([]string{algorithm, amzDate, scope, hex.EncodeToString(sum[:])}, "\n")

	sig := hex.EncodeToString(mac(c.signingKey(day), []byte(toSign)))

	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm, c.key, scope, signedHeaders, sig))
}

const signedHeaders = "host;x-amz-checksum-sha256"

// presign returns a PUT URL that accepts only a body whose SHA-256 is digest,
// plus the checksum header the client has to send. Both the key and that header
// are covered by the signature, so the client can change neither.
func (c creds) presign(endpoint, bucket, key, digest string, expires time.Duration, now time.Time) (string, string, error) {
	raw, err := hex.DecodeString(digest)
	if err != nil {
		return "", "", err
	}
	checksum := base64.StdEncoding.EncodeToString(raw)

	u, err := url.Parse(endpoint + "/" + bucket + "/" + key)
	if err != nil {
		return "", "", err
	}

	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	scope := strings.Join([]string{day, c.region, "s3", "aws4_request"}, "/")

	q := url.Values{
		"X-Amz-Algorithm":     {algorithm},
		"X-Amz-Credential":    {c.key + "/" + scope},
		"X-Amz-Date":          {amzDate},
		"X-Amz-Expires":       {strconv.Itoa(int(expires.Seconds()))},
		"X-Amz-SignedHeaders": {signedHeaders},
	}

	canonReq := strings.Join([]string{
		"PUT",
		escapePath(u.Path),
		q.Encode(),
		"host:" + u.Host + "\nx-amz-checksum-sha256:" + checksum + "\n",
		signedHeaders,
		"UNSIGNED-PAYLOAD",
	}, "\n")

	sum := sha256.Sum256([]byte(canonReq))
	toSign := strings.Join([]string{algorithm, amzDate, scope, hex.EncodeToString(sum[:])}, "\n")
	q.Set("X-Amz-Signature", hex.EncodeToString(mac(c.signingKey(day), []byte(toSign))))

	return u.String() + "?" + q.Encode(), checksum, nil
}
