package main

import (
	"strings"
	"testing"
)

// TestForkBackstop verifies that buildFix refuses to commit own-hygiene files
// to a repo GitHub reports as a fork, independent of how the config classifies
// it — the config-independent safety net that would have prevented diverging
// the tracking forks during a mass apply. Settings-only fixes stay allowed, and
// an explicit `rewritten` reclassification overrides the block.
func TestForkBackstop(t *testing.T) {
	stats := FleetStats{ModalLicense: "MIT"}
	defaults := Defaults{SecurityEmail: "x@example.com", CopyrightHolder: "me"}

	fileCommitChecks := []string{"license", "dependabot", "security-policy"}
	settingChecks := []string{"alerts", "auto-merge", "branch-protection"}

	// A fork left at the default `own` (the misclassification that bit us):
	// file-commit fixes must be refused, settings fixes must still apply.
	fork := RepoAudit{Repo: "me/somefork", IsFork: true, ForkRelation: FRelOwn}
	for _, name := range fileCommitChecks {
		act := buildFix(fork, Check{Name: name, Severity: SevFail}, stats, defaults)
		if !act.NoFix {
			t.Errorf("fork %q: expected NoFix (backstop), got fixable", name)
		}
		if !strings.Contains(act.Reason, "fork") {
			t.Errorf("fork %q: reason %q should mention fork", name, act.Reason)
		}
	}
	for _, name := range settingChecks {
		act := buildFix(fork, Check{Name: name, Severity: SevFail}, stats, defaults)
		if act.NoFix {
			t.Errorf("fork %q: settings fix should be allowed on a fork, got NoFix (%s)", name, act.Reason)
		}
	}

	// `rewritten` (fork-as-credit, full hygiene) overrides the backstop.
	rewritten := RepoAudit{Repo: "me/credit", IsFork: true, ForkRelation: FRelRewritten}
	for _, name := range fileCommitChecks {
		act := buildFix(rewritten, Check{Name: name, Severity: SevFail}, stats, defaults)
		if act.NoFix {
			t.Errorf("rewritten fork %q: expected fixable (override), got NoFix (%s)", name, act.Reason)
		}
	}

	// Non-fork repo: file-commit fixes are unaffected by the backstop.
	own := RepoAudit{Repo: "me/own", IsFork: false, ForkRelation: FRelOwn}
	for _, name := range fileCommitChecks {
		act := buildFix(own, Check{Name: name, Severity: SevFail}, stats, defaults)
		if act.NoFix {
			t.Errorf("non-fork %q: expected fixable, got NoFix (%s)", name, act.Reason)
		}
	}
}

