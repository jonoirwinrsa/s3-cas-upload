package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"syscall"
	"time"
)

type fileEntry struct {
	Path   string   `json:"path"`
	Size   int64    `json:"size"`
	Mode   uint32   `json:"mode"`
	Digest string   `json:"digest"`
	Chunks []string `json:"chunks"`
}

type manifest struct {
	Files []fileEntry `json:"files"`
}

// chunkRef lets the upload re-read bytes rather than hold the tree in memory.
type chunkRef struct {
	path string
	off  int64
	n    int64
}

type stats struct {
	Files, Hashed, Chunks, Missing, Puts, Checks int
	BytesRead, BytesUp                           int64
	Wall                                         time.Duration
}

type client struct {
	server    string
	chunkSize int64
	cache     *cache
	http      *http.Client
}

// hashFile reads the file once for both the whole-file digest and the chunks.
func hashFile(path string, chunkSize int64) (string, []string, []chunkRef, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil, nil, 0, err
	}
	defer f.Close()

	whole := sha256.New()
	buf := make([]byte, chunkSize)
	var (
		digests []string
		refs    []chunkRef
		off     int64
	)
	for {
		n, err := io.ReadFull(f, buf)
		if n > 0 {
			whole.Write(buf[:n])
			h := sha256.Sum256(buf[:n])
			digests = append(digests, hex.EncodeToString(h[:]))
			refs = append(refs, chunkRef{path, off, int64(n)})
			off += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return "", nil, nil, 0, err
		}
	}
	return hex.EncodeToString(whole.Sum(nil)), digests, refs, off, nil
}

func (c *client) scan(root string, st *stats) ([]fileEntry, map[string]chunkRef, error) {
	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Strings(paths)
	st.Files = len(paths)

	entries := make([]fileEntry, len(paths))
	refs := map[string]chunkRef{}
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		ferr error
	)
	sem := make(chan struct{}, runtime.NumCPU())
	for i, p := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			info, err := os.Stat(p)
			if err != nil {
				mu.Lock()
				ferr = err
				mu.Unlock()
				return
			}
			rel, _ := filepath.Rel(root, p)

			if e, ok := c.cache.get(p, info); ok {
				e.Path = rel
				mu.Lock()
				entries[i] = e
				addRefs(refs, p, e, c.chunkSize)
				mu.Unlock()
				return
			}

			digest, chunks, cr, read, err := hashFile(p, c.chunkSize)
			if err != nil {
				mu.Lock()
				ferr = err
				mu.Unlock()
				return
			}
			e := fileEntry{rel, info.Size(), uint32(info.Mode().Perm()), digest, chunks}

			mu.Lock()
			entries[i] = e
			st.Hashed++
			st.BytesRead += read
			for j, d := range chunks {
				if _, seen := refs[d]; !seen {
					refs[d] = cr[j]
				}
			}

			mu.Unlock()
			c.cache.put(p, info, e)
		}()
	}
	wg.Wait()
	return entries, refs, ferr
}

// addRefs records where a cached file's chunks live. A cached file is not
// reopened, but its digests still go to the server, so a blob deleted from the
// store is noticed instead of silently ending up in the manifest.
func addRefs(refs map[string]chunkRef, path string, e fileEntry, chunkSize int64) {
	var off int64
	for _, d := range e.Chunks {
		n := min(chunkSize, e.Size-off)
		if _, seen := refs[d]; !seen {
			refs[d] = chunkRef{path, off, n}
		}
		off += n
	}
}

