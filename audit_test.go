package main

import (
	"strings"
	"testing"
)

func TestExtractHomepageFromNotes(t *testing.T) {
	cases := []struct {
		notes string
		want  string
	}{
		// explicit URL
		{"live at https://timeknife.fyi", "https://timeknife.fyi"},
		{"deploy: https://example.com/app", "https://example.com/app"},
		// URL with trailing punctuation gets stripped
		{"see https://example.com.", "https://example.com"},
		{"link: https://foo.io)", "https://foo.io"},
		{"hits https://x.dev,", "https://x.dev"},
		// bare domain with recognized TLD
		{"live at timeknife.fyi", "https://timeknife.fyi"},
		{"hosted on schorldynamics.com", "https://schorldynamics.com"},
		{"see myproject.tech for details", "https://myproject.tech"},
		// subdomain
		{"docs at api.example.com", "https://api.example.com"},
		// no URL or recognizable domain
		{"this is just a note", ""},
		{"keep up to date with upstream; don't push back", ""},
		// empty
		{"", ""},
		// non-deploy hosts must be suppressed even when their TLD matches.
		// github.com, gitlab.com, npmjs.com etc. show up in notes as code
		// hosts, not as homepages.
		{"forked from github.com/foo/bar", ""},
		{"published on npmjs.com/package/foo", ""},
		{"see https://github.com/owner/repo/issues/4", ""},
		// but a real-looking deploy on a regular TLD still wins
		{"live at https://myapp.example.com", "https://myapp.example.com"},
	}
	for _, c := range cases {
		got := extractHomepageFromNotes(c.notes)
		if got != c.want {
			t.Errorf("extractHomepageFromNotes(%q) = %q, want %q", c.notes, got, c.want)
		}
	}
}

func TestResolveHomepageTarget(t *testing.T) {
	urlStr := "https://override.example"
	pendingStr := "pending"
	emptyStr := ""

	cases := []struct {
		name string
		rc   RepoConfig
		want string
	}{
		{"unset, no notes", RepoConfig{}, ""},
		{"unset, notes infer URL", RepoConfig{Notes: "live at foo.io"}, "https://foo.io"},
		{"explicit URL overrides", RepoConfig{Homepage: &urlStr, Notes: "live at foo.io"}, urlStr},
		{"explicit pending suppresses", RepoConfig{Homepage: &pendingStr, Notes: "live at foo.io"}, ""},
		{"explicit empty string suppresses", RepoConfig{Homepage: &emptyStr, Notes: "live at foo.io"}, ""},
	}
	for _, c := range cases {
		got := resolveHomepageTarget(c.rc)
		if got != c.want {
			t.Errorf("%s: resolveHomepageTarget = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCIAge(t *testing.T) {
	// Invalid timestamp returns -1, not a panic.
	if got := ciAge("not-a-timestamp"); got != -1 {
		t.Errorf("ciAge(invalid) = %d, want -1", got)
	}
	// Valid timestamp returns non-negative.
	if got := ciAge("2020-01-01T00:00:00Z"); got < 0 {
		t.Errorf("ciAge(2020) = %d, want >= 0", got)
	}
}

func TestTierOrder(t *testing.T) {
	// Ordering invariant: active > maintained > demo > scratch > archived.
	if tierOrder(TierActive) <= tierOrder(TierMaintained) {
		t.Error("active must outrank maintained")
	}
	if tierOrder(TierMaintained) <= tierOrder(TierDemo) {
		t.Error("maintained must outrank demo")
	}
	if tierOrder(TierDemo) <= tierOrder(TierScratch) {
		t.Error("demo must outrank scratch")
	}
	if tierOrder(TierScratch) <= tierOrder(TierArchived) {
		t.Error("scratch must outrank archived")
	}
}

func TestRequired(t *testing.T) {
	r := rule{requiredFor: ownAndRewritten, tierFloor: TierDemo}
	// Own + active: required
	if !required(r, RepoConfig{ForkRelation: FRelOwn, Tier: TierActive}) {
		t.Error("own+active should be required for floor=demo")
	}
	// Own + scratch: not required (below floor)
	if required(r, RepoConfig{ForkRelation: FRelOwn, Tier: TierScratch}) {
		t.Error("own+scratch should not be required for floor=demo")
	}
	// Tracking + active: not required (wrong relation)
	if required(r, RepoConfig{ForkRelation: FRelTracking, Tier: TierActive}) {
		t.Error("tracking+active should not be required for ownAndRewritten rule")
	}
	// No floor specified: applies regardless of tier
	noFloor := rule{requiredFor: ownAndRewritten}
	if !required(noFloor, RepoConfig{ForkRelation: FRelOwn, Tier: TierArchived}) {
		t.Error("no floor + own+archived should still be required")
	}
}

func TestSeverityGlyph(t *testing.T) {
	cases := []struct {
		s    Severity
		want string
	}{
		{SevOK, "✓"},
		{SevInfo, "·"},
		{SevWarn, "⚠"},
		{SevFail, "✗"},
		{SevSkip, "—"},
	}
	for _, c := range cases {
		if got := c.s.Glyph(); got != c.want {
			t.Errorf("Severity(%d).Glyph() = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestIsNonDeployURL(t *testing.T) {
	cases := []struct {
		u    string
		want bool
	}{
		{"https://github.com/foo/bar", true},
		{"https://gist.github.com/foo", true},
		{"https://gitlab.com/foo", true},
		{"https://npmjs.com/package/x", true},
		{"https://crates.io/crates/x", true},
		{"https://example.com", false},
		{"https://myapp.io", false},
		{"https://timeknife.fyi", false},
	}
	for _, c := range cases {
		got := isNonDeployURL(c.u)
		if got != c.want {
			t.Errorf("isNonDeployURL(%q) = %v, want %v", c.u, got, c.want)
		}
	}
}

// Sanity-check that the URL-extraction regex doesn't accept obvious garbage.
func TestNotesURLREDoesNotMatchEmpty(t *testing.T) {
	if m := notesURLRE.FindString(""); m != "" {
		t.Errorf("expected no match on empty string, got %q", m)
	}
	if m := notesURLRE.FindString("no url here"); m != "" {
		t.Errorf("expected no match on prose-only, got %q", m)
	}
	if m := notesURLRE.FindString("https://x.io and more text"); !strings.HasPrefix(m, "https://x.io") {
		t.Errorf("expected match starting with https://x.io, got %q", m)
	}
}
