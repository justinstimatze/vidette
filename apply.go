package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// maxTopics is the cap on topics PUT to a repo. GitHub itself allows
	// 20; we cap lower because beyond ~8 topics the discoverability signal
	// degrades to noise.
	maxTopics = 8
	// maxDescriptionLen is the soft cap on description length before
	// vidette truncates with an ellipsis. GitHub's hard cap is 350.
	maxDescriptionLen = 350
	// prListLimit is how many open PRs ghAPI fetches when checking for an
	// existing vidette PR. 100 covers any plausible fleet's open PR set.
	prListLimit = 100
)

// FleetStats holds aggregate facts derived from audits — used to infer
// suggestions like a modal license for repos that lack one.
type FleetStats struct {
	ModalLicense string // SPDX id; empty if no clear winner
}

func ComputeFleetStats(audits []RepoAudit) FleetStats {
	counts := map[string]int{}
	for _, a := range audits {
		for _, c := range a.Checks {
			if c.Name == "license" && c.Severity == SevOK {
				counts[c.Detail]++
			}
		}
	}
	best := ""
	bestN := 0
	for k, v := range counts {
		if v > bestN {
			best = k
			bestN = v
		}
	}
	return FleetStats{ModalLicense: best}
}

type FixAction struct {
	Repo   string
	Check  string
	Plan   string // human-readable description
	Apply  func() error
	NoFix  bool // set when the check is unfixable (license, description, etc.)
	Reason string
}

// PlanFixes walks the audits and returns one FixAction per fail that has a known recipe.
// Audits with unfixable fails get a FixAction with NoFix=true so they're surfaced in the plan.
func PlanFixes(audits []RepoAudit, stats FleetStats, defaults Defaults) []FixAction {
	var actions []FixAction
	for _, a := range audits {
		if a.FetchErr != nil {
			continue
		}
		for _, c := range a.Checks {
			if c.Severity != SevFail && c.Severity != SevWarn {
				continue
			}
			// Probe errors are transient infra noise, not fixable findings.
			if strings.HasPrefix(c.Detail, "probe error") {
				continue
			}
			act := buildFix(a, c, stats, defaults)
			actions = append(actions, act)
		}
	}
	sort.SliceStable(actions, func(i, j int) bool {
		if actions[i].NoFix != actions[j].NoFix {
			return !actions[i].NoFix // fixable first
		}
		if actions[i].Repo != actions[j].Repo {
			return actions[i].Repo < actions[j].Repo
		}
		if oi, oj := checkOrder(actions[i].Check), checkOrder(actions[j].Check); oi != oj {
			return oi < oj
		}
		return actions[i].Check < actions[j].Check
	})
	return actions
}

// checkOrder ranks checks within a repo so content writes complete before
// state mutations that lock the repo. `archive-state` runs last because
// `gh repo archive` makes the repo read-only — any subsequent license,
// SECURITY.md, or topic write would fail.
func checkOrder(name string) int {
	switch name {
	case "archive-state":
		return 100
	default:
		return 50
	}
}

