// Command stress exercises a built amnesia binary the way a working engineer
// would, and reports what it finds.
//
// It is written in Go rather than shell so that Linux, macOS and Windows run
// byte-identical logic. A bash harness would need jq, GNU sed and bash 4 on a
// mac that ships none of them.
//
//	go run ./stress -bin ./amnesia -mode all
//
// Modes:
//
//	accuracy     golden query set, scored hit@1 / hit@3, latency percentiles
//	robustness   hostile and malformed input; nothing may hang, panic or exit oddly
//	safety       every destructive command must be classified as such
//	concurrency  many processes against one cache file at once
//	audit        every command in the corpus, checked against the classifier
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AkashKamal/amnesia/internal/risk"
)

type result struct {
	Command    string  `json:"command"`
	Desc       string  `json:"description"`
	Tool       string  `json:"tool"`
	Source     string  `json:"source"`
	Confidence float64 `json:"confidence"`
	Risk       string  `json:"risk"`
	RiskReason string  `json:"risk_reason"`
	Installed  bool    `json:"installed"`
}

type run struct {
	results []result
	err     error
	code    int
	stderr  string
	took    time.Duration
	timeout bool
}

var (
	bin     = flag.String("bin", "amnesia", "path to the amnesia binary under test")
	mode    = flag.String("mode", "all", "accuracy | robustness | safety | concurrency | all")
	verbose = flag.Bool("v", false, "print every query result, not just failures")
	home    string
)

func main() {
	flag.Parse()

	dir, err := os.MkdirTemp("", "amnesia-stress")
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(dir)
	home = dir

	if _, err := exec.LookPath(*bin); err != nil {
		if _, statErr := os.Stat(*bin); statErr != nil {
			fatal(fmt.Errorf("cannot find binary %q: %v", *bin, err))
		}
	}

	fmt.Printf("amnesia stress test\n")
	fmt.Printf("binary   %s\n", *bin)
	fmt.Printf("platform %s\n", platform())
	fmt.Println(strings.Repeat("=", 78))

	failed := false
	switch *mode {
	case "accuracy":
		failed = accuracy()
	case "robustness":
		failed = robustness()
	case "safety":
		failed = safety()
	case "concurrency":
		failed = concurrency()
	case "audit":
		failed = audit()
	default:
		failed = accuracy()
		failed = robustness() || failed
		failed = safety() || failed
		failed = concurrency() || failed
		failed = audit() || failed
	}

	fmt.Println(strings.Repeat("=", 78))
	if failed {
		fmt.Println("RESULT: issues found (see above)")
		os.Exit(1)
	}
	fmt.Println("RESULT: no blocking issues")
}

func platform() string {
	out, _ := exec.Command(*bin, "doctor").Output()
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "platform") {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "platform"))
		}
	}
	return "unknown"
}

// ask runs one query. Offline, and against a scratch home, so the measurement
// is of the corpus and resolver rather than of somebody's cache or API key.
func ask(args ...string) run {
	cmd := exec.Command(*bin, args...)
	cmd.Env = append(os.Environ(),
		"AMNESIA_OFFLINE=1",
		"AMNESIA_HOME="+home,
		"NO_COLOR=1",
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return run{err: err}
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var r run
	select {
	case err := <-done:
		r.took = time.Since(start)
		r.err = err
		if ee, ok := err.(*exec.ExitError); ok {
			r.code = ee.ExitCode()
			r.err = nil
		}
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		r.took = time.Since(start)
		r.timeout = true
		r.code = -1
	}

	r.stderr = stderr.String()
	if s := strings.TrimSpace(stdout.String()); s != "" {
		_ = json.Unmarshal([]byte(s), &r.results)
	}
	return r
}

// ---------------------------------------------------------------- accuracy --

type query struct {
	text    string
	expect  []string
	tag     string
	scored  bool
	lineNum int
}

func loadQueries() ([]query, error) {
	path := filepath.Join("stress", "queries.txt")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []query
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "|||")
		if len(parts) < 2 {
			continue
		}
		q := query{text: strings.TrimSpace(parts[0]), lineNum: i + 1}
		exp := strings.TrimSpace(parts[1])
		if len(parts) > 2 {
			q.tag = strings.TrimSpace(parts[2])
		}
		if exp != "" {
			for _, alt := range strings.Split(exp, "||") {
				if alt = strings.TrimSpace(alt); alt != "" {
					q.expect = append(q.expect, strings.ToLower(alt))
				}
			}
			q.scored = len(q.expect) > 0
		}
		out = append(out, q)
	}
	return out, nil
}

