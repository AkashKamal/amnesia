// Command overrides-fmt normalizes and sorts internal/corpus/data/overrides.tsv.
//
// overrides.tsv is the hand-maintained layer, and it carries two invariants a
// person editing it by hand will eventually break: every phrase must already be
// in corpus.Normalize form, and rows must be sorted by phrase, because Lookup
// binary-searches them. Getting either wrong makes exact lookup silently miss
// rather than fail loudly.
//
// So contributors write the phrase however reads naturally and run this:
//
//	go run ./tools/overrides-fmt
//
// It rewrites the file in place: phrases normalized, rows sorted, duplicate
// (platform, phrase, command) triples dropped. It never invents or deletes
// content beyond exact duplicates.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/AkashKamal/amnesia/internal/corpus"
)

func main() {
	path := flag.String("f", "internal/corpus/data/overrides.tsv", "file to format in place")
	check := flag.Bool("check", false, "exit non-zero if the file is not already formatted")
	flag.Parse()

	src, err := os.ReadFile(*path)
	if err != nil {
		fatal(err)
	}

	type row struct{ platform, tool, phrase, command, desc string }
	var rows []row
	seen := map[string]bool{}

	for i, line := range strings.Split(string(src), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 5 {
			fatal(fmt.Errorf("%s:%d: want 5 tab-separated fields, got %d", *path, i+1, len(f)))
		}
		r := row{f[0], f[1], corpus.Normalize(f[2]), f[3], f[4]}
		if r.phrase == "" {
			fatal(fmt.Errorf("%s:%d: phrase normalizes to nothing", *path, i+1))
		}
		key := r.platform + "\x00" + r.phrase + "\x00" + r.command
		if seen[key] {
			continue
		}
		seen[key] = true
		rows = append(rows, r)
	}

	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.phrase != b.phrase {
			return a.phrase < b.phrase
		}
		if a.platform != b.platform {
			return a.platform < b.platform
		}
		return a.command < b.command
	})

	var out strings.Builder
	w := bufio.NewWriter(&out)
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.platform, r.tool, r.phrase, r.command, r.desc)
	}
	w.Flush()

	if *check {
		if out.String() != string(src) {
			fmt.Fprintf(os.Stderr, "%s is not formatted; run: go run ./tools/overrides-fmt\n", *path)
			os.Exit(1)
		}
		fmt.Printf("%s: formatted, %d rows\n", *path, len(rows))
		return
	}

	if err := os.WriteFile(*path, []byte(out.String()), 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("%s: %d rows, normalized and sorted\n", *path, len(rows))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "overrides-fmt:", err)
	os.Exit(1)
}
