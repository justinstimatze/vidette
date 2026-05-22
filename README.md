# vidette

> An audit tool for the slow, dignified collapse of your GitHub repositories.

I have looked at the fleet, and I tell you: there is no harmony here. There is only the harmony of overwhelming and collective decay. The repositories drift, each at its own indifferent pace, toward a state where no `LICENSE` file is found, where the `dependabot.yml` is configured on some but not others, where the descriptions are blank as the eye of an unfertilized egg. The repository that nobody touches has been red in CI for many months. It does not protest. It does not even understand it is red. It simply continues, choking quietly on its own forgotten state, in the indifferent silence of the cloud.

This is the condition of the solo maintainer. You did not ask for forty repositories. They accumulated, the way a forest accumulates: not by plan, but by the slow uncontested fact of growth. Each one was useful, once. Some are useful still. But to maintain forty of anything is to encounter the obscene reality that maintenance is not a one-time act. It is a permanent condition, like weather, and it does not care that you are tired.

vidette is a small instrument for surveying this condition. It is not optimistic. It does not believe the fleet can be saved. It only believes the fleet can be looked at, honestly, and then, in the cases where the failure is mechanical and stupid, repaired — not because repair is meaningful, but because the alternative is the slow asphyxiation of letting it continue.

## What it does

There are four commands. Each performs a small, specific act of attention.

```
vidette audit     # walk the fleet. Note what is broken. Emit a report.
vidette plan      # propose repairs for what is broken. Do nothing yet.
vidette apply     # execute the proposed repairs. Mutate GitHub.
vidette suggest   # for what cannot be repaired by rule alone:
                  # write the question down. Hand it to your Claude session.
```

`audit` walks every repository in your fleet and runs roughly a dozen checks against each one. The checks are tier-aware. It will not nag a `scratch` repository for the absence of a `LICENSE`. It will not enforce branch protection on a repository you have already, with full intention, archived. It writes its findings to standard output in markdown. You may pipe this somewhere, or read it directly. It will tell you what it sees. It will not tell you what to feel about what it sees.

`plan` examines those findings and constructs a fix-plan: a list of mechanical actions vidette can perform on your behalf. License files inferred from the modal license of your other repositories. Empty descriptions filled with the first prose line of the README. Topics mined from the backtick-quoted identifiers the author already curated by formatting them as code. The plan is purely descriptive. Nothing happens. You read it. You consider it. You decide whether it is correct.

`apply` does what `plan` proposed. It writes the LICENSE. It PATCHes the description. It opens a pull request when a branch is protected, because vidette respects the institutions you have constructed against yourself. Each action is announced before it executes. At the end of the run, a rollup tells you, repository by repository, what was changed.

`suggest` is for the questions that cannot be answered by code. To classify a repository as `active` or `maintained` or `scratch`, vidette would need judgment — to read the README, to look at the rhythm of recent commits, to weigh tone and frequency and intent. vidette does not have judgment. Your Claude session does. So `suggest` writes a structured prompt to standard output describing what needs to be decided, and exits. Your Claude session — the one in which you ran the command — reads the prompt, considers it, and decides. Then, if you ask it to, it writes the answer back into your config.

## How vidette does its work

There are two ways. Most of the time, vidette does the work itself. `audit` and `apply` are pure Go: deterministic, reproducible, requiring no API key and no model. The inference is heuristic, not language-modelled — a license picked by the mode of your fleet, topics mined from the texture of the README. This is what ships. This is what you can trust to behave the same way tomorrow as it did today.

When a question genuinely requires judgment, vidette refuses to pretend it has judgment. It does not call an LLM API on your behalf. It does not choose your model for you. It writes the question to standard output as a prompt, and hands the prompt to the Claude session you are already running. That session has your context, your model preferences, your authentication, your trust posture, and the entirety of whatever else you were working on. It is, in every sense, better-equipped to decide than a small Go program shelling out to a borrowed API.

This is the architecture: vidette is the structured input; your Claude session is the runtime. If you build tools that want LLM help but do not want to *be* an LLM client, this is the pattern. Take it.

## Quickstart

```bash
git clone https://github.com/justinstimatze/vidette
cd vidette
go build -o vidette .

# Make sure gh is logged in
gh auth status

# Write your fleet inventory
$EDITOR vidette.yml

# Look first
./vidette audit > /tmp/report.md
$PAGER /tmp/report.md

# Plan
./vidette plan

# If the plan is acceptable, execute it
./vidette apply
```

## Configuration

`vidette.yml` lives next to the binary. Per-repository intent is expressed along three axes: how the repository relates to its upstream, what tier of attention it deserves, and whether you wish vidette to ignore it entirely.

