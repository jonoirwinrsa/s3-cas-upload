package main

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
)

// genTree seeds a reproducible tree with a heavy-tailed size distribution and
// some duplicate files, so dedup is exercised inside one upload as well as across two.
func genTree(root string, files int, totalBytes int64, dupFrac float64, seed int64) error {
	if err := os.RemoveAll(root); err != nil {
		return err
	}
	r := rand.New(rand.NewSource(seed))

	w := make([]float64, files)
	var sum float64
	for i := range w {
		x := r.Float64()
		w[i] = x * x * x
		sum += w[i]
	}

	var prev []string
	var wrote, dups int64
	for i := range files {
		dir := filepath.Join(root, fmt.Sprintf("d%02d", i%64))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		p := filepath.Join(dir, fmt.Sprintf("f%06d.bin", i))

		if len(prev) > 0 && r.Float64() < dupFrac {
			b, err := os.ReadFile(prev[r.Intn(len(prev))])
			if err != nil {
				return err
			}
			if err := os.WriteFile(p, b, 0o644); err != nil {
				return err
			}
			wrote += int64(len(b))
			dups++
			continue
		}

		n := int64(w[i]/sum*float64(totalBytes)) + 512
		b := make([]byte, n)
		r.Read(b)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return err
		}
		prev = append(prev, p)
		wrote += n
	}
	fmt.Printf("%d files, %d duplicates, %d bytes\n", files, dups, wrote)
	return nil
}

// pickFiles returns n paths from the tree, chosen by the same seed so a bench
// changes and deletes the same files every time.
func pickFiles(root string, n int, seed int64) ([]string, error) {
	var all []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			all = append(all, p)
		}
		return err
	})
	if err != nil || len(all) < n {
		return nil, err
	}
	r := rand.New(rand.NewSource(seed))
	out := make([]string, n)
	for i := range out {
		out[i] = all[r.Intn(len(all))]
	}
	return out, nil
}