func rankOfMatch(rs []result, expect []string) int {
	for i, r := range rs {
		if i >= 3 {
			break
		}
		cmd := strings.ToLower(r.Command)
		for _, alt := range expect {
			if strings.Contains(cmd, alt) {
				return i + 1
			}
		}
	}
	return 0
}

func accuracy() bool {
	queries, err := loadQueries()
	if err != nil {
		fatal(err)
	}

	type sample struct {
		conf    float64
		correct bool
	}
	var samples []sample

	var (
		hit1, hit3, scored, noAnswer int
		uninstalledTop               int
		lowConfidenceWrong           int
		durations                    []time.Duration
		bySource                     = map[string]int{}
		byTag                        = map[string][2]int{} // [hit1, scored]
		misses                       []string
	)

	fmt.Printf("\n## accuracy  (%d queries, offline, no model)\n\n", len(queries))

	for _, q := range queries {
		r := ask(append([]string{"--json"}, strings.Fields(q.text)...)...)
		durations = append(durations, r.took)

		if r.timeout {
			fmt.Printf("  TIMEOUT  %q\n", q.text)
			noAnswer++
			continue
		}
		if len(r.results) == 0 {
			noAnswer++
			if q.scored {
				scored++
				misses = append(misses, fmt.Sprintf("no answer      %-48s", q.text))
			}
			continue
		}

		top := r.results[0]
		bySource[top.Source]++
		if !top.Installed {
			uninstalledTop++
		}

		if !q.scored {
			if *verbose {
				fmt.Printf("  ---      %-46s -> %s\n", q.text, top.Command)
			}
			continue
		}

		scored++
		rank := rankOfMatch(r.results, q.expect)
		samples = append(samples, sample{top.Confidence, rank == 1})
		t := byTag[q.tag]
		t[1]++
		switch {
		case rank == 1:
			hit1++
			hit3++
			t[0]++
		case rank > 1:
			hit3++
			misses = append(misses, fmt.Sprintf("rank %d         %-48s -> %s", rank, q.text, top.Command))
		default:
			if top.Confidence < 0.7 {
				lowConfidenceWrong++
			}
			misses = append(misses, fmt.Sprintf("wrong (%.2f %s) %-44s -> %s",
				top.Confidence, top.Source, q.text, top.Command))
		}
		byTag[q.tag] = t

		if *verbose && rank == 1 {
			fmt.Printf("  ok       %-46s -> %s\n", q.text, top.Command)
		}
	}

	for _, m := range misses {
		fmt.Printf("  %s\n", m)
	}

	pct := func(n, d int) string {
		if d == 0 {
			return "n/a"
		}
		return fmt.Sprintf("%d/%d (%d%%)", n, d, n*100/d)
	}

	fmt.Printf("\n  accuracy @1        %s\n", pct(hit1, scored))
	fmt.Printf("  accuracy @3        %s\n", pct(hit3, scored))
	fmt.Printf("  no answer at all   %d\n", noAnswer)
	fmt.Printf("  top result is for an uninstalled tool: %d\n", uninstalledTop)
	fmt.Printf("  wrong AND low confidence (<0.70): %d  <- these should have escalated\n", lowConfidenceWrong)

	fmt.Printf("\n  by source: ")
	var srcs []string
	for s := range bySource {
		srcs = append(srcs, s)
	}
	sort.Strings(srcs)
	for _, s := range srcs {
		fmt.Printf("%s=%d  ", s, bySource[s])
	}
	fmt.Println()

	fmt.Printf("\n  by category (hit@1):\n")
	var tags []string
	for t := range byTag {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	for _, t := range tags {
		v := byTag[t]
		fmt.Printf("    %-10s %s\n", t, pct(v[0], v[1]))
	}

	// Threshold sweep. The floor decides when amnesia answers from the corpus
	// and when it escalates to the model, and picking it by intuition is how a
	// tool ends up confidently wrong. Each row is what WOULD happen at that
	// floor, computed from this one run.
	fmt.Printf("\n  confidence floor sweep (what escalates to the model):\n")
	fmt.Printf("    floor   kept-right  kept-WRONG  escalated\n")
	for _, f := range []float64{0.50, 0.55, 0.60, 0.65, 0.70, 0.75, 0.80, 0.90} {
		keptRight, keptWrong, escalated := 0, 0, 0
		for _, s := range samples {
			switch {
			case s.conf < f:
				escalated++
			case s.correct:
				keptRight++
			default:
				keptWrong++
			}
		}
		mark := ""
		if f == 0.55 {
			mark = "  <- previous default"
		}
		fmt.Printf("    %.2f    %-11d %-11d %d%s\n", f, keptRight, keptWrong, escalated, mark)
	}

	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	if n := len(durations); n > 0 {
		fmt.Printf("\n  latency  p50 %v   p90 %v   p99 %v   max %v\n",
			durations[n/2].Round(time.Millisecond),
			durations[n*90/100].Round(time.Millisecond),
			durations[min(n*99/100, n-1)].Round(time.Millisecond),
			durations[n-1].Round(time.Millisecond))
	}

	// Accuracy is reported, not asserted: a threshold here would just get
	// ratcheted down. Only a hang or a crash fails the run.
	return false
}

// -------------------------------------------------------------- robustness --

func robustness() bool {
	fmt.Printf("\n## robustness  (hostile and malformed input)\n\n")

	cases := []struct {
		name string
		args []string
	}{
		{"empty string", []string{""}},
		{"only whitespace", []string{"   "}},
		{"only punctuation", []string{"???", "!!!"}},
		{"only filler words", []string{"how", "do", "i", "the", "a"}},
		{"single character", []string{"x"}},
		{"very long query", []string{strings.Repeat("containerize ", 400)}},
		{"1000 separate args", strings.Fields(strings.Repeat("a ", 1000))},
		{"unicode", []string{"показать", "запущенные", "контейнеры"}},
		{"emoji", []string{"🐳", "list", "containers", "🚀"}},
		{"null-ish bytes", []string{"list\x01containers\x02"}},
		{"shell metacharacters", []string{"list; rm -rf /", "&&", "whoami"}},
		{"backticks", []string{"list `whoami` containers"}},
		{"dollar substitution", []string{"list $(whoami) containers"}},
		{"newline in query", []string{"list\ncontainers"}},
		{"path traversal", []string{"../../../etc/passwd"}},
		{"format string", []string{"%s %d %n %p"}},
		{"sql-ish", []string{"'; DROP TABLE users; --"}},
		{"looks like a flag", []string{"--not-a-real-flag"}},
		{"unknown subcommand-ish", []string{"dcoker", "ps"}},
		{"repeated flags", []string{"--offline", "--offline", "--json", "list", "pods"}},
		{"json and yes together", []string{"--json", "--yes", "list", "pods"}},
	}

	problems := 0
	for _, c := range cases {
		r := ask(c.args...)
		status := "ok"
		switch {
		case r.timeout:
			status = "HANG (killed after 20s)"
			problems++
		case r.err != nil:
			status = fmt.Sprintf("SPAWN ERROR: %v", r.err)
			problems++
		case strings.Contains(r.stderr, "panic:") || strings.Contains(r.stderr, "goroutine "):
			status = "PANIC"
			problems++
		case r.code != 0 && r.code != 1 && r.code != 2:
			status = fmt.Sprintf("odd exit code %d", r.code)
			problems++
		}

		// A query must never come back with a command it is willing to run that
		// contains text the user did not ask about executing.
		for _, res := range r.results {
			if strings.Contains(res.Command, "rm -rf /") && res.Risk != "destructive" {
				status = "UNCLASSIFIED DESTRUCTIVE COMMAND"
				problems++
			}
		}

		mark := " "
		if status != "ok" {
			mark = "!"
		}
		fmt.Printf("  %s %-26s exit=%-3d %6v  %s\n", mark, c.name, r.code, r.took.Round(time.Millisecond), status)
	}

	fmt.Printf("\n  problems: %d\n", problems)
	return problems > 0
}

// ------------------------------------------------------------------ safety --

func safety() bool {
	fmt.Printf("\n## safety  (destructive commands must be labelled)\n\n")

	// Queries whose best answer is genuinely dangerous. If amnesia returns one
	// of these as "safe", a user one keypress from disaster is not warned.
	dangerous := []struct {
		query string
		want  string
	}{
		{"remove unused docker images and volumes", "destructive"},
		{"delete a kubernetes pod", "destructive"},
		{"discard all my local changes", "destructive"},
		{"force push to a remote branch", "destructive"},
		{"delete all files in a directory recursively", "destructive"},
		{"format a disk partition", "destructive"},
		{"stop a running service", "caution"},
		{"kill a process", "caution"},
	}

	problems, missedQuery := 0, 0
	for _, d := range dangerous {
		r := ask(append([]string{"--json"}, strings.Fields(d.query)...)...)
		if len(r.results) == 0 {
			fmt.Printf("    %-46s no answer\n", d.query)
			continue
		}
		top := r.results[0]
		mark, note := " ", ""
		switch {
		case !looksDangerous(top.Command):
			// The classifier is not wrong here - this command genuinely is not
			// dangerous. The query simply never surfaced the dangerous one,
			// which is an accuracy gap, not a safety gap. Conflating the two
			// would make this mode cry wolf.
			mark, note = "-", "  (query missed; not a classifier fault)"
			missedQuery++
		case top.Risk == "safe":
			mark, note = "!", "  <- MISLABELLED"
			problems++
		}
		fmt.Printf("  %s %-44s %-12s %s%s\n", mark, d.query, top.Risk, trunc(top.Command, 46), note)
	}

	// The reverse: harmless read-only commands must not be over-flagged, or the
	// warning becomes noise people click through.
	fmt.Println()
	harmless := []string{
		"list running containers",
		"show me what changed",
		"list pods in all namespaces",
		"show disk space",
		"show the commit history as a graph",
	}
	falsePositives := 0
	for _, q := range harmless {
		r := ask(append([]string{"--json"}, strings.Fields(q)...)...)
		if len(r.results) == 0 {
			continue
		}
		if top := r.results[0]; top.Risk != "safe" {
			fmt.Printf("  ! %-46s over-flagged as %s (%s) -> %s\n", q, top.Risk, top.RiskReason, top.Command)
			falsePositives++
		}
	}
	fmt.Printf("\n  MISLABELLED dangerous: %d   over-flagged harmless: %d   query missed the dangerous command: %d\n", problems, falsePositives, missedQuery)
	return problems > 0
}

// ------------------------------------------------------------- concurrency --

// concurrency runs many processes against one cache file. The cache is appended
// to by every invocation that consults a model, and engineers do run several
// shells at once.
func concurrency() bool {
	fmt.Printf("\n## concurrency  (32 processes, shared cache and config)\n\n")

	const n = 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string

	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := ask("--json", "list", "running", "containers")
			if r.timeout || r.err != nil || (r.code != 0 && r.code != 1) {
				mu.Lock()
				failures = append(failures, fmt.Sprintf("worker %d: code=%d timeout=%v err=%v", i, r.code, r.timeout, r.err))
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	for _, f := range failures {
		fmt.Printf("  ! %s\n", f)
	}
	fmt.Printf("  %d processes in %v (%v each, wall clock)\n", n, elapsed.Round(time.Millisecond), (elapsed / n).Round(time.Millisecond))

	// A corrupted cache is the failure that matters: it would poison every
	// later run, not just this one.
	cachePath := filepath.Join(home, "learned.jsonl")
	if b, err := os.ReadFile(cachePath); err == nil {
		bad := 0
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line == "" {
				continue
			}
			var v map[string]any
			if json.Unmarshal([]byte(line), &v) != nil {
				bad++
			}
		}
		fmt.Printf("  cache lines unparseable after concurrent writes: %d\n", bad)
		if bad > 0 {
			failures = append(failures, "corrupt cache")
		}
	} else {
		fmt.Printf("  cache not written (expected: offline runs never call a model)\n")
	}

	fmt.Printf("\n  problems: %d\n", len(failures))
	return len(failures) > 0
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "stress:", err)
	os.Exit(2)
}

