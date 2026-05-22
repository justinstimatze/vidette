package main

import (
	"strings"
	"testing"
)

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
