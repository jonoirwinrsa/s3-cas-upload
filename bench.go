package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"
)

type row struct {
	name string
	st   *stats
	hits int
}

func benchmark(store *s3, signer, root, cachePath string, chunk, seed int64, hc *http.Client) error {
	if _, err := store.empty(blobPrefix); err != nil {
		return err
	}
	os.Remove(cachePath)

	var rows []row
	run := func(name string, useCache bool) error {
		cl := &client{signer, chunk, loadCache(cachePath, useCache), hc}
		_, st, err := cl.Upload(root)
		if err != nil {
			return err
		}
		rows = append(rows, row{name, st, cl.cache.Hits})
		return nil
	}

	if err := run("cold", true); err != nil {
		return err
	}

	picks, err := pickFiles(root, 2, seed)
	if err != nil {
		return err
	}

	// Same length, new contents: the cache sees the mtime move and the store
	// has to take the bytes.
	info, err := os.Stat(picks[0])
	if err != nil {
		return err
	}
	b := make([]byte, info.Size())
	rand.New(rand.NewSource(seed + 1)).Read(b)
	if err := os.WriteFile(picks[0], b, 0o644); err != nil {
		return err
	}
	if err := run("changed, cached", true); err != nil {
		return err
	}
	if err := run("changed, rehashed", false); err != nil {
		return err
	}

	if err := os.Remove(picks[1]); err != nil {
		return err
	}
	if err := run("deleted", true); err != nil {
		return err
	}

	printTable(rows)
	return nil
}

func printTable(rows []row) {
	hdr := []string{""}
	for _, r := range rows {
		hdr = append(hdr, r.name)
	}
	cell := func(v ...string) {
		fmt.Printf("| %-18s |", v[0])
		for _, s := range v[1:] {
			fmt.Printf(" %17s |", s)
		}
		fmt.Println()
	}
	cell(hdr...)
	sep := []string{strings.Repeat("-", 18)}
	for range rows {
		sep = append(sep, strings.Repeat("-", 17))
	}
	cell(sep...)

	metric := func(name string, f func(row) string) {
		v := []string{name}
		for _, r := range rows {
			v = append(v, f(r))
		}
		cell(v...)
	}
	metric("bytes uploaded", func(r row) string { return fmt.Sprint(r.st.BytesUp) })
	metric("PUT requests", func(r row) string { return fmt.Sprint(r.st.Puts) })
	metric("existence checks", func(r row) string { return fmt.Sprint(r.st.Checks) })
	metric("files hashed", func(r row) string { return fmt.Sprint(r.st.Hashed) })
	metric("bytes read", func(r row) string { return fmt.Sprint(r.st.BytesRead) })
	metric("wall clock", func(r row) string { return r.st.Wall.Round(time.Millisecond).String() })
}

// crossover asks only about digests already present, so no URLs get signed and
// the timing is the existence check alone.
func crossover(store *s3, signer string, counts []int) error {
	have, err := store.listPrefix(blobPrefix)
	if err != nil {
		return err
	}
	var digests []string
	for k := range have {
		digests = append(digests, strings.TrimPrefix(k, blobPrefix))
	}
	fmt.Printf("bucket holds %d blobs\n\n", len(digests))
	fmt.Printf("%10s %12s %12s\n", "digests", "head", "list")

	ask := func(n int, strategy string) (time.Duration, error) {
		body, _ := json.Marshal(map[string]any{"digests": digests[:n]})
		start := time.Now()
		resp, err := http.Post(signer+"/missing?strategy="+strategy, "application/json", strings.NewReader(string(body)))
		if err != nil {
			return 0, err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return time.Since(start), nil
	}

	for _, n := range counts {
		if n > len(digests) {
			break
		}
		h, err := ask(n, "head")
		if err != nil {
			return err
		}
		l, err := ask(n, "list")
		if err != nil {
			return err
		}
		fmt.Printf("%10d %12s %12s\n", n, h.Round(time.Millisecond), l.Round(time.Millisecond))
	}
	return nil
}