func buildFix(a RepoAudit, c Check, stats FleetStats, defaults Defaults) FixAction {
	switch c.Name {
	case "license":
		if stats.ModalLicense == "" {
			return FixAction{Repo: a.Repo, Check: c.Name, NoFix: true, Reason: "no modal license inferable from fleet"}
		}
		spdx := stats.ModalLicense
		holder := defaults.CopyrightHolder
		return FixAction{
			Repo:  a.Repo,
			Check: c.Name,
			Plan:  fmt.Sprintf("write LICENSE: %s (inferred from fleet modal, holder=%s)", spdx, holder),
			Apply: func() error { return fixLicense(a.Repo, spdx, holder, defaults.MergeStrategy) },
		}
	case "description":
		return FixAction{
			Repo:  a.Repo,
			Check: c.Name,
			Plan:  "PATCH description = first line of README (inferred)",
			Apply: func() error { return fixDescription(a.Repo) },
		}
	case "topics":
		return FixAction{
			Repo:  a.Repo,
			Check: c.Name,
			Plan:  "PUT topics = lang stack + README backtick-quoted terms (inferred, cap 8)",
			Apply: func() error { return fixTopics(a.Repo) },
		}
	case "auto-merge":
		return FixAction{
			Repo:  a.Repo,
			Check: c.Name,
			Plan:  "PATCH repos/" + a.Repo + " allow_auto_merge=true",
			Apply: func() error { return fixAutoMerge(a.Repo) },
		}
	case "dependabot":
		return FixAction{
			Repo:  a.Repo,
			Check: c.Name,
			Plan:  "write .github/dependabot.yml (detect ecosystem from repo languages)",
			Apply: func() error { return fixDependabot(a.Repo, defaults.MergeStrategy) },
		}
	case "branch-protection":
		enforceAdmins := false
		if defaults.EnforceAdmins != nil {
			enforceAdmins = *defaults.EnforceAdmins
		}
		return FixAction{
			Repo:  a.Repo,
			Check: c.Name,
			Plan:  fmt.Sprintf("PUT branch protection (block force-push and deletion only; enforce_admins=%v)", enforceAdmins),
			Apply: func() error { return fixBranchProtection(a.Repo, enforceAdmins) },
		}
	case "upstream-sync":
		return FixAction{
			Repo:  a.Repo,
			Check: c.Name,
			Plan:  "gh repo sync " + a.Repo + " (fails if local divergence exists)",
			Apply: func() error { return fixUpstreamSync(a.Repo) },
		}
	case "security-policy":
		email := defaults.SecurityEmail
		if email == "" {
			return FixAction{Repo: a.Repo, Check: c.Name, NoFix: true, Reason: "no security_email configured (set defaults.security_email or git config user.email)"}
		}
		return FixAction{
			Repo:  a.Repo,
			Check: c.Name,
			Plan:  fmt.Sprintf("write SECURITY.md (contact: %s, 90-day disclosure window)", email),
			Apply: func() error { return fixSecurityPolicy(a.Repo, email, defaults.MergeStrategy) },
		}
	case "archive-state":
		return FixAction{
			Repo:  a.Repo,
			Check: c.Name,
			Plan:  "gh repo archive " + a.Repo + " (matches vidette.yml tier=archived)",
			Apply: func() error { return fixArchiveState(a.Repo) },
		}
	case "homepage":
		target := a.HomepageTarget
		if target == "" {
			return FixAction{Repo: a.Repo, Check: c.Name, NoFix: true, Reason: "no homepage target resolved"}
		}
		return FixAction{
			Repo:  a.Repo,
			Check: c.Name,
			Plan:  "PATCH homepage = " + target,
			Apply: func() error { return fixHomepage(a.Repo, target) },
		}
	default:
		return FixAction{
			Repo:   a.Repo,
			Check:  c.Name,
			NoFix:  true,
			Reason: "needs human judgment: " + c.Detail,
		}
	}
}

// fixLicense fetches the SPDX template from github's /licenses/{key} endpoint,
// substitutes year and copyright holder, and writes LICENSE via the contents API.
func fixLicense(repo, spdx, holder, mergeStrategy string) error {
	key := strings.ToLower(spdx)
	body, ok, err := ghAPI("licenses/" + key)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("license template %s not found", spdx)
	}
	var tmpl struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(body, &tmpl); err != nil {
		return fmt.Errorf("parse license template: %w", err)
	}
	year := fmt.Sprintf("%d", time.Now().Year())
	text := strings.ReplaceAll(tmpl.Body, "[year]", year)
	text = strings.ReplaceAll(text, "[fullname]", holder)
	text = strings.ReplaceAll(text, "<year>", year)
	text = strings.ReplaceAll(text, "<name of author>", holder)

	return writeFile(repo, "LICENSE", text,
		"add LICENSE (via vidette)",
		"Add LICENSE ("+spdx+")",
		"Adds a "+spdx+" license, inferred from fleet-modal usage. Generated by vidette.",
		mergeStrategy)
}

