package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLocalOverlayPath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"vidette.yml", "vidette.local.yml"},
		{"path/to/foo.yml", "path/to/foo.local.yml"},
		{"foo.yaml", "foo.local.yaml"},
		{"noext", ""},
	}
	for _, c := range cases {
		got := localOverlayPath(c.in)
		if got != c.want {
			t.Errorf("localOverlayPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLoadConfigAppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vidette.yml")
	yml := `
user: alice
repos:
  foo: { tier: maintained }
`
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.ForkRelation != FRelOwn {
		t.Errorf("default fork_relation = %q, want own", cfg.Defaults.ForkRelation)
	}
	if cfg.Defaults.Tier != TierActive {
		t.Errorf("default tier = %q, want active", cfg.Defaults.Tier)
	}
	if cfg.Defaults.MergeStrategy != "squash" {
		t.Errorf("default merge_strategy = %q, want squash", cfg.Defaults.MergeStrategy)
	}
	if cfg.Defaults.EnforceAdmins == nil || *cfg.Defaults.EnforceAdmins {
		t.Errorf("default enforce_admins = %v, want false", cfg.Defaults.EnforceAdmins)
	}
	// CopyrightHolder defaults to git config user.name or the GitHub user.
	if cfg.Defaults.CopyrightHolder == "" {
		t.Error("default copyright_holder should never be empty (falls back to user)")
	}
}

func TestLoadConfigRejectsBadMergeStrategy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vidette.yml")
	yml := `
user: alice
defaults:
  merge_strategy: bogus
`
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Error("expected error on invalid merge_strategy, got nil")
	}
}

func TestLoadConfigOverlayMerges(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "vidette.yml")
	overlay := filepath.Join(dir, "vidette.local.yml")
	if err := os.WriteFile(base, []byte("user: alice\nrepos:\n  foo: { tier: maintained }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overlay, []byte("repos:\n  bar: { tier: scratch }\n  foo: { tier: active }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(base)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Repos["bar"].Tier != TierScratch {
		t.Error("overlay should add new entries")
	}
	if cfg.Repos["foo"].Tier != TierActive {
		t.Error("overlay entry should override base entry with same name")
	}
}

func TestLoadConfigRejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vidette.yml")
	// Typo: include_privat instead of include_private
	yml := "user: alice\ndefaults:\n  include_privat: true\n"
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Error("expected error on typo'd field, got nil")
	}
}

func TestLoadConfigRejectsBadForkRelation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vidette.yml")
	yml := "user: alice\nrepos:\n  foo: { fork_relation: forked, tier: active }\n"
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Error("expected error on invalid fork_relation, got nil")
	}
}

func TestLoadConfigRejectsBadTier(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "vidette.yml")
	yml := "user: alice\nrepos:\n  foo: { tier: live }\n"
	if err := os.WriteFile(path, []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Error("expected error on invalid tier, got nil")
	}
}

func TestResolveAppliesDefaults(t *testing.T) {
	cfg := &Config{
		Defaults: Defaults{ForkRelation: FRelOwn, Tier: TierActive},
		Repos: map[string]RepoConfig{
			"specified": {Tier: TierMaintained},
		},
	}
	// Unspecified repo: returns defaults
	rc := cfg.Resolve("anything")
	if rc.ForkRelation != FRelOwn || rc.Tier != TierActive {
		t.Errorf("Resolve(unspecified) = %+v, want defaults", rc)
	}
	// Specified repo: inherits unset fields from defaults
	rc = cfg.Resolve("specified")
	if rc.ForkRelation != FRelOwn {
		t.Errorf("specified fork_relation should inherit default, got %q", rc.ForkRelation)
	}
	if rc.Tier != TierMaintained {
		t.Errorf("specified tier should be maintained, got %q", rc.Tier)
	}
}