```yaml
user: your-github-username

defaults:
  fork_relation: own              # own | rewritten | tracking | snapshot | profile
  tier: active                    # active | maintained | demo | scratch | archived
  include_private: true           # also audit private repos (default true)
  copyright_holder: Your Name     # written into generated LICENSE files; falls
                                  # back to `git config user.name`, then to `user`
  security_email: you@example.com # written into generated SECURITY.md; falls
                                  # back to `git config user.email`
  enforce_admins: false           # branch-protection: also bind admins?
                                  # default false (solo workflows need recovery paths)
  merge_strategy: squash          # auto-merge strategy: squash | rebase | merge

repos:
  my-active-service:   { tier: active }
  my-shipped-library:  { tier: maintained }
  my-poc-demo:         { tier: demo, notes: "not finished, returning soon" }
  my-hackathon-2014:   { tier: archived }

  upstream-fork:
    fork_relation: tracking
    tier: maintained
    notes: "track upstream; do not push back"

  username:
    fork_relation: profile
    tier: maintained

  some-repo:
    tier: active
    homepage: pending      # suppress homepage check until the deploy is live
```

Every field under `defaults:` is optional. `vidette` will infer reasonable values from `git config` and the GitHub user where it can.

**Privacy note on `include_private`.** Default is `true` — vidette will list, audit, and (with `apply`) mutate private repos. If you're running this against a corporate or shared GitHub account, set `include_private: false` or use the local-overlay pattern below to scope which repos vidette knows about.

**fork_relation** decides which checks fire at all:
- `own` / `rewritten` — the full inventory of hygiene applies
- `tracking` — only the upstream-sync check
- `snapshot` — vidette will skip it, recognizing you are not maintaining it
- `profile` — the special case of your `username/username` profile readme

**tier** decides how loud the silence around a repository is allowed to be:
- `active` — every check fires. CI, dependabot, auto-merge, branch protection
- `maintained` — only safety checks: license, branch protection, security policy
- `demo` — discoverability checks (description, topics) but no nag about dependabot
- `scratch` — almost everything skipped. A throwaway is permitted to be a throwaway
- `archived` — everything skipped except the bidirectional check on archive-state itself

**ignore: true** removes the repository from audit altogether, for the cases where vidette has nothing useful to say — vendored code, NDA repositories, configurations you handle by hand.

**Local overlay.** If `vidette.local.yml` exists beside `vidette.yml`, vidette merges it at load. Entries in the overlay override or extend the base. Use this to keep private-repository entries out of the committed yml. The overlay should be gitignored.

## What is checked

For an `own`, `active` repository, vidette performs the following acts of attention.

| check | what the fix looks like |
|---|---|
| license | Write `LICENSE`, inferred from the modal SPDX identifier of your fleet |
| default-branch | Detect non-`main`, surface for rename |
| description | PATCH the description with the first prose line of the README |
| topics | PUT topics: language stack plus README backtick-mined identifiers, capped at 8 |
| dependabot | Write `.github/dependabot.yml`, ecosystem detected from the repository's languages |
| auto-merge | PATCH `allow_auto_merge=true` |
| branch-protection | PUT minimal protection: block force-push, block deletion. Nothing more |
| security-policy | Write `SECURITY.md`, with maintainer email and a 90-day disclosure window |
| archive-state | Bidirectional drift detection: tier=archived but live, or archived but tier ≠ archived |
| ci | The latest push-event workflow run: flagged when failing or growing stale |
| homepage | Fires only when notes mention a URL, or when `homepage:` is set explicitly |
| upstream-sync | For tracking forks: how many commits behind the parent's default branch |

When a file write hits HTTP 409 because of branch protection or repository rulesets, vidette falls back transparently to a `vidette/<slug>-<unix>` feature branch, opens a pull request, and enables auto-merge with squash. Subsequent runs are idempotent — if a vidette PR is already open for that file, vidette does not open a second one.

## Status

Alpha. The author runs vidette on his own fleet — approximately forty repositories. New checks emerge organically as new cases of drift are noticed.

What works: every check listed above. The PR fallback for branch-protected files. Idempotent re-runs (existing `vidette/*` PRs are reused, not duplicated). Crypto-random suffix on PR branches so parallel applies for distinct files don't collide. The local overlay. The plan/apply split. The session-handoff pattern in `suggest`. Rate-limit detection on the `gh api` wrapper, with a single sleep-and-retry. Graceful SIGINT during both audit and apply — in-flight work completes, no new work starts, the partial result tells you what happened. Strict YAML loading (typo'd config keys are a hard error, not a silent default) and enum validation on `tier` / `fork_relation`. A small unit-test suite covering the regex helpers, inference functions, and config validation. Code hosts (`github.com`, `gitlab.com`, `npmjs.com`, etc.) in notes are excluded from homepage inference, so "forked from github.com/x" doesn't accidentally PATCH a homepage to GitHub.

What is open:
- The `suggest` loop does not yet close automatically — your Claude session decides, but you still write the answer back into your config by hand. Eventually the loop should close on its own.
- vidette only knows GitHub. The same patterns would apply to GitLab, Gitea, Codeberg. The API client would need to be rewritten.
- vidette is a snapshot. Drift over time — week-on-week deltas — is not surfaced anywhere. It could be.
- No CI of its own. `go test ./...` and `gofmt -l .` are the bar.

## License

MIT. See `LICENSE`.