// fixDescription pulls the repo's README, extracts the first line of prose
// (skipping headings/badges/blank lines), and PATCHes the repo description.
func fixDescription(repo string) error {
	body, ok, err := ghAPI("repos/" + repo + "/readme")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no README to infer description from")
	}
	var rdme struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(body, &rdme); err != nil {
		return fmt.Errorf("parse readme: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(rdme.Content, "\n", ""))
	if err != nil {
		return fmt.Errorf("decode readme: %w", err)
	}
	desc := firstProseLine(string(raw))
	if desc == "" {
		return fmt.Errorf("could not extract prose line from README")
	}
	if len(desc) > maxDescriptionLen {
		desc = desc[:maxDescriptionLen-3] + "..."
	}
	cmd := exec.Command("gh", "api", "-X", "PATCH", "repos/"+repo, "-f", "description="+desc)
	return runWithStderr(cmd)
}

// firstProseLine returns the first non-empty line of markdown that isn't a
// heading, badge link, HTML tag, code fence, or content inside a code fence.
// Returns "" if nothing usable.
func firstProseLine(md string) string {
	inFence := false
	for _, line := range strings.Split(md, "\n") {
		l := strings.TrimSpace(line)
		if strings.HasPrefix(l, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "#") {
			continue
		}
		if strings.HasPrefix(l, "<") || strings.HasPrefix(l, "[!") || strings.HasPrefix(l, "![") {
			continue
		}
		if strings.HasPrefix(l, "---") {
			continue
		}
		return l
	}
	return ""
}

// fixTopics seeds the repo's topics from its language stack (top languages by
// byte count) plus frequently-mentioned backtick-quoted identifiers in the
// README. Caps at 8 topics, all lowercase, GitHub-valid.
//
// House cascade for inferred fixers: try the most authoritative signal first,
// fall back to coarser signals as needed, and only return a descriptive error
// if every tier comes up empty. Here that's languages (API truth) → README
// (author-curated via backticks/headings) → repo description (free-form
// fallback). Apply the same pattern when adding new inferred fixers: never
// silently no-op, and never lean on a single thin source.
func fixTopics(repo string) error {
	seen := map[string]bool{}
	var topics []string
	add := func(t string) bool {
		if !isValidTopic(t) || seen[t] {
			return false
		}
		seen[t] = true
		topics = append(topics, t)
		return len(topics) < maxTopics
	}

	// Language stack from the API — highly reliable signal.
	if langBody, ok, _ := ghAPI("repos/" + repo + "/languages"); ok {
		var langs map[string]int
		if err := json.Unmarshal(langBody, &langs); err == nil {
			type kv struct {
				k string
				v int
			}
			pairs := make([]kv, 0, len(langs))
			for k, v := range langs {
				pairs = append(pairs, kv{k, v})
			}
			sort.Slice(pairs, func(i, j int) bool { return pairs[i].v > pairs[j].v })
			for _, p := range pairs {
				if !add(strings.ToLower(p.k)) {
					break
				}
			}
		}
	}

	// README-mined identifiers: backtick-quoted alpha tokens + heading words.
	if rdmeBody, ok, _ := ghAPI("repos/" + repo + "/readme"); ok {
		var rdme struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(rdmeBody, &rdme); err == nil {
			raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(rdme.Content, "\n", ""))
			if err == nil {
				for _, t := range mineReadmeTopics(string(raw)) {
					if !add(t) {
						break
					}
				}
			}
		}
	}

	// Description fallback — when other sources are thin, mine the repo's
	// own description for topic-y words.
	if meta, err := FetchRepoMeta(repo); err == nil && meta.Description != "" {
		for _, t := range mineDescriptionTopics(meta.Description) {
			if !add(t) {
				break
			}
		}
	}

	if len(topics) == 0 {
		return fmt.Errorf("no topic candidates inferable from languages or README")
	}

	body, err := json.Marshal(map[string]any{"names": topics})
	if err != nil {
		return err
	}
	cmd := exec.Command("gh", "api", "-X", "PUT", "repos/"+repo+"/topics", "--input", "-")
	cmd.Stdin = bytes.NewReader(body)
	return runWithStderr(cmd)
}

var topicStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "this": true, "that": true, "with": true,
	"from": true, "into": true, "main": true, "fmt": true, "err": true, "var": true,
	"func": true, "true": true, "false": true, "nil": true, "return": true,
	"import": true, "package": true, "src": true, "lib": true, "bin": true,
	"out": true, "tmp": true, "config": true, "default": true, "string": true,
	"int": true, "bool": true, "use": true, "via": true, "see": true, "your": true,
	"you": true, "are": true, "not": true, "but": true, "all": true, "any": true,
	"which": true, "set": true, "get": true, "run": true, "license": true,
	"readme": true, "todo": true, "fixme": true, "note": true, "warning": true,
}

func isValidTopic(s string) bool {
	if len(s) < 2 || len(s) > 50 {
		return false
	}
	for i, r := range s {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if i == 0 && !isAlnum {
			return false
		}
		if !isAlnum && r != '-' {
			return false
		}
	}
	return !topicStopwords[s]
}

var (
	backtickIdentRE = regexp.MustCompile("`([a-zA-Z][a-zA-Z0-9-]{1,40})`")
	headingRE       = regexp.MustCompile(`(?m)^#{1,4}\s+(.+)$`)
	alphaWordRE     = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9-]{1,30}`)
)

// mineReadmeTopics extracts theme words from a README: backtick-quoted
// identifiers (the author already curated these by code-formatting) plus
// words from h1–h4 headings (the author also curated these by promoting them
// to section titles).
func mineReadmeTopics(md string) []string {
	counts := map[string]int{}

	for _, m := range backtickIdentRE.FindAllStringSubmatch(md, -1) {
		t := strings.ToLower(m[1])
		if isValidTopic(t) {
			counts[t] += 2 // double-weight code-formatted words
		}
	}

	for _, m := range headingRE.FindAllStringSubmatch(md, -1) {
		for _, w := range alphaWordRE.FindAllString(m[1], -1) {
			t := strings.ToLower(w)
			if isValidTopic(t) {
				counts[t]++
			}
		}
	}

	return rankedTopics(counts)
}

// mineDescriptionTopics extracts theme words from a free-form repo description.
// Lower-confidence than README mining — only used as a fallback source.
func mineDescriptionTopics(desc string) []string {
	counts := map[string]int{}
	for _, w := range alphaWordRE.FindAllString(desc, -1) {
		t := strings.ToLower(w)
		if isValidTopic(t) {
			counts[t]++
		}
	}
	return rankedTopics(counts)
}

func rankedTopics(counts map[string]int) []string {
	type kv struct {
		k string
		v int
	}
	pairs := make([]kv, 0, len(counts))
	for k, v := range counts {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].v != pairs[j].v {
			return pairs[i].v > pairs[j].v
		}
		return pairs[i].k < pairs[j].k
	})
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.k)
	}
	return out
}

func fixAutoMerge(repo string) error {
	cmd := exec.Command("gh", "api", "-X", "PATCH", "repos/"+repo, "-F", "allow_auto_merge=true")
	return runWithStderr(cmd)
}

// fixBranchProtection applies minimal protection: block force-push and deletion,
// nothing else. enforceAdmins controls whether admins are bound by the rules
// too — defaults to false for solo workflows that need recovery paths.
func fixBranchProtection(repo string, enforceAdmins bool) error {
	meta, err := FetchRepoMeta(repo)
	if err != nil {
		return err
	}
	branch := meta.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	body := map[string]any{
		"required_status_checks":           nil,
		"enforce_admins":                   enforceAdmins,
		"required_pull_request_reviews":    nil,
		"restrictions":                     nil,
		"allow_force_pushes":               false,
		"allow_deletions":                  false,
		"required_linear_history":          false,
		"required_conversation_resolution": false,
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	cmd := exec.Command("gh", "api", "-X", "PUT", "repos/"+repo+"/branches/"+branch+"/protection", "--input", "-")
	cmd.Stdin = bytes.NewReader(buf)
	return runWithStderr(cmd)
}

func fixUpstreamSync(repo string) error {
	cmd := exec.Command("gh", "repo", "sync", repo)
	return runWithStderr(cmd)
}

const securityPolicyTemplate = `# Security Policy

