# Feature request + feedback: audit the Dependabot *alerts* setting, not just the config file

**Date:** 2026-05-26
**Reporter:** Claude (Opus 4.7), from a session working in the `plancheck` repo
**Context:** While addressing security alerts in `justinstimatze/plancheck` (a public Go
repo), I found that vidette's existing `dependabot` check sees one layer of a
two-layer problem. Filing this where a future vidette session can pick it up.

## TL;DR

vidette's `dependabot` check probes whether `.github/dependabot.yml` *exists* and
writes it when missing. It does **not** probe whether the repo's **Dependabot
vulnerability-alerts setting** is enabled. Those are independent toggles: a repo can
have the config file and still have alerts off, or — as happened in plancheck — have
*both* off. The config-file half is fleet hygiene vidette already does well; the
alerts-setting half is a pure settings probe that fits vidette's deterministic model
exactly and is currently invisible to it.

## The incident (concrete)

plancheck, a public Go repo:

1. **`.github/dependabot.yml` was missing.** vidette's `dependabot` check *would*
   have caught this (`SevFail: "no .github/dependabot.yml"`) — assuming plancheck is
   in the fleet config. This half works.
2. **Dependabot vulnerability alerts were *disabled* at the repo settings level.**
   `GET /repos/{owner}/{repo}/dependabot/alerts` returned `403 "Dependabot alerts are
   disabled for this repository."` Nothing in vidette surfaces this — the config file
   and the alerts toggle are separate, and vidette only looks at the file.
3. Separately, `govulncheck` found 5 *reachable* vulnerabilities (a stale
   `golang.org/x/net` and four stdlib CVEs from an un-bumped Go toolchain). See the
   non-goal below — this layer is deliberately **not** a vidette concern.

The fix in plancheck was three independent actions:
`PUT /repos/{}/vulnerability-alerts`, `PUT /repos/{}/automated-security-fixes`, and
writing `.github/dependabot.yml`. vidette today would only have prompted the third.

## Feature request: an `alerts` check

Add a tier-aware check (sibling of `dependabot`) that probes the alerts setting. It
slots cleanly into the existing pattern in `audit.go` and reuses `ghAPI` from
`github.go` — `ghAPI` already returns the `(body, exists, err)` triple where `exists`
distinguishes 2xx from 404, which is precisely the `vulnerability-alerts` semantics
(204 enabled / 404 disabled).

**audit.go** — alongside the `dependabot` block:

```go
// Dependabot alerts — settings-level toggle, independent of the config file.
// GET .../vulnerability-alerts returns 204 (enabled) or 404 (disabled).
if required(rule{requiredFor: ownAndRewritten, tierFloor: TierActive}, rc) {
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
```

**github.go** — one helper, mirroring `HasBranchProtection`:

```go
// DependabotAlertsEnabled reports whether vulnerability alerts are on.
// 204 → enabled, 404 → disabled.
func DependabotAlertsEnabled(repo string) (bool, error) {
    _, ok, err := ghAPI(fmt.Sprintf("repos/%s/vulnerability-alerts", repo))
    return ok, err
}
```

**apply.go** — the repair is a settings mutation, not a file write, so no branch /
PR fallback is needed:

```
gh api -X PUT repos/{owner}/{repo}/vulnerability-alerts
gh api -X PUT repos/{owner}/{repo}/automated-security-fixes   # optional second action
```

Notes for whoever implements this:
- The `vulnerability-alerts` endpoint historically needs `repo` (or `admin:repo_hook`)
  scope and repo-admin permission. The probe (GET) is cheap; the PUT may need a token
  refresh — handle the scope error like the other privileged mutations.
- Tier-awareness: like `dependabot`, only nag `active`+ repos that have a package
  ecosystem or workflows (`repoHasDependabotSurface`). An archived or scratch repo with
  alerts off is not drift.
- Consider gating on visibility: enabling alerts matters most on public repos.

## Explicit non-goal (what this is NOT asking for)

This is **not** a request for vidette to run `govulncheck`, parse advisories, or do any
code-reachability / CVE analysis. That is a per-repo CI concern (the repo's own
`go test` / vuln-scan step), not fleet hygiene, and it would break vidette's
"deterministic, no-API-key, no-model, reproducible-tomorrow" property. vidette's job is
*"is this repo configured to receive and act on alerts"* — a settings-state probe —
not *"does this code call a known-vulnerable symbol."* Keep the boundary there.

## Why it fits vidette's model

- Pure settings state from `gh api` — deterministic, no LLM, reproducible.
- Reuses the existing `ghAPI` `exists`-bool pattern and the tier/`required` machinery.
- The auto-fix is idempotent (PUT is a no-op when already enabled) and needs no
  branch/PR fallback, since it mutates settings rather than files.
- It closes a real, observed blind spot: "config file present" silently read as
  "alerts on," when they are independent toggles.
