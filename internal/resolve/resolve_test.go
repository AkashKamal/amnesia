package resolve

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AkashKamal/amnesia/internal/risk"
)

// nonsense is a query no corpus row can match at any stage, so tests about
// escalation to the model stay true as the generated corpus grows. Asserting
// "the corpus does not know about Azure" would break the day tldr adds an az
// page.
const nonsense = "zzqqwx frobnicate the quux manifold"

type stubModel struct {
	called int
	out    []Result
}

func (m *stubModel) Name() string { return "stub" }
func (m *stubModel) Suggest(context.Context, string, Env) ([]Result, error) {
	m.called++
	return m.out, nil
}

type mapCache map[string][]Result

func (c mapCache) Get(_ context.Context, key, platform string) ([]Result, error) {
	return c[platform+"|"+key], nil
}
func (c mapCache) Put(_ context.Context, key, platform string, r Result) error {
	r.Source = SourceLearned
	c[platform+"|"+key] = append(c[platform+"|"+key], r)
	return nil
}

var linux = Env{Platform: "linux", Shell: "bash"}

// newT builds a resolver that believes every tool is installed. Without this,
// results would depend on what happens to be on the CI runner: the docker test
// would pass on a machine with docker and fail on one without.
func newT(env Env) *Resolver {
	r := New(env)
	r.HasTool = func(string) bool { return true }
	return r
}

// newTWith builds a resolver where only the named tools exist.
func newTWith(env Env, tools ...string) *Resolver {
	have := make(map[string]bool, len(tools))
	for _, t := range tools {
		have[t] = true
	}
	r := New(env)
	r.HasTool = func(t string) bool { return have[t] }
	return r
}

func TestExactHitNeverReachesTheModel(t *testing.T) {
	m := &stubModel{}
	r := newT(linux)
	r.Model = m

	got, err := r.Resolve(context.Background(), "How do I check disk usage?")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got[0].Command != "df -h" {
		t.Fatalf("command = %q, want the curated df -h", got[0].Command)
	}
	if got[0].Source != SourceStaticExact {
		t.Fatalf("source = %v, want corpus", got[0].Source)
	}
	if got[0].Confidence != 1 {
		t.Errorf("confidence = %v, want 1 for an exact hit", got[0].Confidence)
	}
	if m.called != 0 {
		t.Fatalf("model called %d times on an exact hit", m.called)
	}
}

func TestPlatformFiltering(t *testing.T) {
	for _, tc := range []struct{ platform, wantTool string }{
		{"linux", "ss"},
		{"osx", "lsof"},
	} {
		got, err := newT(Env{Platform: tc.platform}).Resolve(context.Background(), "show listening ports")
		if err != nil {
			t.Fatalf("%s: %v", tc.platform, err)
		}
		if got[0].Tool != tc.wantTool {
			t.Errorf("%s: tool = %q (%q), want %q", tc.platform, got[0].Tool, got[0].Command, tc.wantTool)
		}
	}
}

func TestFuzzyAnswersARewordedQuery(t *testing.T) {
	m := &stubModel{}
	r := newT(linux)
	r.Model = m

	// Not a phrase in any row: "a" and "running" are extra, and word order
	// differs. Coverage scoring should still land on docker exec.
	got, err := r.Resolve(context.Background(), "shell into a running container")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got[0].Source != SourceStaticFuzzy {
		t.Fatalf("source = %v, want fuzzy", got[0].Source)
	}
	if got[0].Tool != "docker" || !strings.Contains(got[0].Command, "exec") {
		t.Fatalf("got %q (%s), want a docker exec form", got[0].Command, got[0].Tool)
	}
	if m.called != 0 {
		t.Fatal("model called for a query the corpus covers")
	}
}

func TestModelMissIsCached(t *testing.T) {
	ctx := context.Background()
	m := &stubModel{out: []Result{{Command: "frob --quux", Desc: "frobnicate"}}}
	cache := mapCache{}
	r := newT(linux)
	r.Model, r.Cache = m, cache

	if _, err := r.Resolve(ctx, nonsense); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	got, err := r.Resolve(ctx, nonsense)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if m.called != 1 {
		t.Fatalf("model called %d times; the second lookup should have hit the cache", m.called)
	}
	if got[0].Source != SourceLearned {
		t.Fatalf("source = %v, want cache", got[0].Source)
	}
}