func (c *client) askMissing(digests []string) (map[string]target, int, error) {
	body, err := json.Marshal(map[string]any{"digests": digests})
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.http.Post(c.server+"/missing", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, 0, fmt.Errorf("missing: %s: %s", resp.Status, b)
	}
	var out struct {
		Upload map[string]target `json:"upload"`
		Checks int               `json:"checks"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, 0, err
	}
	return out.Upload, out.Checks, nil
}

// putSigned is all the client does with S3, using the URL and checksum it was
// handed.
func putSigned(hc *http.Client, url, checksum string, body []byte) (int, string, error) {
	req, err := http.NewRequest("PUT", url, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.ContentLength = int64(len(body))
	if checksum != "" {
		req.Header.Set("X-Amz-Checksum-Sha256", checksum)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if m := codeRE.FindSubmatch(b); m != nil {
		return resp.StatusCode, string(m[1]), nil
	}
	return resp.StatusCode, "", nil
}

func (c *client) uploadBlobs(want map[string]target, refs map[string]chunkRef, st *stats) error {
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		ferr error
	)
	sem := make(chan struct{}, 8)
	for d, t := range want {
		ref, ok := refs[d]
		if !ok {
			return fmt.Errorf("no bytes for digest %s", d)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			buf := make([]byte, ref.n)
			f, err := os.Open(ref.path)
			if err == nil {
				_, err = f.ReadAt(buf, ref.off)
				f.Close()
			}
			if err == nil {
				var code int
				var ec string
				code, ec, err = putSigned(c.http, t.URL, t.Checksum, buf)
				if err == nil && code >= 400 {
					err = fmt.Errorf("put %s: %d %s", d, code, ec)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				ferr = err
				return
			}
			st.Puts++
			st.BytesUp += ref.n
		}()
	}
	wg.Wait()
	return ferr
}

// putOne covers the manifest, which the client cannot write directly either.
func (c *client) putOne(body []byte, st *stats) (string, error) {
	h := sha256.Sum256(body)
	d := hex.EncodeToString(h[:])
	want, checks, err := c.askMissing([]string{d})
	st.Checks += checks
	if err != nil {
		return "", err
	}
	if t, ok := want[d]; ok {
		code, ec, err := putSigned(c.http, t.URL, t.Checksum, body)
		if err != nil {
			return "", err
		}
		if code >= 400 {
			return "", fmt.Errorf("put manifest: %d %s", code, ec)
		}
		st.Puts++
		st.BytesUp += int64(len(body))
	}
	return d, nil
}

func (c *client) Upload(root string) (string, *stats, error) {
	st := &stats{}
	start := time.Now()

	entries, refs, err := c.scan(root, st)
	if err != nil {
		return "", st, err
	}
	st.Chunks = len(refs)

	digests := make([]string, 0, len(refs))
	for d := range refs {
		digests = append(digests, d)
	}
	sort.Strings(digests)

	want, checks, err := c.askMissing(digests)
	st.Checks += checks
	st.Missing = len(want)
	if err != nil {
		return "", st, err
	}
	if err := c.uploadBlobs(want, refs, st); err != nil {
		return "", st, err
	}

	body, err := json.Marshal(manifest{entries})
	if err != nil {
		return "", st, err
	}
	id, err := c.putOne(body, st)
	if err != nil {
		return "", st, err
	}
	if err := c.cache.save(); err != nil {
		return "", st, err
	}
	st.Wall = time.Since(start)
	return id, st, nil
}

func inode(info os.FileInfo) uint64 {
	if s, ok := info.Sys().(*syscall.Stat_t); ok {
		return s.Ino
	}
	return 0
}

// verify reassembles each file from its chunks and checks it against the digest
// the manifest recorded, which tests chunk order as well as content.
func verify(store *s3, id string) error {
	b, err := store.get(blobKey(id))
	if err != nil {
		return err
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	for _, e := range m.Files {
		h := sha256.New()
		var n int64
		for _, d := range e.Chunks {
			cb, err := store.get(blobKey(d))
			if err != nil {
				return fmt.Errorf("%s: %w", e.Path, err)
			}
			sum := sha256.Sum256(cb)
			if got := hex.EncodeToString(sum[:]); got != d {
				return fmt.Errorf("%s: chunk stored under %s hashes to %s", e.Path, d, got)
			}
			h.Write(cb)
			n += int64(len(cb))
		}
		got := hex.EncodeToString(h.Sum(nil))
		if got != e.Digest {
			return fmt.Errorf("%s: reassembled to %s, manifest says %s", e.Path, got, e.Digest)
		}
		if n != e.Size {
			return fmt.Errorf("%s: reassembled %d bytes, manifest says %d", e.Path, n, e.Size)
		}
		fmt.Printf("ok  %-28s %2d chunk(s)  %9d bytes  %s\n", e.Path, len(e.Chunks), n, e.Digest[:12])
	}
	return nil
}
