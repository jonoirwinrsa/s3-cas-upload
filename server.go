package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

type target struct {
	URL      string `json:"url"`
	Checksum string `json:"checksum"`
}

// server holds the bucket credentials. It answers existence checks and signs
// upload URLs, and never reads or hashes content.
type server struct {
	s3       *s3
	creds    creds
	endpoint string
	bucket   string
	strategy string // head or list
	ttl      time.Duration
}

const blobPrefix = "blobs/sha256/"

func blobKey(digest string) string { return blobPrefix + digest }

// missing also reports how many S3 requests the answer cost.
func (sv *server) missing(digests []string, strategy string) ([]string, int, error) {
	if strategy == "list" {
		before := sv.s3.lists.Load()
		have, err := sv.s3.listPrefix(blobPrefix)
		if err != nil {
			return nil, 0, err
		}
		var out []string
		for _, d := range digests {
			if !have[blobKey(d)] {
				out = append(out, d)
			}
		}
		return out, int(sv.s3.lists.Load() - before), nil
	}

	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		out  []string
		ferr error
	)
	sem := make(chan struct{}, 16)
	for _, d := range digests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ok, err := sv.s3.head(blobKey(d))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				ferr = err
			case !ok:
				out = append(out, d)
			}
		}()
	}
	wg.Wait()
	return out, len(digests), ferr
}

func (sv *server) handleMissing(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Digests []string `json:"digests"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	strategy := sv.strategy
	if q := r.URL.Query().Get("strategy"); q != "" {
		strategy = q
	}
	need, checks, err := sv.missing(req.Digests, strategy)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	upload := map[string]target{}
	for _, d := range need {
		u, sum, err := sv.creds.presign(sv.endpoint, sv.bucket, blobKey(d), d, sv.ttl, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		upload[d] = target{u, sum}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"upload": upload, "checks": checks})
}

func (sv *server) serve(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /missing", sv.handleMissing)
	return http.ListenAndServe(addr, mux)
}