func TestOfflineFailsInsteadOfGuessing(t *testing.T) {
	_, err := newT(linux).Resolve(context.Background(), nonsense)
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("err = %v, want ErrNoMatch", err)
	}
}

func TestEveryResultIsRiskClassified(t *testing.T) {
	got, err := newT(linux).Resolve(context.Background(), "remove unused images and volumes")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got[0].Risk != risk.Destructive {
		t.Fatalf("risk = %v, want destructive for %q", got[0].Risk, got[0].Command)
	}
	if got[0].RiskReason == "" {
		t.Fatal("destructive result carries no reason to show the user")
	}
}

// A model answer must be classified too: the model is the least trustworthy
// source in the cascade, so it must not be the one that skips the safety check.
func TestModelResultsAreRiskClassified(t *testing.T) {
	r := newT(linux)
	r.Model = &stubModel{out: []Result{{Command: "rm -rf /var/lib/thing", Tool: "rm"}}}

	got, err := r.Resolve(context.Background(), nonsense)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got[0].Risk != risk.Destructive {
		t.Fatalf("model result classified %v, want destructive", got[0].Risk)
	}
}

func TestMaxResultsIsHonoured(t *testing.T) {
	r := newT(linux)
	r.MaxResults = 1
	r.Model = &stubModel{out: []Result{{Command: "a"}, {Command: "b"}, {Command: "c"}}}

	got, err := r.Resolve(context.Background(), nonsense)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
}

func BenchmarkExact(b *testing.B) {
	ctx := context.Background()
	r := newT(linux)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := r.Resolve(ctx, "check disk usage"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFuzzyMiss(b *testing.B) {
	ctx := context.Background()
	r := newT(linux)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = r.Resolve(ctx, "shell into a running container")
	}
}

// A command for a tool this machine does not have is not an answer. This is the
// bug that made Windows useless in the first build: tldr's "common" platform is
// really "common across Unix-likes", so amnesia cheerfully offered df, lsof and
// aconnect on a machine that had none of them.
func TestUninstalledToolsRankBelowInstalledOnes(t *testing.T) {
	// Only git exists. "show what changed" should still find git status rather
	// than some Unix tool that scores similarly.
	r := newTWith(linux, "git")
	got, err := r.Resolve(context.Background(), "show what changed")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !got[0].Installed {
		t.Fatalf("top result %q (%s) is not installed", got[0].Command, got[0].Tool)
	}
	if got[0].Tool != "git" {
		t.Fatalf("tool = %q, want git (the only installed tool)", got[0].Tool)
	}
}

func TestExactMatchForAMissingToolDoesNotEndTheCascade(t *testing.T) {
	// "check disk usage" exact-matches the curated df row. With no df on the
	// machine and a model available, the model must still be consulted.
	m := &stubModel{out: []Result{{Command: "Get-PSDrive", Tool: "powershell"}}}
	r := newTWith(Env{Platform: "windows", Shell: "powershell"}, "powershell")
	r.Model = m

	got, err := r.Resolve(context.Background(), "check disk usage")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if m.called != 1 {
		t.Fatalf("model called %d times; an uninstalled exact hit must not end the cascade", m.called)
	}
	if got[0].Command != "Get-PSDrive" {
		t.Fatalf("got %q, want the model answer for an installed tool", got[0].Command)
	}
}

func TestUninstalledAnswerIsStillReturnedOffline(t *testing.T) {
	// Nothing installed, no model. Returning the df row flagged Installed=false
	// is more useful than ErrNoMatch - the user learns what to install.
	r := newTWith(linux)
	got, err := r.Resolve(context.Background(), "check disk usage")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got[0].Command != "df -h" {
		t.Fatalf("got %q, want df -h", got[0].Command)
	}
	if got[0].Installed {
		t.Fatal("df reported as installed when nothing is")
	}
}
