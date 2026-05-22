package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type ForkRelation string

const (
	FRelOwn       ForkRelation = "own"
	FRelRewritten ForkRelation = "rewritten"
	FRelTracking  ForkRelation = "tracking"
	FRelSnapshot  ForkRelation = "snapshot"
	FRelProfile   ForkRelation = "profile"
)

type Tier string

const (
	TierActive     Tier = "active"
	TierMaintained Tier = "maintained"
	TierDemo       Tier = "demo"
	TierScratch    Tier = "scratch"
	TierArchived   Tier = "archived"
)

type RepoConfig struct {
	ForkRelation ForkRelation `yaml:"fork_relation,omitempty"`
	Tier         Tier         `yaml:"tier,omitempty"`
	Upstream     string       `yaml:"upstream,omitempty"`
	Notes        string       `yaml:"notes,omitempty"`
	// Homepage overrides notes-inferred homepage detection.
	// nil (unset)      → infer URL from notes regex
	// non-nil URL      → that's the expected homepage; check drift against it
	// non-nil non-URL  → suppress the check entirely (e.g. "pending" while
	//                    the deploy isn't live yet, or "skip" for incidental
	//                    URL mentions in notes that aren't real deploys)
	Homepage *string `yaml:"homepage,omitempty"`
	// Ignore drops the repo from audit/apply entirely. Use for NDA-private
	// or vendored repos where vidette has nothing useful to say.
	Ignore bool `yaml:"ignore,omitempty"`
}

type Defaults struct {
	ForkRelation   ForkRelation `yaml:"fork_relation"`
	Tier           Tier         `yaml:"tier"`
	IncludePrivate *bool        `yaml:"include_private,omitempty"`
	// CopyrightHolder is written into generated LICENSE files. Falls back to
	// `git config user.name`, then to the GitHub username.
	CopyrightHolder string `yaml:"copyright_holder,omitempty"`
	// SecurityEmail is written into generated SECURITY.md files. Falls back
	// to `git config user.email`.
	SecurityEmail string `yaml:"security_email,omitempty"`
	// EnforceAdmins controls whether branch protection applies to repo
	// admins too. Defaults to false (solo workflows want the ability to
	// recover from accidents without unprotecting first).
	EnforceAdmins *bool `yaml:"enforce_admins,omitempty"`
	// MergeStrategy is the auto-merge strategy used on vidette-opened PRs.
	// One of "squash" (default), "rebase", or "merge".
	MergeStrategy string `yaml:"merge_strategy,omitempty"`
}

type Config struct {
	User     string                `yaml:"user"`
	Defaults Defaults              `yaml:"defaults"`
	Repos    map[string]RepoConfig `yaml:"repos"`
}

func LoadConfig(path string) (*Config, error) {
	c, err := readConfigFile(path)
	if err != nil {
		return nil, err
	}

	// Layered loading: if a sibling `<basename>.local.<ext>` exists, merge it
	// on top. Repos defined in the local layer override base entries with the
	// same name; non-empty user/defaults in the local layer override base too.
	// The local layer is gitignored so private-repo entries stay off the
	// public yml. See .gitignore.
	if localPath := localOverlayPath(path); localPath != "" {
		if _, err := os.Stat(localPath); err == nil {
			local, err := readConfigFile(localPath)
			if err != nil {
				return nil, err
			}
			mergeConfig(c, local)
		}
	}

	if c.Defaults.ForkRelation == "" {
		c.Defaults.ForkRelation = FRelOwn
	}
	if c.Defaults.Tier == "" {
		c.Defaults.Tier = TierActive
	}
	if c.Defaults.IncludePrivate == nil {
		t := true
		c.Defaults.IncludePrivate = &t
	}
	if c.Defaults.CopyrightHolder == "" {
		if name := gitConfigGet("user.name"); name != "" {
			c.Defaults.CopyrightHolder = name
		} else {
			c.Defaults.CopyrightHolder = c.User
		}
	}
	if c.Defaults.SecurityEmail == "" {
		c.Defaults.SecurityEmail = gitConfigGet("user.email")
	}
	if c.Defaults.EnforceAdmins == nil {
		f := false
		c.Defaults.EnforceAdmins = &f
	}
	if c.Defaults.MergeStrategy == "" {
		c.Defaults.MergeStrategy = "squash"
	}
	switch c.Defaults.MergeStrategy {
	case "squash", "rebase", "merge":
	default:
		return nil, fmt.Errorf("invalid merge_strategy %q (want squash|rebase|merge)", c.Defaults.MergeStrategy)
	}
	return c, nil
}

