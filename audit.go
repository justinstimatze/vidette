package main

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

type Severity int

const (
	SevOK Severity = iota
	SevInfo
	SevWarn
	SevFail
	SevSkip
)

func (s Severity) Glyph() string {
	switch s {
	case SevOK:
		return "✓"
	case SevInfo:
		return "·"
	case SevWarn:
		return "⚠"
	case SevFail:
		return "✗"
	case SevSkip:
		return "—"
	}
	return "?"
}

type Check struct {
	Name     string
	Severity Severity
	Detail   string
}

type RepoAudit struct {
	Repo           string
	ForkRelation   ForkRelation
	Tier           Tier
	Notes          string
	HomepageTarget string // resolved target URL or "" if no target / suppressed
	IsFork         bool   // GitHub's fork bit — drives the config-independent fork backstop in buildFix
	Checks         []Check
	FetchErr       error
}

// resolveHomepageTarget returns the expected homepage URL for a repo, or ""
// if the check should be silent. Explicit RepoConfig.Homepage takes precedence
// over notes-regex inference, and an explicit non-URL value (e.g. "pending")
// is the suppression signal.
func resolveHomepageTarget(rc RepoConfig) string {
	if rc.Homepage != nil {
		if strings.HasPrefix(*rc.Homepage, "http") {
			return *rc.Homepage
		}
		return ""
	}
	return extractHomepageFromNotes(rc.Notes)
}

// rule: which fork_relations require this check
type rule struct {
	requiredFor map[ForkRelation]bool
	// optional tier-floor; check is required only when tier is at-or-above this
	tierFloor Tier
}

var (
	ownAndRewritten = map[ForkRelation]bool{FRelOwn: true, FRelRewritten: true}
	allExceptSnap   = map[ForkRelation]bool{FRelOwn: true, FRelRewritten: true, FRelTracking: true}
	trackingOnly    = map[ForkRelation]bool{FRelTracking: true}
)

func tierOrder(t Tier) int {
	switch t {
	case TierActive:
		return 4
	case TierMaintained:
		return 3
	case TierDemo:
		return 2
	case TierScratch:
		return 1
	case TierArchived:
		return 0
	}
	return 0
}

func required(r rule, rc RepoConfig) bool {
	if !r.requiredFor[rc.ForkRelation] {
		return false
	}
	if r.tierFloor != "" && tierOrder(rc.Tier) < tierOrder(r.tierFloor) {
		return false
	}
	return true
}

