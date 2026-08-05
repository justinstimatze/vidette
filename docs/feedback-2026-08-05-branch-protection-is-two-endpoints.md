# Feedback: `branch-protection` reads one of the two endpoints that protect a branch

**Date:** 2026-08-05
**Reporter:** Claude (Opus 5), from a session working in the `ettle` repo
**Context:** Rolling Dependabot auto-merge out across the fleet, I swept nine repos for
"has the auto-merge workflow, has a required status check" and got one repo wrong.
Chasing why turned up a live blind spot in vidette. Filing it where a future vidette
session can pick it up.

## TL;DR

`HasBranchProtection` (`github.go:152`) probes `repos/{}/branches/{}/protection` — the
**classic** branch-protection API. GitHub protects branches through two independent
systems: classic protection and **rulesets**, and it enforces the union of both. A repo
whose `main` is fully protected by a ruleset returns 404 from the classic endpoint, so
vidette reports `branch-protection: FAIL "default branch unprotected"` on a branch that
is, in fact, protected.

`vidette apply` then offers to fix it by PUTting classic protection with
`"required_status_checks": nil` (`apply.go:542`). That is not destructive — the two
layers are additive, so the ruleset's requirements survive — but the check goes green
for the wrong reason, and on a repo with *no* ruleset the same repair produces a
protected branch that requires nothing, while `auto-merge: OK` sits next to it.

## The incident (concrete)

Sweeping nine repos on 2026-08-05 via `repos/{}/branches/main/protection`, I read
`slimemold` as having the Dependabot auto-merge workflow and no required status check —
the state where `gh pr merge --auto` has nothing pending to wait on.

That should mean Dependabot PRs merge without waiting for CI. So I checked whether they
had. slimemold #50 (`modernc.org/sqlite` 1.54.0 → 1.55.0):

| event | time |
|---|---|
| all three workflows start | 2026-08-05T07:43:59Z |
| auto-merge job concludes | 07:44:15Z |
| CI concludes (success) | 07:45:22Z |
| PR merged | 07:45:24Z |

Two seconds after CI, seventy after auto-merge was enabled. It waited. Something was
enforcing a requirement that the classic endpoint did not report:

```
$ gh api repos/justinstimatze/slimemold/rulesets --jq '.[] | "\(.name) \(.enforcement)"'
Default active

$ gh api repos/justinstimatze/slimemold/rules/branches/main \
    --jq '.[] | {type, checks: .parameters.required_status_checks}'
{"type":"required_status_checks","checks":[{"context":"test"}]}
```

slimemold is in `vidette.yml` at `tier: active`, and the `branch-protection` check has
`tierFloor: TierMaintained` (`audit.go:231`) with `tierOrder(active)=4 ≥ maintained=3`,
so this repo is audited today and reads FAIL today.

Of the nine, exactly one used a ruleset. The other eight are classic-only, which is why
this has stayed invisible: the fleet is nearly homogeneous, and the one exception is the
one that looked broken.

## Request 1: make `branch-protection` ruleset-aware

`GET /repos/{owner}/{repo}/rules/branches/{branch}` returns the **effective** rules for
a branch — one call, already resolved across every ruleset that matches. It returns
`[]` when none apply. It does *not* include classic protection, so both probes are
needed and the answer is the union.

**github.go** — sibling of `HasBranchProtection`:

```go
// BranchRuleTypes returns the effective ruleset rule types for a branch.
// An empty slice means no ruleset applies; this is independent of, and additive
// to, classic branch protection.
func BranchRuleTypes(repo, branch string) ([]string, error) {
    body, ok, err := ghAPI(fmt.Sprintf("repos/%s/rules/branches/%s", repo, branch))
    if err != nil || !ok {
        return nil, err
    }
    var rules []struct {
        Type string `json:"type"`
    }
    if err := json.Unmarshal(body, &rules); err != nil {
        return nil, err
    }
    out := make([]string, 0, len(rules))
    for _, r := range rules {
        out = append(out, r.Type)
    }
    return out, nil
}
```

**audit.go** — `branch-protection` passes if either layer covers the branch:

```go
has, err := HasBranchProtection(repo, meta.DefaultBranch)
ruleTypes, rerr := BranchRuleTypes(repo, meta.DefaultBranch)
switch {
case err != nil || rerr != nil:
    // ... existing SevWarn probe-error path
case has:
    a.Checks = append(a.Checks, Check{"branch-protection", SevOK, ""})
case len(ruleTypes) > 0:
    a.Checks = append(a.Checks, Check{"branch-protection", SevOK, "ruleset"})
default:
    a.Checks = append(a.Checks, Check{"branch-protection", SevFail, "default branch unprotected"})
}
```

The `"ruleset"` detail string matters for the operator: "protected, and not by the
mechanism vidette would have written" is worth seeing in the report.

## Request 2: a `required-checks` check

The sweep's original question — is auto-merge gated on anything? — is one vidette
cannot answer today. `auto-merge` (`audit.go:222`) reads `meta.AllowAutoMerge` and
stops; `branch-protection` reads existence and stops. Both were OK on every repo in the
sweep, including the ones where nothing gated a merge.

The two settings interact. `allow_auto_merge=true` plus a `pull_request`-triggered
auto-merge workflow plus zero required contexts means Dependabot PRs merge before CI
reports. Each half is fine alone, and vidette's repairs turn on the first
(`apply.go:160`) and can create the second (`apply.go:542`).

```go
// Required status checks — only meaningful where auto-merge can act on them.
if required(rule{requiredFor: ownAndRewritten, tierFloor: TierActive}, rc) {
    contexts, err := RequiredCheckContexts(repo, meta.DefaultBranch) // union of both layers
    switch {
    case err != nil:
        a.Checks = append(a.Checks, Check{"required-checks", SevWarn, "probe error: " + err.Error()})
    case len(contexts) > 0:
        a.Checks = append(a.Checks, Check{"required-checks", SevOK, strings.Join(contexts, ", ")})
    case !meta.AllowAutoMerge:
        a.Checks = append(a.Checks, Check{"required-checks", SevInfo, "none (auto-merge off)"})
    default:
        a.Checks = append(a.Checks, Check{"required-checks", SevFail, "auto-merge on with no required check"})
    }
}
```

Classic contexts come from `required_status_checks.contexts` on the protection payload;
ruleset contexts from `.parameters.required_status_checks[].context` on the effective
rules. Union them.

**I would ship this check with no auto-fix.** The repair needs the repo's own CI job
names, which live in `.github/workflows/*.yml` and would mean parsing workflow YAML —
more than any other vidette probe does, and a judgement call (nowcast has four jobs;
requiring all four is a decision, not a default). Report it and let the operator run the
PUT. The value is in seeing the gap, not in closing it unattended.

## Explicit non-goal

Not asking vidette to evaluate whether the required jobs are the *right* jobs, to run or
read CI, or to author workflow files. The question is "does a merge have to wait for
something," which is pure settings state, deterministic, no model. Keep the boundary
where the 2026-05-26 alerts note put it.

## Why it fits vidette's model

- Both probes are `gh api` reads through the existing `ghAPI` `(body, exists, err)`
  triple — deterministic, reproducible, no key beyond `gh`.
- Request 1 is a straight correctness fix: vidette currently reports a false FAIL and
  offers a repair for a problem that isn't there.
- Request 2 closes the same shape as the alerts finding — two independent toggles read
  as one, where each reads OK alone and the pair is the actual state.
