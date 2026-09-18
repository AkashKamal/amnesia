// Command corpus-gen builds internal/corpus/data/commands.tsv from a tldr-pages
// checkout.
//
// The generated layer is never hand-written. A curated set of thousands of
// docker/kubectl/ffmpeg invocations is a full-time maintenance job that goes
// stale within a release; tldr-pages already has a community doing exactly that
// job, with per-platform pages and a permissive licence. Our value is the
// lookup and the cascade, not re-typing man pages.
//
// Hand curation lives in the sibling overrides.tsv, which this command never
// touches. See the package comment in internal/corpus for why there are two
// layers.
//
// The output is committed so the build needs no network and a bad generator run
// shows up as a reviewable diff.
//
//	git clone --depth 1 https://github.com/tldr-pages/tldr /tmp/tldr
//	go run ./tools/corpus-gen -tldr /tmp/tldr -out internal/corpus/data/commands.tsv
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/AkashKamal/amnesia/internal/corpus"
)

// Only platforms internal/corpus understands. tldr also ships android, sunos
// and the BSDs; adding one means adding it to corpus.platformOK and to the
// invariant test, in that order.
var wanted = map[string]bool{"common": true, "linux": true, "osx": true, "windows": true}

// Rows per (platform, phrase). The resolver shows at most three candidates, so
// a fourth row is weight in the binary that no user can ever see.
const maxPerPhrase = 3

// brackets strips tldr's mnemonic flag notation: "List [a]ll containers".
// corpus.Normalize deletes the brackets for matching, and the human-readable
// description should agree with what was matched.
var brackets = strings.NewReplacer("[", "", "]", "")

type row struct {
	platform, tool, phrase, command, desc string
}

func main() {
	tldr := flag.String("tldr", "", "path to a tldr-pages checkout")
	out := flag.String("out", "internal/corpus/data/commands.tsv", "output TSV")
	flag.Parse()

	if *tldr == "" {
		fmt.Fprintln(os.Stderr, "corpus-gen: -tldr is required")
		os.Exit(2)
	}

	// pages/ is English. pages.xx/ are translations, and an English query must
	// not fuzzy-match a Spanish description.
	root := filepath.Join(*tldr, "pages")
	rows, err := walk(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "corpus-gen:", err)
		os.Exit(1)
	}
	if len(rows) == 0 {
		fmt.Fprintf(os.Stderr, "corpus-gen: no pages found under %s\n", root)
		os.Exit(1)
	}

	// Sorted by phrase: the invariant corpus.Lookup's binary search depends on.
	// Ties broken by platform then command so output is deterministic and
	// regeneration diffs stay readable.
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.phrase != b.phrase {
			return a.phrase < b.phrase
		}
		if a.platform != b.platform {
			return a.platform < b.platform
		}
		return a.command < b.command
	})

	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "corpus-gen:", err)
		os.Exit(1)
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	seen := map[string]int{}
	dupe := map[string]bool{}
	written := 0
	for _, r := range rows {
		k := r.platform + "|" + r.phrase
		if seen[k] >= maxPerPhrase {
			continue
		}
		// The same phrase and the same command from two different pages is
		// pure duplication, and it would waste one of the three slots.
		if dupe[k+"|"+r.command] {
			continue
		}
		dupe[k+"|"+r.command] = true
		seen[k]++
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", r.platform, r.tool, r.phrase, r.command, r.desc)
		written++
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, "corpus-gen:", err)
		os.Exit(1)
	}
	fmt.Printf("corpus-gen: %d rows written from %d examples -> %s\n", written, len(rows), *out)
}

func walk(root string) ([]row, error) {
	var rows []row
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		platform := filepath.Base(filepath.Dir(path))
		if !wanted[platform] {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		body := string(b)
		// Alias pages carry no real examples, only "View documentation for the
		// original command" pointing at another page. Indexing those would put
		// "tldr docker container ls" in front of someone who asked about docker.
		if strings.Contains(body, "This command is an alias of") {
			return nil
		}
		rows = append(rows, parsePage(platform, body)...)
		return nil
	})
	return rows, err
}

// parsePage reads one tldr page:
//
//	# docker ps
//	> List containers.
//	- List all containers, running and stopped:
//	`docker ps --all`
//
// The "- ..." line becomes the phrase, the backticked line becomes the command.
func parsePage(platform, content string) []row {
	var (
		out  []row
		tool string
		desc string
	)

	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#"):
			// "# docker ps" -> tool is "docker"; the page covers a subcommand.
			title := strings.TrimSpace(strings.TrimPrefix(line, "#"))
			tool, _, _ = strings.Cut(title, " ")

		case strings.HasPrefix(line, "-"):
			desc = strings.TrimSpace(strings.TrimPrefix(line, "-"))
			desc = brackets.Replace(strings.TrimSuffix(desc, ":"))

		case strings.HasPrefix(line, "`") && strings.HasSuffix(line, "`") && len(line) > 2:
			cmd := placeholders(strings.Trim(line, "`"))
			if tool == "" || desc == "" || !usable(cmd, desc) {
				continue
			}
			phrase := corpus.Normalize(desc)
			if phrase == "" {
				continue
			}
			out = append(out, row{platform, tool, phrase, cmd, desc})
			desc = ""
		}
	}
	return out
}

// usable rejects rows that cannot survive the pipeline: tabs and newlines would
// corrupt the TSV, anything this long is a script rather than something a user
// can read off a confirm prompt and approve, and leftover braces mean
// placeholders() did not understand the row.
func usable(cmd, desc string) bool {
	if cmd == "" || len(cmd) > 200 || len(desc) > 160 {
		return false
	}
	if strings.ContainsAny(cmd, "\t\n\r") || strings.ContainsAny(desc, "\t\n\r") {
		return false
	}
	return !strings.Contains(cmd, "{{") && !strings.Contains(cmd, "}}")
}

// placeholders rewrites tldr's {{path/to/file}} into <path-to-file>, the form
// cmd/amnesia refuses to auto-execute. Keeping tldr's syntax would mean either
// running a literal "{{...}}" or teaching the CLI a second template language.
func placeholders(cmd string) string {
	var b strings.Builder
	for {
		i := strings.Index(cmd, "{{")
		if i < 0 {
			b.WriteString(cmd)
			return b.String()
		}
		j := strings.Index(cmd[i:], "}}")
		if j < 0 {
			b.WriteString(cmd)
			return b.String()
		}
		b.WriteString(cmd[:i])
		b.WriteString("<" + slug(cmd[i+2:i+j]) + ">")
		cmd = cmd[i+j+2:]
	}
}

func slug(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	// The placeholder must stay recognisable to cmd/amnesia, which refuses to
	// run anything matching <name>. An empty slug would produce a literal <>.
	if out == "" || (out[0] < 'a' || out[0] > 'z') && out[0] != '_' {
		return "value"
	}
	return out
}