// gitConfigGet returns `git config --get <key>` or "" if unset or git isn't
// available. Used to populate copyright/security defaults from the local git
// identity when vidette.yml doesn't specify them explicitly.
func gitConfigGet(key string) string {
	out, err := exec.Command("git", "config", "--get", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func readConfigFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c Config
	// KnownFields(true) makes typo'd keys (e.g. `include_privat: true`) a
	// hard error instead of a silent default. Catches a common footgun.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := validateConfig(&c, path); err != nil {
		return nil, err
	}
	return &c, nil
}

// validateConfig checks enum-typed fields in a freshly-parsed config and
// returns an error on unknown fork_relation or tier values. Typos here
// silently disable checks (the unknown enum doesn't match any rule's
// requiredFor map), so loud-failing at load time is much safer.
func validateConfig(c *Config, path string) error {
	validFR := map[ForkRelation]bool{FRelOwn: true, FRelRewritten: true, FRelTracking: true, FRelSnapshot: true, FRelProfile: true}
	validTier := map[Tier]bool{TierActive: true, TierMaintained: true, TierDemo: true, TierScratch: true, TierArchived: true}
	if c.Defaults.ForkRelation != "" && !validFR[c.Defaults.ForkRelation] {
		return fmt.Errorf("%s: defaults.fork_relation %q is not one of own|rewritten|tracking|snapshot|profile", path, c.Defaults.ForkRelation)
	}
	if c.Defaults.Tier != "" && !validTier[c.Defaults.Tier] {
		return fmt.Errorf("%s: defaults.tier %q is not one of active|maintained|demo|scratch|archived", path, c.Defaults.Tier)
	}
	for name, rc := range c.Repos {
		if rc.ForkRelation != "" && !validFR[rc.ForkRelation] {
			return fmt.Errorf("%s: repos.%s.fork_relation %q is not one of own|rewritten|tracking|snapshot|profile", path, name, rc.ForkRelation)
		}
		if rc.Tier != "" && !validTier[rc.Tier] {
			return fmt.Errorf("%s: repos.%s.tier %q is not one of active|maintained|demo|scratch|archived", path, name, rc.Tier)
		}
	}
	return nil
}

func localOverlayPath(path string) string {
	ext := filepath.Ext(path)
	if ext == "" {
		return ""
	}
	return strings.TrimSuffix(path, ext) + ".local" + ext
}

func mergeConfig(base, overlay *Config) {
	if overlay.User != "" {
		base.User = overlay.User
	}
	if overlay.Defaults.ForkRelation != "" {
		base.Defaults.ForkRelation = overlay.Defaults.ForkRelation
	}
	if overlay.Defaults.Tier != "" {
		base.Defaults.Tier = overlay.Defaults.Tier
	}
	if overlay.Defaults.IncludePrivate != nil {
		base.Defaults.IncludePrivate = overlay.Defaults.IncludePrivate
	}
	if base.Repos == nil {
		base.Repos = map[string]RepoConfig{}
	}
	for k, v := range overlay.Repos {
		base.Repos[k] = v
	}
}

func (c *Config) Resolve(name string) RepoConfig {
	rc := c.Repos[name]
	if rc.ForkRelation == "" {
		rc.ForkRelation = c.Defaults.ForkRelation
	}
	if rc.Tier == "" {
		rc.Tier = c.Defaults.Tier
	}
	return rc
}