If you discover a security vulnerability, please email %s directly rather than opening a public issue or PR.

I'll acknowledge receipt within 7 days and aim to provide an initial assessment within 30 days. We can coordinate on a disclosure timeline — defaulting to 90 days from initial report unless circumstances warrant otherwise.

Thanks for helping keep this project and its users safe.
`

func fixSecurityPolicy(repo, email, mergeStrategy string) error {
	content := fmt.Sprintf(securityPolicyTemplate, email)
	return writeFile(repo, "SECURITY.md", content,
		"add SECURITY.md (via vidette)",
		"Add SECURITY.md",
		"Adds a security policy referencing the maintainer email and a 90-day disclosure window. Generated by vidette.",
		mergeStrategy)
}

func fixArchiveState(repo string) error {
	cmd := exec.Command("gh", "repo", "archive", repo, "--yes")
	return runWithStderr(cmd)
}

func fixHomepage(repo, url string) error {
	cmd := exec.Command("gh", "api", "-X", "PATCH", "repos/"+repo, "-f", "homepage="+url)
	return runWithStderr(cmd)
}

func fixDependabot(repo, mergeStrategy string) error {
	eco, err := probeEcosystem(repo)
	if err != nil {
		return fmt.Errorf("probe ecosystem: %w", err)
	}
	_, hasWorkflows, _ := ghAPI("repos/" + repo + "/contents/.github/workflows")
	if eco == "" && !hasWorkflows {
		return fmt.Errorf("no detectable ecosystem and no workflows directory — refusing to write empty dependabot.yml")
	}
	content := renderDependabotYAML(eco, hasWorkflows)
	return writeFile(repo, ".github/dependabot.yml", content,
		"add dependabot config (via vidette)",
		"Add Dependabot config",
		"Adds a .github/dependabot.yml tracking the repo's primary ecosystem. Generated by vidette.",
		mergeStrategy)
}

// probeEcosystem returns the primary dependabot package-ecosystem name based on
// GitHub's language stats. Returns "" if no mapping is known — caller writes a
// github-actions-only file in that case.
func probeEcosystem(repo string) (string, error) {
	body, ok, err := ghAPI("repos/" + repo + "/languages")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	var langs map[string]int
	if err := json.Unmarshal(body, &langs); err != nil {
		return "", err
	}
	if len(langs) == 0 {
		return "", nil
	}
	type kv struct {
		k string
		v int
	}
	pairs := make([]kv, 0, len(langs))
	for k, v := range langs {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].v > pairs[j].v })
	switch pairs[0].k {
	case "Go":
		return "gomod", nil
	case "Python":
		return "pip", nil
	case "JavaScript", "TypeScript":
		return "npm", nil
	case "Rust":
		return "cargo", nil
	case "Ruby":
		return "bundler", nil
	}
	return "", nil
}

func renderDependabotYAML(eco string, hasWorkflows bool) string {
	var b strings.Builder
	b.WriteString("version: 2\nupdates:\n")
	if eco != "" {
		fmt.Fprintf(&b, "  - package-ecosystem: %s\n    directory: /\n    schedule:\n      interval: weekly\n", eco)
	}
	// Only include github-actions ecosystem if there's actually a workflows
	// directory — Dependabot errors with dependency_file_not_found otherwise.
	if hasWorkflows {
		b.WriteString("  - package-ecosystem: github-actions\n    directory: /\n    schedule:\n      interval: weekly\n")
	}
	return b.String()
}

// writeFile writes `content` to `path` in the repo's default branch. If the
// direct contents-API write is blocked by branch protection or repository
// rulesets, it falls back to creating a `vidette/...` feature branch, writing
// there, opening a PR, and enabling auto-merge with mergeStrategy. PR URL is
// logged to stderr.
func writeFile(repo, path, content, commitMessage, prTitle, prBody, mergeStrategy string) error {
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	cmd := exec.Command("gh", "api", "-X", "PUT",
		"repos/"+repo+"/contents/"+path,
		"-f", "message="+commitMessage,
		"-f", "content="+encoded,
	)
	err := runWithStderr(cmd)
	if err == nil {
		return nil
	}
	if !isProtectionBlock(err) {
		return err
	}
	return commitFileViaPR(repo, path, content, commitMessage, prTitle, prBody, mergeStrategy)
}

func isProtectionBlock(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "http 409") ||
		strings.Contains(s, "rule violations") ||
		strings.Contains(s, "status checks") ||
		strings.Contains(s, "required status check")
}

func commitFileViaPR(repo, path, content, commitMessage, prTitle, prBody, mergeStrategy string) error {
	meta, err := FetchRepoMeta(repo)
	if err != nil {
		return fmt.Errorf("fetch meta: %w", err)
	}
	defaultBranch := meta.DefaultBranch
	if defaultBranch == "" {
		return fmt.Errorf("repo has no default branch")
	}

	// Idempotency: if a prior apply already opened a PR for this exact
	// file, don't open a duplicate. The previous PR is still the active
	// path to merge this change.
	slug := pathToSlug(path)
	if existing, ok := findExistingViddettePR(repo, slug); ok {
		fmt.Fprintf(os.Stderr, "  → PR already open: %s (vidette/%s-* branch)\n", existing, slug)
		return nil
	}

	refBody, ok, err := ghAPI("repos/" + repo + "/git/ref/heads/" + defaultBranch)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("default branch ref not found: %s", defaultBranch)
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(refBody, &ref); err != nil {
		return fmt.Errorf("parse ref: %w", err)
	}

	// Random suffix (not unix-time) so parallel applies for distinct files
	// don't collide on a 1-second resolution.
	suffix, err := randomSuffix(6)
	if err != nil {
		return fmt.Errorf("random suffix: %w", err)
	}
	branchName := fmt.Sprintf("vidette/%s-%s", slug, suffix)
	refReq, _ := json.Marshal(map[string]any{
		"ref": "refs/heads/" + branchName,
		"sha": ref.Object.SHA,
	})
	cmd := exec.Command("gh", "api", "-X", "POST", "repos/"+repo+"/git/refs", "--input", "-")
	cmd.Stdin = bytes.NewReader(refReq)
	if err := runWithStderr(cmd); err != nil {
		return fmt.Errorf("create branch %s: %w", branchName, err)
	}

	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	cmd = exec.Command("gh", "api", "-X", "PUT",
		"repos/"+repo+"/contents/"+path,
		"-f", "message="+commitMessage,
		"-f", "content="+encoded,
		"-f", "branch="+branchName,
	)
	if err := runWithStderr(cmd); err != nil {
		return fmt.Errorf("write file to %s: %w", branchName, err)
	}

	cmd = exec.Command("gh", "pr", "create", "-R", repo,
		"--head", branchName,
		"--base", defaultBranch,
		"--title", prTitle,
		"--body", prBody,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	prOut, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("create PR: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	prURL := strings.TrimSpace(string(prOut))
	fmt.Fprintf(os.Stderr, "  → PR opened: %s (branch protected; manual merge if auto-merge can't satisfy checks)\n", prURL)

	if prURL != "" {
		strategyFlag := "--squash"
		switch mergeStrategy {
		case "rebase":
			strategyFlag = "--rebase"
		case "merge":
			strategyFlag = "--merge"
		}
		mergeCmd := exec.Command("gh", "pr", "merge", prURL, "--auto", strategyFlag)
		_ = runWithStderr(mergeCmd)
	}
	return nil
}

// randomSuffix returns n bytes of crypto-random hex, suitable for branch
// names that need to be collision-resistant across parallel apply runs.
func randomSuffix(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// findExistingViddettePR returns the URL of an open PR on `repo` whose head
// branch matches `vidette/<slug>-*`, or ("", false) if there is none.
// Returns ("", false) on any probe error rather than failing the apply —
// worst case we open a duplicate, which is the pre-existing behavior.
func findExistingViddettePR(repo, slug string) (string, bool) {
	out, err := exec.Command("gh", "pr", "list", "-R", repo,
		"--state", "open",
		"--json", "url,headRefName",
		"--limit", fmt.Sprintf("%d", prListLimit),
	).Output()
	if err != nil {
		return "", false
	}
	var prs []struct {
		URL         string `json:"url"`
		HeadRefName string `json:"headRefName"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return "", false
	}
	prefix := "vidette/" + slug + "-"
	for _, p := range prs {
		if strings.HasPrefix(p.HeadRefName, prefix) {
			return p.URL, true
		}
	}
	return "", false
}

