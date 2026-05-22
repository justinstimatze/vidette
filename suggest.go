package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	// maxLanguagesInEvidence caps the number of top languages included per
	// repo in the suggest prompt. Three is enough to disambiguate the
	// majority case without bloating the prompt for polyglot repos.
	maxLanguagesInEvidence = 3
	// readmeExcerptBytes is the per-repo README budget in the suggest
	// prompt. ~800 bytes captures the lede and first content section of
	// most READMEs without ballooning multi-repo prompts.
	readmeExcerptBytes = 800
	// suggestParallel is the number of evidence-gathers run concurrently.
	suggestParallel = 6
)

// RepoEvidence holds the per-repo material needed to propose a tier and
// fork_relation. Kept small on purpose — the LLM doing the classification
// (typically Claude in the user's session) needs signal, not source.
type RepoEvidence struct {
	Repo        string
	Description string
	Topics      []string
	Archived    bool
	IsFork      bool
	ParentName  string
	Languages   []string
	Readme      string
}

func gatherEvidence(repo string) RepoEvidence {
	ev := RepoEvidence{Repo: repo}
	if meta, err := FetchRepoMeta(repo); err == nil {
		ev.Description = meta.Description
		ev.Topics = meta.Topics
		ev.Archived = meta.Archived
		ev.IsFork = meta.IsFork
		if meta.Parent != nil {
			ev.ParentName = meta.Parent.FullName
		}
	}
	if body, ok, _ := ghAPI("repos/" + repo + "/languages"); ok {
		var langs map[string]int
		// Ignored unmarshal errors here are deliberate: the languages
		// endpoint is best-effort signal for the suggest prompt, not a
		// correctness boundary. Missing it just means the LLM sees fewer
		// hints, which is acceptable.
		if json.Unmarshal(body, &langs) == nil {
			type kv struct {
				k string
				v int
			}
			pairs := make([]kv, 0, len(langs))
			for k, v := range langs {
				pairs = append(pairs, kv{k, v})
			}
			sort.Slice(pairs, func(i, j int) bool { return pairs[i].v > pairs[j].v })
			n := maxLanguagesInEvidence
			if len(pairs) < n {
				n = len(pairs)
			}
			for _, p := range pairs[:n] {
				ev.Languages = append(ev.Languages, p.k)
			}
		}
	}
	if body, ok, _ := ghAPI("repos/" + repo + "/readme"); ok {
		var rdme struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(body, &rdme) == nil {
			raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(rdme.Content, "\n", ""))
			if err == nil {
				ev.Readme = truncateUTF8(string(raw), readmeExcerptBytes)
			}
		}
	}
	return ev
}

// truncateUTF8 returns s truncated to at most n bytes, backing off to a valid
// UTF-8 rune boundary so the result is never a half-character. Appends "..."
// when truncation occurred.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}

func RunSuggest(w io.Writer, cfg *Config, includePrivate bool) error {
	fleet, err := ListUserRepos(cfg.User, includePrivate)
	if err != nil {
		return err
	}

	var unconfigured []string
	for _, repo := range fleet {
		short := strings.TrimPrefix(repo, cfg.User+"/")
		if _, ok := cfg.Repos[short]; !ok {
			unconfigured = append(unconfigured, repo)
		}
	}

	if len(unconfigured) == 0 {
		fmt.Fprintln(w, "All fleet repos are declared in vidette.yml — nothing to suggest.")
		return nil
	}

	evs := make([]RepoEvidence, len(unconfigured))
	sem := make(chan struct{}, suggestParallel)
	var wg sync.WaitGroup
	for i, repo := range unconfigured {
		i, repo := i, repo
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			evs[i] = gatherEvidence(repo)
		}()
	}
	wg.Wait()

	renderSuggestPrompt(w, evs)
	return nil
}

func renderSuggestPrompt(w io.Writer, evs []RepoEvidence) {
	fmt.Fprintln(w, "# vidette suggest — tier classification needed")
	fmt.Fprintf(w, "\n%d repos are in the fleet but not declared in `vidette.yml`. Read the evidence for each and propose `fork_relation` + `tier`.\n\n", len(evs))
	fmt.Fprintln(w, "**fork_relation:** `own` (you wrote it) · `rewritten` (fork where you replaced most code) · `tracking` (fork tracking upstream) · `snapshot` (frozen fork) · `profile` (user/user profile-readme repo)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "**tier:** `active` (under development) · `maintained` (shipped, low-touch) · `demo` (showcase/POC) · `scratch` (throwaway) · `archived` (done; also archive on GitHub)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Reply with a YAML block to paste under `repos:` in vidette.yml:")
	fmt.Fprintln(w, "```yaml")
	fmt.Fprintln(w, "foo: { fork_relation: own, tier: maintained }")
	fmt.Fprintln(w, "bar: { fork_relation: own, tier: scratch, notes: \"experiment\" }")
	fmt.Fprintln(w, "```")
	fmt.Fprintln(w, "\n---")

	for _, ev := range evs {
		fmt.Fprintf(w, "\n## `%s`\n\n", ev.Repo)
		if ev.Archived {
			fmt.Fprintln(w, "- **archived on GitHub**")
		}
		if ev.IsFork {
			fmt.Fprintf(w, "- **fork of %s**\n", ev.ParentName)
		}
		if ev.Description != "" {
			fmt.Fprintf(w, "- description: %s\n", ev.Description)
		} else {
			fmt.Fprintln(w, "- description: (none)")
		}
		if len(ev.Topics) > 0 {
			fmt.Fprintf(w, "- topics: %s\n", strings.Join(ev.Topics, ", "))
		}
		if len(ev.Languages) > 0 {
			fmt.Fprintf(w, "- languages: %s\n", strings.Join(ev.Languages, ", "))
		}
		if ev.Readme != "" {
			fmt.Fprintln(w, "\n**README excerpt:**")
			fmt.Fprintln(w, "```")
			fmt.Fprintln(w, ev.Readme)
			fmt.Fprintln(w, "```")
		} else {
			fmt.Fprintln(w, "\n*(no README)*")
		}
		fmt.Fprintln(w, "\n---")
	}
}
