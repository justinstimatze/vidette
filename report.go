package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

type Summary struct {
	Total     int
	Fail      int
	Warn      int
	OK        int
	Unconfig  []string
	FetchErrs []string
}

func RenderReport(w io.Writer, audits []RepoAudit, unconfigured []string, configuredNotFound []string) {
	var sum Summary
	for _, a := range audits {
		sum.Total++
		if a.FetchErr != nil {
			sum.FetchErrs = append(sum.FetchErrs, a.Repo+": "+a.FetchErr.Error())
			continue
		}
		for _, c := range a.Checks {
			switch c.Severity {
			case SevFail:
				sum.Fail++
			case SevWarn:
				sum.Warn++
			case SevOK:
				sum.OK++
			}
		}
	}
	sum.Unconfig = unconfigured

	fmt.Fprintf(w, "# vidette audit\n\n")
	fmt.Fprintf(w, "**%d repos** · ✗ %d fail · ⚠ %d warn · ✓ %d ok\n\n", sum.Total, sum.Fail, sum.Warn, sum.OK)

	if len(unconfigured) > 0 {
		fmt.Fprintf(w, "## Unconfigured repos\n\nFound in fleet but not in vidette.yml — add `fork_relation` and `tier`:\n\n")
		for _, r := range unconfigured {
			fmt.Fprintf(w, "- `%s`\n", r)
		}
		fmt.Fprintln(w)
	}

	if len(configuredNotFound) > 0 {
		fmt.Fprintf(w, "## Configured but not in fleet\n\nIn vidette.yml but `gh repo list` didn't return them (renamed? archived? private?):\n\n")
		for _, r := range configuredNotFound {
			fmt.Fprintf(w, "- `%s`\n", r)
		}
		fmt.Fprintln(w)
	}

	// Priority alarms: collect all SevFail across the fleet
	type alarm struct {
		Repo  string
		Tier  Tier
		Check string
		Sev   Severity
		Det   string
	}
	var alarms []alarm
	for _, a := range audits {
		for _, c := range a.Checks {
			if c.Severity == SevFail || c.Severity == SevWarn {
				alarms = append(alarms, alarm{a.Repo, a.Tier, c.Name, c.Severity, c.Detail})
			}
		}
	}
	sort.SliceStable(alarms, func(i, j int) bool {
		if alarms[i].Sev != alarms[j].Sev {
			return alarms[i].Sev > alarms[j].Sev // SevFail > SevWarn
		}
		if alarms[i].Tier != alarms[j].Tier {
			return tierOrder(alarms[i].Tier) > tierOrder(alarms[j].Tier)
		}
		return alarms[i].Repo < alarms[j].Repo
	})

	if len(alarms) > 0 {
		fmt.Fprintf(w, "## Alarms (sorted by severity × tier)\n\n")
		fmt.Fprintf(w, "| sev | repo | tier | check | detail |\n")
		fmt.Fprintf(w, "|---|---|---|---|---|\n")
		for _, al := range alarms {
			fmt.Fprintf(w, "| %s | `%s` | %s | %s | %s |\n", al.Sev.Glyph(), al.Repo, al.Tier, al.Check, al.Det)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "## Per-repo detail\n\n")
	sort.Slice(audits, func(i, j int) bool { return audits[i].Repo < audits[j].Repo })
	for _, a := range audits {
		fmt.Fprintf(w, "### `%s` [%s, %s]\n\n", a.Repo, a.ForkRelation, a.Tier)
		if a.FetchErr != nil {
			fmt.Fprintf(w, "- ✗ fetch failed: %s\n\n", a.FetchErr)
			continue
		}
		for _, c := range a.Checks {
			line := fmt.Sprintf("- %s %s", c.Severity.Glyph(), c.Name)
			if c.Detail != "" {
				line += ": " + c.Detail
			}
			fmt.Fprintln(w, line)
		}
		fmt.Fprintln(w)
	}

	if len(sum.FetchErrs) > 0 {
		fmt.Fprintf(w, "## Fetch errors\n\n")
		for _, e := range sum.FetchErrs {
			fmt.Fprintf(w, "- %s\n", e)
		}
	}
}

// diffRepos returns (inFleetNotConfig, inConfigNotFleet)
func diffRepos(fleet []string, cfg *Config, user string) (unconfigured, missing []string) {
	configured := map[string]bool{}
	for name := range cfg.Repos {
		configured[user+"/"+name] = true
	}
	fleetSet := map[string]bool{}
	for _, f := range fleet {
		fleetSet[f] = true
		if !configured[f] {
			// strip user/ for friendlier display
			short := strings.TrimPrefix(f, user+"/")
			unconfigured = append(unconfigured, short)
		}
	}
	for name := range cfg.Repos {
		if !fleetSet[user+"/"+name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(unconfigured)
	sort.Strings(missing)
	return
}