// ---------------------------------------------------------------- audit ----

// audit classifies every command that ships in the binary.
//
// The end-to-end safety mode only reaches commands some query happens to
// surface. This reads the corpus directly and asks the real question: is there
// ANY row amnesia could offer that is dangerous and labelled safe? At thirty
// thousand rows, sampling cannot answer that.
func audit() bool {
	fmt.Printf("\n\n\\ audit  (classify every shippable command)\n\n")

	// Deliberately independent of internal/risk's own patterns. If the two
	// lists were derived from each other, the audit would only prove that the
	// classifier agrees with itself.
	danger := []struct {
		name string
		re   *regexp.Regexp
	}{
		{"recursive force delete", regexp.MustCompile(`(?i)\brm\s+-[a-z]*r[a-z]*f|\brm\s+-[a-z]*f[a-z]*r`)},
		{"disk overwrite", regexp.MustCompile(`(?i)\bdd\b.*of=/dev/|\bmkfs|\bdiskpart\b|\bformat\s+[a-z]:`)},
		{"history rewrite", regexp.MustCompile(`(?i)git\s+push\b.*(--force|-f)\b`)},
		{"cluster delete", regexp.MustCompile(`(?i)kubectl\s+delete\b`)},
		{"drop database", regexp.MustCompile(`(?i)\bdrop\s+(database|table)\b`)},
		{"remote script to shell", regexp.MustCompile(`(?i)(curl|wget)\b[^|]*\|\s*(sudo\s+)?(ba|z)?sh`)},
		{"windows recursive delete", regexp.MustCompile(`(?i)\b(rd|rmdir)\s+/s|Remove-Item[^|]*-Recurse[^|]*-Force`)},
	}

	files := []string{
		filepath.Join("internal", "corpus", "data", "overrides.tsv"),
		filepath.Join("internal", "corpus", "data", "commands.tsv"),
	}

	rows, flagged, missed, shown := 0, 0, 0, 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			fmt.Printf("  cannot read %s: %v\n", f, err)
			return true
		}
		for _, line := range strings.Split(string(b), "\n") {
			cols := strings.Split(strings.TrimRight(line, "\r"), "\t")
			if len(cols) != 5 {
				continue
			}
			rows++
			cmd := cols[3]
			level, _ := risk.Classify(cmd)
			if level != risk.Safe {
				flagged++
			}
			for _, d := range danger {
				if d.re.MatchString(cmd) && level != risk.Destructive {
					missed++
					if shown < 12 {
						fmt.Printf("  ! %-24s %-11s %s\n", d.name, level, trunc(cmd, 58))
						shown++
					}
					break
				}
			}
		}
	}

	fmt.Printf("\n  rows audited            %d\n", rows)
	fmt.Printf("  flagged caution/destr.  %d (%d%%)\n", flagged, flagged*100/maxInt(rows, 1))
	fmt.Printf("  dangerous but unflagged %d\n", missed)
	return missed > 0
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "..."
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// looksDangerous reuses the audit patterns so the safety mode judges the command
// it actually got back, not the intent of the question that produced it.
func looksDangerous(cmd string) bool {
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`(?i)\brm\s+-[a-z]*r[a-z]*f`),
		regexp.MustCompile(`(?i)\bdd\b.*of=/dev/|\bmkfs|\bdiskpart\b`),
		regexp.MustCompile(`(?i)git\s+push\b.*(--force|-f)\b|git\s+reset\s+--hard`),
		regexp.MustCompile(`(?i)kubectl\s+delete\b|docker\s+system\s+prune`),
		regexp.MustCompile(`(?i)\b(rd|rmdir)\s+/s|Remove-Item[^|]*-Recurse`),
		regexp.MustCompile(`(?i)\b(net|sc)\s+stop\b|\bsystemctl\s+stop\b|\bfuser\s+-k|\bkill\b`),
		regexp.MustCompile(`(?i)\bsgdisk\b|\bfdisk\b|\bparted\b`),
	} {
		if re.MatchString(cmd) {
			return true
		}
	}
	return false
}