func AuditRepo(repo string, rc RepoConfig) RepoAudit {
	a := RepoAudit{
		Repo:           repo,
		ForkRelation:   rc.ForkRelation,
		Tier:           rc.Tier,
		Notes:          rc.Notes,
		HomepageTarget: resolveHomepageTarget(rc),
	}

	meta, err := FetchRepoMeta(repo)
	if err != nil {
		a.FetchErr = err
		return a
	}
	a.IsFork = meta.IsFork

	// Profile readme repos (user/user) are special — no license, no PR flow,
	// no CI; just confirm the default branch and move on.
	if rc.ForkRelation == FRelProfile {
		a.Checks = append(a.Checks, Check{"profile-repo", SevInfo, "profile readme — standard checks skipped"})
		if meta.DefaultBranch == "main" {
			a.Checks = append(a.Checks, Check{"default-branch", SevOK, "main"})
		} else {
			a.Checks = append(a.Checks, Check{"default-branch", SevWarn, meta.DefaultBranch})
		}
		return a
	}

	// License — NOASSERTION means a LICENSE file exists but GitHub can't
	// auto-classify it (custom license, multi-license, etc.). Don't autofix
	// that — the user picked it deliberately. Floor at demo: scratch is
	// throwaway and archived is frozen, neither needs a fresh license write.
	if required(rule{requiredFor: ownAndRewritten, tierFloor: TierDemo}, rc) {
		switch {
		case meta.License != nil && meta.License.SPDXID != "" && meta.License.SPDXID != "NOASSERTION":
			a.Checks = append(a.Checks, Check{"license", SevOK, meta.License.SPDXID})
		case meta.License != nil && meta.License.SPDXID == "NOASSERTION":
			a.Checks = append(a.Checks, Check{"license", SevInfo, "non-standard (LICENSE present but not auto-classified)"})
		default:
			a.Checks = append(a.Checks, Check{"license", SevFail, "missing"})
		}
	} else if !ownAndRewritten[rc.ForkRelation] {
		a.Checks = append(a.Checks, Check{"license", SevSkip, "inherited from upstream"})
	}

	// Default branch is main. Floored at demo because archived repos are
	// read-only (can't be renamed without unarchive→rename→re-archive dance)
	// and scratch repos aren't worth the noise.
	if required(rule{requiredFor: ownAndRewritten, tierFloor: TierDemo}, rc) {
		if meta.DefaultBranch == "main" {
			a.Checks = append(a.Checks, Check{"default-branch", SevOK, "main"})
		} else {
			a.Checks = append(a.Checks, Check{"default-branch", SevFail, meta.DefaultBranch})
		}
	} else if !ownAndRewritten[rc.ForkRelation] {
		a.Checks = append(a.Checks, Check{"default-branch", SevSkip, meta.DefaultBranch + " (inherited)"})
	}

	// Description — discoverability check, skip for scratch/archived where
	// content is throwaway anyway.
	if required(rule{requiredFor: ownAndRewritten, tierFloor: TierDemo}, rc) {
		if meta.Description != "" {
			a.Checks = append(a.Checks, Check{"description", SevOK, ""})
		} else {
			a.Checks = append(a.Checks, Check{"description", SevWarn, "empty"})
		}
	}

	// Topics — same floor as description.
	if required(rule{requiredFor: ownAndRewritten, tierFloor: TierDemo}, rc) {
		if len(meta.Topics) > 0 {
			a.Checks = append(a.Checks, Check{"topics", SevOK, fmt.Sprintf("%d", len(meta.Topics))})
		} else {
			a.Checks = append(a.Checks, Check{"topics", SevWarn, "none set"})
		}
	}

	// Dependabot — velocity check, only nag active repos that have something
	// for dependabot to manage (package ecosystem or workflows).
	if required(rule{requiredFor: ownAndRewritten, tierFloor: TierActive}, rc) {
		manageable := repoHasDependabotSurface(repo)
		has, err := HasFile(repo, ".github/dependabot.yml")
		switch {
		case err != nil:
			a.Checks = append(a.Checks, Check{"dependabot", SevWarn, "probe error: " + err.Error()})
		case has:
			a.Checks = append(a.Checks, Check{"dependabot", SevOK, ""})
		case !manageable:
			a.Checks = append(a.Checks, Check{"dependabot", SevSkip, "no package manifests or workflows"})
		default:
			a.Checks = append(a.Checks, Check{"dependabot", SevFail, "no .github/dependabot.yml"})
		}
	}

	// Dependabot alerts — settings-level toggle, independent of the config
	// file. A repo can have .github/dependabot.yml present and still have
	// alerts disabled at the repo-settings level (and vice-versa). Same
	// surface gate as the dependabot check: only nag active repos with a
	// package ecosystem or workflows to actually be alerted about.
	if required(rule{requiredFor: ownAndRewritten, tierFloor: TierActive}, rc) {
		if !repoHasDependabotSurface(repo) {
			a.Checks = append(a.Checks, Check{"alerts", SevSkip, "no package manifests or workflows"})
		} else {
			enabled, err := DependabotAlertsEnabled(repo)
			switch {
			case err != nil:
				a.Checks = append(a.Checks, Check{"alerts", SevWarn, "probe error: " + err.Error()})
			case enabled:
				a.Checks = append(a.Checks, Check{"alerts", SevOK, ""})
			default:
				a.Checks = append(a.Checks, Check{"alerts", SevFail, "Dependabot vulnerability alerts disabled"})
			}
		}
	}

	// Auto-merge — velocity check, only nag active repos
	if required(rule{requiredFor: ownAndRewritten, tierFloor: TierActive}, rc) {
		if meta.AllowAutoMerge {
			a.Checks = append(a.Checks, Check{"auto-merge", SevOK, ""})
		} else {
			a.Checks = append(a.Checks, Check{"auto-merge", SevFail, "Allow auto-merge is off"})
		}
	}

	// Branch protection
	if required(rule{requiredFor: ownAndRewritten, tierFloor: TierMaintained}, rc) {
		has, err := HasBranchProtection(repo, meta.DefaultBranch)
		if err != nil {
			a.Checks = append(a.Checks, Check{"branch-protection", SevWarn, "probe error: " + err.Error()})
		} else if has {
			a.Checks = append(a.Checks, Check{"branch-protection", SevOK, ""})
		} else {
			a.Checks = append(a.Checks, Check{"branch-protection", SevFail, "default branch unprotected"})
		}
	}

	// Security policy — safety check (not velocity), applies to maintained+.
	if required(rule{requiredFor: ownAndRewritten, tierFloor: TierMaintained}, rc) {
		has, err := HasFile(repo, "SECURITY.md")
		if err != nil {
			a.Checks = append(a.Checks, Check{"security-policy", SevWarn, "probe error: " + err.Error()})
		} else if has {
			a.Checks = append(a.Checks, Check{"security-policy", SevOK, ""})
		} else {
			a.Checks = append(a.Checks, Check{"security-policy", SevFail, "no SECURITY.md"})
		}
	}

	// Archive-state — flags drift between the user's declared intent and
	// GitHub's actual state in both directions.
	switch {
	case rc.Tier == TierArchived && meta.Archived:
		a.Checks = append(a.Checks, Check{"archive-state", SevOK, "archived on GitHub"})
	case rc.Tier == TierArchived && !meta.Archived:
		a.Checks = append(a.Checks, Check{"archive-state", SevFail, "tier=archived but repo is live on GitHub"})
	case rc.Tier != TierArchived && meta.Archived:
		a.Checks = append(a.Checks, Check{"archive-state", SevFail, fmt.Sprintf("repo is archived on GitHub but tier=%s in vidette.yml", rc.Tier)})
	}

	// Homepage — fires only when there's an expected target: either from
	// an explicit RepoConfig.Homepage URL, or inferred from notes mentioning
	// a URL/recognized-TLD domain. Setting RepoConfig.Homepage to a
	// non-URL sentinel (e.g. "pending") suppresses the check — useful when
	// the deploy isn't live yet but the repo's intent is recorded. Floored
	// at demo because scratch/archived don't need discoverability work.
	if required(rule{requiredFor: ownAndRewritten, tierFloor: TierDemo}, rc) {
		if target := a.HomepageTarget; target != "" {
			switch {
			case meta.Homepage == "":
				a.Checks = append(a.Checks, Check{"homepage", SevFail, fmt.Sprintf("expected %s; homepage is empty", target)})
			case !strings.EqualFold(strings.TrimRight(meta.Homepage, "/"), strings.TrimRight(target, "/")):
				a.Checks = append(a.Checks, Check{"homepage", SevWarn, fmt.Sprintf("expected %s; homepage is %s (left for review)", target, meta.Homepage)})
			default:
				a.Checks = append(a.Checks, Check{"homepage", SevOK, meta.Homepage})
			}
		}
	}

	// CI health — skip for scratch/archived. Broken CI on a frozen or
	// throwaway repo is not actionable signal.
	if required(rule{requiredFor: allExceptSnap, tierFloor: TierDemo}, rc) {
		run, streakStart, capped, err := LatestCIRun(repo, meta.DefaultBranch)
		if err != nil {
			a.Checks = append(a.Checks, Check{"ci", SevWarn, "probe error: " + err.Error()})
		} else if run == nil {
			a.Checks = append(a.Checks, Check{"ci", SevInfo, "no runs"})
		} else {
			age := ciAge(run.CreatedAt)
			// streak is how long the current failure has persisted (oldest
			// consecutive failure of the same workflow); "≥" when the last
			// success predates the scan window.
			streak := ciAge(streakStart)
			since := func() string {
				if capped {
					return fmt.Sprintf("≥%dd", streak)
				}
				return fmt.Sprintf("%dd", streak)
			}
			switch run.Conclusion {
			case "success":
				a.Checks = append(a.Checks, Check{"ci", SevOK, fmt.Sprintf("green, %dd ago", age)})
			case "failure":
				a.Checks = append(a.Checks, Check{"ci", SevFail, fmt.Sprintf("failing %s (%s)", since(), run.Name)})
			case "action_required":
				a.Checks = append(a.Checks, Check{"ci", SevFail, fmt.Sprintf("action_required %s (%s)", since(), run.Name)})
			case "":
				a.Checks = append(a.Checks, Check{"ci", SevInfo, fmt.Sprintf("in progress (%s)", run.Status)})
			default:
				a.Checks = append(a.Checks, Check{"ci", SevWarn, fmt.Sprintf("%s, %dd ago", run.Conclusion, age)})
			}
		}
	}

	// Upstream sync lag
	if required(rule{requiredFor: trackingOnly}, rc) {
		behind, err := CompareUpstream(meta)
		if err != nil {
			a.Checks = append(a.Checks, Check{"upstream-sync", SevWarn, "probe error: " + err.Error()})
		} else if meta.Parent == nil {
			a.Checks = append(a.Checks, Check{"upstream-sync", SevWarn, "marked tracking but no parent found"})
		} else if behind == 0 {
			a.Checks = append(a.Checks, Check{"upstream-sync", SevOK, "up to date with " + meta.Parent.FullName})
		} else if behind <= 10 {
			a.Checks = append(a.Checks, Check{"upstream-sync", SevWarn, fmt.Sprintf("%d commits behind %s", behind, meta.Parent.FullName)})
		} else {
			a.Checks = append(a.Checks, Check{"upstream-sync", SevFail, fmt.Sprintf("%d commits behind %s", behind, meta.Parent.FullName)})
		}
	}

	return a
}

