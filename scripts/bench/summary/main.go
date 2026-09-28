// Command summary prints a Markdown table of median benchmark results
// comparing the fork (candidate) with the upstream (baseline) binary.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const maxLineBytes = 4 << 20

type record struct {
	Binary     string `json:"binary"`
	CacheMode  string `json:"cache_mode"`
	Purpose    string `json:"purpose"`
	WallNS     int64  `json:"wall_ns"`
	UserCPUNS  int64  `json:"user_cpu_ns"`
	SysCPUNS   int64  `json:"system_cpu_ns"`
	PeakRSS    int64  `json:"peak_tree_rss_bytes"`
	ExitCode   int    `json:"exit_code"`
	CacheAfter int64  `json:"cache_bytes_after"`
}

type sample struct {
	record
	order string
}

func main() {
	if len(os.Args) != 2 {
		_, _ = fmt.Fprintln(os.Stderr, "usage: summary <results-root>")
		os.Exit(2)
	}
	samples, err := load(os.Args[1])
	if err == nil {
		err = write(os.Stdout, samples)
	}
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "benchmark summary: %v\n", err)
		os.Exit(1)
	}
}

// load reads every results.jsonl under dir; the parent directory names the
// binary execution order.
func load(dir string) ([]sample, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()

	var samples []sample
	err = fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "results.jsonl" {
			return err
		}
		f, err := root.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		order := filepath.Base(filepath.Dir(path))
		scanner := bufio.NewScanner(f)
		scanner.Buffer(nil, maxLineBytes)
		for scanner.Scan() {
			var r record
			if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if r.Purpose == "timing" {
				samples = append(samples, sample{r, order})
			}
		}
		return scanner.Err()
	})
	if err != nil {
		return nil, err
	}
	if len(samples) == 0 {
		return nil, errors.New("no timing samples found")
	}
	return samples, nil
}

type metric struct {
	name  string
	unit  string
	scale float64
	value func(record) int64
}

var metrics = []metric{
	{"wall", "s", 1e9, func(r record) int64 { return r.WallNS }},
	{"user CPU", "s", 1e9, func(r record) int64 { return r.UserCPUNS }},
	{"system CPU", "s", 1e9, func(r record) int64 { return r.SysCPUNS }},
	{"peak tree RSS", "MiB", 1 << 20, func(r record) int64 { return r.PeakRSS }},
	{"cache size", "KiB", 1 << 10, func(r record) int64 { return r.CacheAfter }},
}

func write(w io.Writer, samples []sample) error {
	var b strings.Builder
	failed := 0
	for _, s := range samples {
		if s.ExitCode != 0 {
			failed++
		}
	}
	if failed > 0 {
		fmt.Fprintf(&b, "> [!WARNING]\n> %d timing samples exited non-zero and are excluded.\n\n", failed)
	}

	for _, mode := range cacheModes(samples) {
		base := filter(samples, mode, "upstream", "")
		cand := filter(samples, mode, "fork", "")
		if len(base) == 0 || len(cand) == 0 {
			continue
		}
		fmt.Fprintf(&b, "### %s cache (%d candidate / %d baseline samples)\n\n", mode, len(cand), len(base))
		b.WriteString("| metric | baseline | candidate | Δ |\n|---|---|---|---|\n")
		for _, m := range metrics {
			bm, cm := median(base, m.value)/m.scale, median(cand, m.value)/m.scale
			fmt.Fprintf(&b, "| %s | %.3f %s | %.3f %s | %s |\n", m.name, bm, m.unit, cm, m.unit, delta(bm, cm))
		}
		for _, order := range orders(samples) {
			bo, co := filter(samples, mode, "upstream", order), filter(samples, mode, "fork", order)
			if len(bo) == 0 || len(co) == 0 {
				continue
			}
			wall := metrics[0]
			bm, cm := median(bo, wall.value)/wall.scale, median(co, wall.value)/wall.scale
			fmt.Fprintf(&b, "| wall, %s | %.3f s | %.3f s | %s |\n", order, bm, cm, delta(bm, cm))
		}
		b.WriteString("\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func cacheModes(samples []sample) []string {
	var modes []string
	for _, s := range samples {
		if !slices.Contains(modes, s.CacheMode) {
			modes = append(modes, s.CacheMode)
		}
	}
	slices.Sort(modes)
	return modes
}

func orders(samples []sample) []string {
	var out []string
	for _, s := range samples {
		if !slices.Contains(out, s.order) {
			out = append(out, s.order)
		}
	}
	slices.Sort(out)
	return out
}

func filter(samples []sample, mode, binary, order string) []record {
	var out []record
	for _, s := range samples {
		if s.CacheMode == mode && s.Binary == binary && s.ExitCode == 0 && (order == "" || s.order == order) {
			out = append(out, s.record)
		}
	}
	return out
}

func median(records []record, value func(record) int64) float64 {
	vs := make([]int64, len(records))
	for i, r := range records {
		vs[i] = value(r)
	}
	slices.Sort(vs)
	n := len(vs)
	if n%2 == 1 {
		return float64(vs[n/2])
	}
	return float64(vs[n/2-1]+vs[n/2]) / 2
}

func delta(base, cand float64) string {
	if base == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%+.1f%%", 100*(cand-base)/base)
}