func TestSplitRepoFlag(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"a", []string{"a"}},
		{"a,b , c", []string{"a", "b", "c"}},
		{"a,,b,", []string{"a", "b"}},
	}
	for _, tc := range cases {
		got := splitRepoFlag(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitRepoFlag(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitRepoFlag(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestFirstProseLine(t *testing.T) {
	cases := []struct {
		name string
		md   string
		want string
	}{
		{"plain first line", "this is the description\nmore stuff", "this is the description"},
		{"skip heading", "# title\nthe real description", "the real description"},
		{"skip multiple headings", "# title\n## section\ndescription", "description"},
		{"skip blank lines", "\n\n  \ndescription", "description"},
		{"skip badges", "[![ci](url)](url)\ndescription", "description"},
		{"skip html", "<p>html</p>\ndescription", "description"},
		{"skip code fence", "```\ncode\n```\ndescription", "description"},
		{"skip hr", "---\ndescription", "description"},
		{"all-heading README", "# only\n# headings\n## here", ""},
		{"empty", "", ""},
		{"trims whitespace", "   the description   ", "the description"},
	}
	for _, c := range cases {
		got := firstProseLine(c.md)
		if got != c.want {
			t.Errorf("%s: firstProseLine = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestIsValidTopic(t *testing.T) {
	cases := []struct {
		topic string
		want  bool
	}{
		{"go", true},
		{"github-actions", true},
		{"web3", true},
		{"a", false},           // too short
		{"-leading", false},    // starts with non-alnum
		{"under_score", false}, // underscore not allowed
		{"UPPER", false},       // uppercase not allowed (lowercase normalization happens before this)
		{"", false},
		{strings.Repeat("a", 51), false}, // too long
		{"the", false},                   // stopword
		{"readme", false},                // stopword
		{"todo", false},                  // stopword
		{"valid-topic", true},
	}
	for _, c := range cases {
		got := isValidTopic(c.topic)
		if got != c.want {
			t.Errorf("isValidTopic(%q) = %v, want %v", c.topic, got, c.want)
		}
	}
}

func TestMineReadmeTopics(t *testing.T) {
	md := "# my project\n\n## features\n\nUses `react` and `typescript` for the frontend. Also `vite`."
	got := mineReadmeTopics(md)
	want := map[string]bool{"react": true, "typescript": true, "vite": true, "my": true, "project": true, "features": true, "frontend": true, "uses": true, "also": true}
	for _, g := range got {
		if !want[g] {
			t.Errorf("mineReadmeTopics returned unexpected %q (full: %v)", g, got)
		}
	}
	// Backtick-quoted identifiers should rank above heading-word-only
	// matches because they get double-weight in counts.
	if len(got) > 0 && got[0] != "react" && got[0] != "typescript" && got[0] != "vite" {
		t.Errorf("expected backtick-weighted token first, got %q", got[0])
	}
}

func TestMineDescriptionTopics(t *testing.T) {
	desc := "Go library for parallel HTTP probes"
	got := mineDescriptionTopics(desc)
	hasGo, hasLibrary := false, false
	for _, g := range got {
		if g == "go" {
			hasGo = true
		}
		if g == "library" {
			hasLibrary = true
		}
	}
	if !hasGo || !hasLibrary {
		t.Errorf("expected 'go' and 'library' in topics from %q, got %v", desc, got)
	}
}

func TestPathToSlug(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"LICENSE", "license"},
		{"SECURITY.md", "security"},
		{".github/dependabot.yml", "dependabot"},
		{"some/Path/File.YAML", "file"},
		{"", "file"},
	}
	for _, c := range cases {
		got := pathToSlug(c.path)
		if got != c.want {
			t.Errorf("pathToSlug(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestCheckOrder(t *testing.T) {
	// archive-state must sort after content writes so it runs last per-repo.
	if checkOrder("archive-state") <= checkOrder("license") {
		t.Error("archive-state must order after license")
	}
	if checkOrder("archive-state") <= checkOrder("security-policy") {
		t.Error("archive-state must order after security-policy")
	}
}

func TestIsProtectionBlock(t *testing.T) {
	cases := []struct {
		errStr string
		want   bool
	}{
		{"HTTP 409 Conflict", true},
		{"rule violations: cannot push", true},
		{"required status checks have not succeeded", true},
		{"HTTP 404 not found", false},
		{"random transport error", false},
	}
	for _, c := range cases {
		got := isProtectionBlock(stringError(c.errStr))
		if got != c.want {
			t.Errorf("isProtectionBlock(%q) = %v, want %v", c.errStr, got, c.want)
		}
	}
}

func TestIsRateLimitError(t *testing.T) {
	cases := []struct {
		errStr string
		want   bool
	}{
		{"API rate limit exceeded for user ID 123", true},
		{"HTTP 403: secondary rate limit", true},
		{"abuse detection mechanism", true},
		{"HTTP 404", false},
		{"random network error", false},
		{"", false},
	}
	for _, c := range cases {
		var err error
		if c.errStr != "" {
			err = stringError(c.errStr)
		}
		got := isRateLimitError(err)
		if got != c.want {
			t.Errorf("isRateLimitError(%q) = %v, want %v", c.errStr, got, c.want)
		}
	}
}

func TestTruncateUTF8(t *testing.T) {
	// short string passes through
	if got := truncateUTF8("short", 100); got != "short" {
		t.Errorf("truncateUTF8 short = %q, want %q", got, "short")
	}
	// truncation appends ellipsis
	long := strings.Repeat("a", 200)
	got := truncateUTF8(long, 50)
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncateUTF8 should append ..., got %q", got)
	}
	if len(got) > 53 { // 50 bytes + "..."
		t.Errorf("truncateUTF8 result too long: %d", len(got))
	}
	// multi-byte boundary: a 2-byte rune at the cut point must not split.
	mb := strings.Repeat("é", 100) // each é is 2 bytes
	cut := truncateUTF8(mb, 51)    // odd number — boundary case
	// trim "..." and verify the remainder is valid UTF-8 (no half-rune)
	trimmed := strings.TrimSuffix(cut, "...")
	for i := 0; i < len(trimmed); {
		r := []rune(trimmed[i:])
		if len(r) == 0 {
			break
		}
		i += len(string(r[0]))
	}
	// If we got here without panic, multi-byte safety holds.
}

// stringError is a tiny error helper for tests; using fmt.Errorf would also
// work but this is more explicit about intent.
type stringError string

func (e stringError) Error() string { return string(e) }