var (
	notesURLRE = regexp.MustCompile(`https?://[^\s,;)]+`)
	// notesDomainRE matches a domain ending in one of an allowlisted set of
	// TLDs commonly used for indie/personal deploys. Conservative on
	// purpose — a false positive would PATCH a homepage to garbage. Add
	// TLDs here when you encounter a legitimate deploy that doesn't match.
	notesDomainRE = regexp.MustCompile(`\b[a-z0-9][a-z0-9-]*(?:\.[a-z0-9][a-z0-9-]*)*\.(?:fyi|com|org|net|io|dev|ai|app|so|xyz|co|me|sh|page|site|tech|systems|zone|tools|cloud|blog|run|build|works|wtf)\b`)
)

// trailing punctuation that frequently abuts a URL in prose; stripped from
// extracted URL matches before normalization.
const urlTrailingPunct = ".,;:)!?\"'"

// nonDeployHosts are domains that frequently appear in notes (often as code
// hosts or registries) but aren't homepages anyone wants vidette to PATCH.
// Match is by substring after lowercasing, so subdomains like
// "gist.github.com" are caught by "github.com".
var nonDeployHosts = []string{
	"github.com",
	"gitlab.com",
	"bitbucket.org",
	"codeberg.org",
	"sr.ht",
	"npmjs.com",
	"pypi.org",
	"crates.io",
	"hub.docker.com",
}

func isNonDeployURL(u string) bool {
	s := strings.ToLower(u)
	for _, host := range nonDeployHosts {
		if strings.Contains(s, host) {
			return true
		}
	}
	return false
}

// extractHomepageFromNotes returns a normalized https URL inferred from a
// vidette.yml notes field, or "" if no URL/domain pattern is present.
// Conservative on purpose — a false positive here would PATCH a repo's
// homepage to garbage. Recognized-TLD allowlist keeps "github.com" and
// similar incidental domains from firing as deploy targets.
func extractHomepageFromNotes(notes string) string {
	if notes == "" {
		return ""
	}
	if m := notesURLRE.FindString(notes); m != "" {
		candidate := strings.TrimRight(m, urlTrailingPunct)
		if !isNonDeployURL(candidate) {
			return candidate
		}
	}
	if m := notesDomainRE.FindString(strings.ToLower(notes)); m != "" {
		candidate := "https://" + m
		if !isNonDeployURL(candidate) {
			return candidate
		}
	}
	return ""
}

func ciAge(createdAt string) int {
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return -1
	}
	return int(time.Since(t).Hours() / 24)
}
