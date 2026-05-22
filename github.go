package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// rateLimitBackoff is how long to sleep when a request hits GitHub's rate
// limit before retrying. One attempt only — if the second try also fails the
// error is surfaced unchanged. Vidette doesn't try to be clever about
// long-term throttling; that's the caller's job.
const rateLimitBackoff = 60 * time.Second

// ghAPI runs `gh api <path>` and returns body, exists, error.
// exists=false when the endpoint returned 404 (the resource is simply absent).
// error is for transport/auth/parse failures — not 404. On a rate-limit
// response (HTTP 403 + "rate limit" in stderr), ghAPI sleeps once and retries.
func ghAPI(path string) (body []byte, exists bool, err error) {
	body, exists, err = ghAPIOnce(path)
	if err != nil && isRateLimitError(err) {
		fmt.Fprintf(os.Stderr, "vidette: rate limit hit on %s; sleeping %s and retrying once\n", path, rateLimitBackoff)
		time.Sleep(rateLimitBackoff)
		body, exists, err = ghAPIOnce(path)
	}
	return body, exists, err
}

func ghAPIOnce(path string) (body []byte, exists bool, err error) {
	cmd := exec.Command("gh", "api", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		es := stderr.String()
		if strings.Contains(es, "HTTP 404") || strings.Contains(es, "Not Found") {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("gh api %s: %w: %s", path, err, strings.TrimSpace(es))
	}
	return out, true, nil
}

// isRateLimitError reports whether err looks like a GitHub rate-limit
// rejection. gh's stderr on rate limit contains "rate limit" or the legacy
// "abuse rate limit" phrasing; either signal is sufficient.
func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "rate limit") ||
		strings.Contains(s, "api rate") ||
		strings.Contains(s, "abuse detection") ||
		(strings.Contains(s, "http 403") && strings.Contains(s, "secondary rate"))
}

type RepoMeta struct {
	NameWithOwner string `json:"full_name"`
	Name          string `json:"name"`
	DefaultBranch string `json:"default_branch"`
	IsFork        bool   `json:"fork"`
	Archived      bool   `json:"archived"`
	Description   string `json:"description"`
	Homepage      string `json:"homepage"`
	Topics        []string
	License       *struct {
		SPDXID string `json:"spdx_id"`
	} `json:"license"`
	AllowAutoMerge bool   `json:"allow_auto_merge"`
	PushedAt       string `json:"pushed_at"`
	Parent         *struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	} `json:"parent"`
}

func FetchRepoMeta(nameWithOwner string) (*RepoMeta, error) {
	body, ok, err := ghAPI("repos/" + nameWithOwner)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("repo %s not found", nameWithOwner)
	}
	var m RepoMeta
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("parse repo meta: %w", err)
	}
	// Topics comes back as {"names": [...]} from the topics endpoint, but
	// repos/{owner}/{repo} returns them as "topics" too; defensive parse.
	var topicsWrap struct {
		Topics []string `json:"topics"`
	}
	_ = json.Unmarshal(body, &topicsWrap)
	m.Topics = topicsWrap.Topics
	return &m, nil
}

// ListUserRepos returns all repos for the user, including archived ones —
// archived repos can drift back to live state (manual unarchive), and the
// audit's archive-state check needs them in the fleet to detect that. Tier
// floors on individual checks suppress noise for tier=archived configs.
// When includePrivate is true, private + internal repos are included (gh's
// default when no --visibility is passed); otherwise only public.
func ListUserRepos(user string, includePrivate bool) ([]string, error) {
	args := []string{"repo", "list", user, "--limit", "1000", "--json", "nameWithOwner"}
	if !includePrivate {
		args = append(args, "--visibility", "public")
	}
	cmd := exec.Command("gh", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("gh repo list: %w", err)
	}
	var rs []struct {
		NameWithOwner string `json:"nameWithOwner"`
	}
	if err := json.Unmarshal(out, &rs); err != nil {
		return nil, fmt.Errorf("parse repo list: %w", err)
	}
	names := make([]string, len(rs))
	for i, r := range rs {
		names[i] = r.NameWithOwner
	}
	return names, nil
}

func HasFile(repo, path string) (bool, error) {
	_, ok, err := ghAPI(fmt.Sprintf("repos/%s/contents/%s", repo, path))
	return ok, err
}

// repoHasDependabotSurface reports whether the repo has anything for dependabot
// to manage — a recognized language (so package manifests likely exist) or a
// .github/workflows directory.
func repoHasDependabotSurface(repo string) bool {
	if body, ok, err := ghAPI("repos/" + repo + "/languages"); err == nil && ok {
		var langs map[string]int
		if json.Unmarshal(body, &langs) == nil && len(langs) > 0 {
			return true
		}
	}
	_, hasWorkflows, _ := ghAPI("repos/" + repo + "/contents/.github/workflows")
	return hasWorkflows
}

func HasBranchProtection(repo, branch string) (bool, error) {
	_, ok, err := ghAPI(fmt.Sprintf("repos/%s/branches/%s/protection", repo, branch))
	return ok, err
}

type CIRun struct {
	Conclusion string `json:"conclusion"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
}

// LatestCIRun returns the most recent push-event workflow run on the given
// branch, or nil if there are none. Filtering to event=push avoids picking up
// pull_request runs (which can have action_required status from external
// contributors and confuse the audit about the actual main-branch state).
func LatestCIRun(repo, branch string) (*CIRun, error) {
	body, ok, err := ghAPI(fmt.Sprintf("repos/%s/actions/runs?per_page=1&branch=%s&event=push", repo, branch))
	if err != nil || !ok {
		return nil, err
	}
	var wrap struct {
		Runs []CIRun `json:"workflow_runs"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, fmt.Errorf("parse runs: %w", err)
	}
	if len(wrap.Runs) == 0 {
		return nil, nil
	}
	return &wrap.Runs[0], nil
}

// CompareUpstream returns commits-behind for a tracking fork's default branch
// vs. the parent's default branch. Returns 0 / nil if not a fork or parent
// info unavailable.
func CompareUpstream(meta *RepoMeta) (int, error) {
	if meta.Parent == nil {
		return 0, nil
	}
	// /repos/{owner}/{repo}/compare/{base}...{head}
	// base = parent's branch via parent name, head = user:branch
	parts := strings.SplitN(meta.NameWithOwner, "/", 2)
	if len(parts) != 2 {
		return 0, nil
	}
	user := parts[0]
	parentParts := strings.SplitN(meta.Parent.FullName, "/", 2)
	if len(parentParts) != 2 {
		return 0, nil
	}
	parentOwner := parentParts[0]
	base := fmt.Sprintf("%s:%s", parentOwner, meta.Parent.DefaultBranch)
	head := fmt.Sprintf("%s:%s", user, meta.DefaultBranch)
	path := fmt.Sprintf("repos/%s/compare/%s...%s", meta.Parent.FullName, base, head)
	body, ok, err := ghAPI(path)
	if err != nil || !ok {
		return 0, err
	}
	var cmp struct {
		BehindBy int `json:"behind_by"`
		AheadBy  int `json:"ahead_by"`
	}
	if err := json.Unmarshal(body, &cmp); err != nil {
		return 0, fmt.Errorf("parse compare: %w", err)
	}
	return cmp.BehindBy, nil
}