func pathToSlug(path string) string {
	base := filepath.Base(path)
	name := strings.TrimSuffix(base, filepath.Ext(base))
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, ".", "-")
	if name == "" {
		name = "file"
	}
	return name
}

func runWithStderr(cmd *exec.Cmd) error {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// RunApply prints the fix plan; if doApply is true, executes each fixable action.
// Unfixable fails are listed at the end as "still requires your attention".
// In apply mode, a trailing "Changes" section rolls up successes (and failures)
// grouped by repo so the operator can see at a glance what mutated.
func RunApply(w io.Writer, ctx context.Context, audits []RepoAudit, defaults Defaults, doApply bool) (applied, failed int) {
	stats := ComputeFleetStats(audits)
	actions := PlanFixes(audits, stats, defaults)
	var fixable, unfixable []FixAction
	for _, a := range actions {
		if a.NoFix {
			unfixable = append(unfixable, a)
		} else {
			fixable = append(fixable, a)
		}
	}

	if doApply {
		fmt.Fprintf(w, "# vidette apply\n\n**Mode:** executing (mutating GitHub)\n\n")
	} else {
		fmt.Fprintf(w, "# vidette plan\n\n**Mode:** dry-run (run `vidette apply` to execute)\n\n")
	}

	type result struct {
		check string
		err   error
	}
	applyResults := map[string][]result{}
	var applyOrder []string

	if len(fixable) == 0 {
		fmt.Fprintf(w, "No fixable findings.\n")
	} else {
		fmt.Fprintf(w, "## Fixable (%d)\n\n", len(fixable))
		for _, a := range fixable {
			if doApply {
				if err := ctx.Err(); err != nil {
					fmt.Fprintf(w, "- `%s` · %s\n  - plan: %s\n  - ⌧ skipped: %s\n", a.Repo, a.Check, a.Plan, err)
					continue
				}
			}
			fmt.Fprintf(w, "- `%s` · %s\n  - plan: %s\n", a.Repo, a.Check, a.Plan)
			if doApply {
				err := a.Apply()
				if _, seen := applyResults[a.Repo]; !seen {
					applyOrder = append(applyOrder, a.Repo)
				}
				applyResults[a.Repo] = append(applyResults[a.Repo], result{a.Check, err})
				if err != nil {
					fmt.Fprintf(w, "  - ✗ apply failed: %s\n", err)
					failed++
				} else {
					fmt.Fprintf(w, "  - ✓ applied\n")
					applied++
				}
			}
		}
	}

	if len(unfixable) > 0 {
		fmt.Fprintf(w, "\n## Still requires your attention (%d)\n\n", len(unfixable))
		for _, a := range unfixable {
			fmt.Fprintf(w, "- `%s` · %s — %s\n", a.Repo, a.Check, a.Reason)
		}
	}

	if doApply && len(applyOrder) > 0 {
		fmt.Fprintf(w, "\n## Changes (%d applied · %d failed)\n\n", applied, failed)
		for _, repo := range applyOrder {
			rs := applyResults[repo]
			var oks, errs []string
			for _, r := range rs {
				if r.err == nil {
					oks = append(oks, r.check)
				} else {
					errs = append(errs, r.check)
				}
			}
			fmt.Fprintf(w, "- `%s`:", repo)
			if len(oks) > 0 {
				fmt.Fprintf(w, " ✓ %s", strings.Join(oks, ", "))
			}
			if len(errs) > 0 {
				fmt.Fprintf(w, " ✗ %s", strings.Join(errs, ", "))
			}
			fmt.Fprintln(w)
		}
	}
	return applied, failed
}
