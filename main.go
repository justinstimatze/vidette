package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
)

// Version is set via -ldflags "-X main.Version=..." at build time. When
// unset, displayVersion() falls back to the VCS revision Go embeds in
// binaries automatically since 1.18, so `go install ...@latest` users still
// see a meaningful version string.
var Version = "dev"

func displayVersion() string {
	if Version != "dev" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		var rev, modified string
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				modified = s.Value
			}
		}
		if rev != "" {
			if len(rev) > 7 {
				rev = rev[:7]
			}
			if modified == "true" {
				rev += "+dirty"
			}
			return rev
		}
	}
	return "dev"
}

// verbose toggles per-repo progress logging to stderr. Off by default; opt in
// via -v on any subcommand.
var verbose bool

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "audit":
		cmdAudit(os.Args[2:])
	case "plan":
		cmdApply(os.Args[2:], false)
	case "apply":
		cmdApply(os.Args[2:], true)
	case "suggest":
		cmdSuggest(os.Args[2:])
	case "version", "--version", "-V":
		fmt.Println("vidette", displayVersion())
	case "-h", "--help", "help":
		usage()
	default:
		// Back-compat: bare `vidette` with no subcommand runs audit, so first
		// positional may be a flag.
		if os.Args[1] != "" && os.Args[1][0] == '-' {
			cmdAudit(os.Args[1:])
			return
		}
		fmt.Fprintf(os.Stderr, "vidette: unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `vidette %s — scout your GitHub repo fleet for config drift

usage:
  vidette audit   [-config FILE] [-o FILE] [-p N] [-repo A,B] [-v]  read-only audit, emit markdown
  vidette plan    [-config FILE] [-p N] [-repo A,B] [-v]            dry-run: show what apply would do
  vidette apply   [-config FILE] [-p N] [-repo A,B] [-v]            execute the plan (mutates GitHub)
  vidette suggest [-config FILE] [-v]                               emit per-repo evidence prompt for tier classification
  vidette version                                                   print version

flags:
  -repo  comma-separated repo names to scope to (default: whole fleet)
  -v     verbose: log per-repo progress to stderr

plan/apply only act on repos explicitly classified in the config; unconfigured
repos (in the fleet but not in vidette.yml) are audit-only until you classify them.
`, displayVersion())
}

func cmdAudit(args []string) {
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	configPath := fs.String("config", "vidette.yml", "path to config")
	outPath := fs.String("o", "", "output path (default: stdout)")
	parallel := fs.Int("p", 6, "parallel repo audits")
	repoFlag := fs.String("repo", "", "comma-separated repo names to scope to (default: whole fleet)")
	v := fs.Bool("v", false, "verbose: log per-repo progress")
	_ = fs.Parse(args)
	verbose = *v

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	_, audits, unconfigured, missing := loadAndAudit(ctx, *configPath, *parallel, splitRepoFlag(*repoFlag))

	out := os.Stdout
	if *outPath != "" {
		f, err := os.Create(*outPath)
		if err != nil {
			die("open output: %v", err)
		}
		defer f.Close()
		out = f
	}
	RenderReport(out, audits, unconfigured, missing)
}

func cmdSuggest(args []string) {
	fs := flag.NewFlagSet("suggest", flag.ExitOnError)
	configPath := fs.String("config", "vidette.yml", "path to config")
	v := fs.Bool("v", false, "verbose: log per-repo progress")
	_ = fs.Parse(args)
	verbose = *v

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		die("config: %v", err)
	}
	if cfg.User == "" {
		die("config: missing `user`")
	}
	includePrivate := cfg.Defaults.IncludePrivate == nil || *cfg.Defaults.IncludePrivate
	if err := RunSuggest(os.Stdout, cfg, includePrivate); err != nil {
		die("suggest: %v", err)
	}
}

func cmdApply(args []string, doApply bool) {
	name := "plan"
	if doApply {
		name = "apply"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	configPath := fs.String("config", "vidette.yml", "path to config")
	parallel := fs.Int("p", 6, "parallel repo audits")
	repoFlag := fs.String("repo", "", "comma-separated repo names to scope to (default: whole fleet)")
	v := fs.Bool("v", false, "verbose: log per-repo progress")
	_ = fs.Parse(args)
	verbose = *v

	// Install a SIGINT handler that lets the current in-flight fix complete
	// but stops vidette from starting new ones. RunApply checks ctx in its
	// per-action loop.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg, audits, unconfigured, _ := loadAndAudit(ctx, *configPath, *parallel, splitRepoFlag(*repoFlag))

	// plan/apply act only on repos explicitly classified in the config.
	// Repos matched only by `defaults:` (i.e. unconfigured — present in the
	// fleet but absent from vidette.yml) are audit-only: surfacing them is
	// audit's job, mutating them is not. This is the fail-safe that stops a
	// partial or empty config from fixing the whole fleet under default
	// own/active. Classify a repo to opt it into plan/apply.
	if len(unconfigured) > 0 {
		skip := make(map[string]bool, len(unconfigured))
		for _, r := range unconfigured {
			skip[r] = true
		}
		kept := audits[:0]
		for _, a := range audits {
			if skip[strings.TrimPrefix(a.Repo, cfg.User+"/")] {
				continue
			}
			kept = append(kept, a)
		}
		audits = kept
		fmt.Fprintf(os.Stderr, "%s: skipping %d unconfigured repo(s) (audit-only until classified in %s): %s\n",
			name, len(unconfigured), *configPath, strings.Join(unconfigured, ", "))
	}

	applied, failed := RunApply(os.Stdout, ctx, audits, cfg.Defaults, doApply)
	if doApply {
		fmt.Fprintf(os.Stderr, "\napplied: %d · failed: %d\n", applied, failed)
		if failed > 0 {
			os.Exit(1)
		}
	}
}

func loadAndAudit(ctx context.Context, configPath string, parallel int, repoFilter []string) (*Config, []RepoAudit, []string, []string) {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		die("config: %v", err)
	}
	if cfg.User == "" {
		die("config: missing `user`")
	}

	includePrivate := cfg.Defaults.IncludePrivate == nil || *cfg.Defaults.IncludePrivate
	fleet, err := ListUserRepos(cfg.User, includePrivate)
	if err != nil {
		die("list repos: %v", err)
	}

	// -repo scoping: restrict the fleet to an explicit, named subset. This is
	// the safe way to act on one or a few repos — unlike trimming the config,
	// which silently leaves every other repo at the defaults and (pre-#1) let
	// apply mutate the whole fleet.
	if len(repoFilter) > 0 {
		want := make(map[string]bool, len(repoFilter))
		for _, r := range repoFilter {
			want[strings.TrimSpace(r)] = true
		}
		filtered := fleet[:0]
		for _, repo := range fleet {
			if want[strings.TrimPrefix(repo, cfg.User+"/")] {
				filtered = append(filtered, repo)
			}
		}
		for _, repo := range filtered {
			delete(want, strings.TrimPrefix(repo, cfg.User+"/"))
		}
		for r := range want {
			fmt.Fprintf(os.Stderr, "warning: -repo %q not found in fleet — skipping\n", r)
		}
		fleet = filtered
	}

	unconfigured, missing := diffRepos(fleet, cfg, cfg.User)
	if len(repoFilter) > 0 {
		// "Configured but not in fleet" is computed against the (now narrowed)
		// fleet, so under -repo every other classified repo would falsely look
		// missing. The diagnostic only makes sense for a whole-fleet run.
		missing = nil
	}

	// Drop repos marked ignore:true before auditing — they stay "configured"
	// (so they don't show up in unconfigured), they just aren't probed.
	auditable := make([]string, 0, len(fleet))
	for _, repo := range fleet {
		short := strings.TrimPrefix(repo, cfg.User+"/")
		if cfg.Resolve(short).Ignore {
			if verbose {
				fmt.Fprintf(os.Stderr, "skipped %s (ignore: true)\n", repo)
			}
			continue
		}
		auditable = append(auditable, repo)
	}

	audits := make([]RepoAudit, len(auditable))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, repo := range auditable {
		// Cancellation gate: stop launching new goroutines after SIGINT.
		// In-flight goroutines finish on their own; this keeps the audit
		// from blowing through the rest of the fleet after a Ctrl+C.
		if err := ctx.Err(); err != nil {
			break
		}
		i, repo := i, repo
		short := strings.TrimPrefix(repo, cfg.User+"/")
		rc := cfg.Resolve(short)
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			audits[i] = AuditRepo(repo, rc)
			if verbose {
				fmt.Fprintf(os.Stderr, "audited %s\n", repo)
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "audit interrupted: %s — partial results follow\n", err)
	}
	return cfg, audits, unconfigured, missing
}

// splitRepoFlag parses the comma-separated -repo value into a clean name list.
// Empty input means "no filter" (whole fleet).
func splitRepoFlag(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "vidette: "+format+"\n", args...)
	os.Exit(1)
}
