// Command bpkit is a terminal toolkit for the Bindplane API. It holds the
// connection profiles; the extensions do the work.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"bpkit/internal/bpclient"
	"bpkit/internal/ext"
	"bpkit/internal/exts/cloner"
	"bpkit/internal/exts/labeler"
	"bpkit/internal/exts/migrator"
	"bpkit/internal/plan"
	"bpkit/internal/profiles"
	"bpkit/internal/shell"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		server, csvPath, query, selector, version, profileName string
		apiKeyEnv, user, pass, project                         string
		apply, skipTLS                                         bool
	)
	flag.StringVar(&server, "server", os.Getenv("BINDPLANE_URL"), "Bindplane server URL (env: $BINDPLANE_URL). Passing --server explicitly overrides the default profile")
	flag.StringVar(&profileName, "profile", "", "Saved profile to connect with (default: the profile marked default; manage profiles in the TUI with p)")
	flag.StringVar(&project, "project", os.Getenv("BINDPLANE_PROJECT"), "Project to scope requests to, sent as X-Bindplane-Account-ID (env: $BINDPLANE_PROJECT)")
	flag.StringVar(&apiKeyEnv, "api-key-env", "BINDPLANE_API_KEY", "Env var holding the Bindplane API key")
	flag.StringVar(&user, "user", os.Getenv("BINDPLANE_USER"), "Basic-auth username (alternative to API key)")
	flag.StringVar(&pass, "pass", os.Getenv("BINDPLANE_PASSWORD"), "Basic-auth password")
	flag.BoolVar(&skipTLS, "skip-tls-verify", false, "Skip TLS certificate verification (lab only)")

	flag.StringVar(&query, "query", "", "Labeler: initial collector query, e.g. os:linux")
	flag.StringVar(&selector, "selector", "", "Labeler: initial label selector, e.g. env=prod")
	flag.StringVar(&version, "version", "", "Labeler: initial current-version filter, e.g. 1.60.0 or 1.60")
	flag.StringVar(&csvPath, "csv", "", "Labeler: CSV of hostname/agent-id and labels. Runs headless instead of the TUI")
	flag.BoolVar(&apply, "apply", false, "With --csv: actually apply label changes (default is a dry run). The TUI always starts as a dry run and asks for confirmation")

	flag.Usage = usage
	flag.Parse()

	// Carry profiles over from bp_labeler the first time bpkit runs.
	if moved, err := profiles.MigrateLegacy(); err == nil && moved {
		fmt.Fprintln(os.Stderr, "note: imported your bplabeler profiles")
	}

	flagCreds := false
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "server", "user", "pass", "api-key-env", "project":
			flagCreds = true
		}
	})
	store, active, client, err := connect(profileName, flagCreds,
		profiles.Profile{Server: server, APIKey: os.Getenv(apiKeyEnv), Project: project, SkipTLS: skipTLS},
		user, pass)
	if err != nil {
		return err
	}

	// Headless: label from a CSV without opening the TUI.
	if csvPath != "" {
		if client == nil {
			return fmt.Errorf("no credentials: use --profile <name>, or set --server/$BINDPLANE_URL and an API key")
		}
		return runCSV(client, csvPath, apply)
	}

	exts := []ext.Extension{
		labeler.Ext{Query: query, Selector: selector, Version: version},
		cloner.Ext{},
		migrator.Ext{},
	}

	// A bare extension id as the first argument opens straight into it.
	if args := flag.Args(); len(args) > 0 {
		if _, ok := findExt(exts, args[0]); !ok {
			return fmt.Errorf("unknown tool %q (try one of: %s)", args[0], extIDs(exts))
		}
		exts = reorder(exts, args[0])
	}

	model := shell.New(store, active, client, exts)
	_, err = tea.NewProgram(model, tea.WithAltScreen()).Run()
	return err
}

func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprintln(out, "bpkit — a terminal toolkit for the Bindplane API.")
	fmt.Fprintln(out, "\nUsage:\n  bpkit [flags] [tool]")
	fmt.Fprintln(out, "\nTools:")
	fmt.Fprintln(out, "  label     Bulk set/remove labels, assign a fleet, upgrade collectors")
	fmt.Fprintln(out, "  clone     Clone a template Windows Event source once per CSV row")
	fmt.Fprintln(out, "  migrate   Copy configurations and their dependencies to another project")
	fmt.Fprintln(out, "\nWith no tool, bpkit opens its menu. Connections are saved as profiles;")
	fmt.Fprintln(out, "press p on the menu, or ctrl+p inside a tool, to manage them.")
	fmt.Fprintln(out, "\nFlags:")
	flag.PrintDefaults()
}

func findExt(exts []ext.Extension, id string) (ext.Extension, bool) {
	for _, e := range exts {
		if strings.EqualFold(e.Meta().ID, id) {
			return e, true
		}
	}
	return nil, false
}

func extIDs(exts []ext.Extension) string {
	ids := make([]string, 0, len(exts))
	for _, e := range exts {
		ids = append(ids, e.Meta().ID)
	}
	return strings.Join(ids, ", ")
}

// reorder puts the named extension first so the menu opens on it.
func reorder(exts []ext.Extension, id string) []ext.Extension {
	out := make([]ext.Extension, 0, len(exts))
	if e, ok := findExt(exts, id); ok {
		out = append(out, e)
	}
	for _, e := range exts {
		if !strings.EqualFold(e.Meta().ID, id) {
			out = append(out, e)
		}
	}
	return out
}

func runCSV(client *bpclient.Client, path string, apply bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	rows, warns, err := plan.ReadCSV(f)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	ctx := context.Background()
	agents, err := client.ListAgents(ctx, "", "")
	if err != nil {
		return err
	}
	p := plan.ForCSV(rows, agents)
	p.Warnings = append(warns, p.Warnings...)

	if apply {
		fmt.Println("APPLY")
	} else {
		fmt.Println("DRY RUN (no changes made; pass --apply to apply)")
	}
	fmt.Println(strings.Repeat("-", 40))
	p.Render(os.Stdout)
	if !apply {
		return nil
	}

	failed := false
	for _, r := range p.Execute(ctx, client) {
		if r.Err != nil {
			failed = true
			fmt.Fprintf(os.Stderr, "FAILED %d collector(s) %v: %v %v\n", r.Agents, r.Labels, r.Err, r.Failed)
			continue
		}
		fmt.Printf("OK %d collector(s) updated with %v\n", r.Updated, r.Labels)
	}
	if failed {
		return fmt.Errorf("some label updates failed")
	}
	return nil
}

// connect picks the connection, in order: --profile; credentials passed as
// command-line flags; the saved default profile; credentials from the
// environment. With none of those, client is nil and bpkit opens the profile
// manager.
func connect(profileName string, flagCreds bool, creds profiles.Profile, user, pass string) (*profiles.Store, profiles.Profile, *bpclient.Client, error) {
	var store *profiles.Store
	if path, err := profiles.DefaultPath(); err == nil {
		store = profiles.NewStore(path)
	} else if profileName != "" {
		return nil, profiles.Profile{}, nil, err
	}
	fromProfile := func(p profiles.Profile) (*bpclient.Client, error) {
		return bpclient.New(bpclient.Options{
			BaseURL: p.Server, APIKey: p.APIKey, Project: p.Project, SkipTLSVerify: p.SkipTLS,
		})
	}
	fromCreds := func() (*profiles.Store, profiles.Profile, *bpclient.Client, error) {
		p := creds
		p.Name = "environment"
		c, err := bpclient.New(bpclient.Options{
			BaseURL: p.Server, APIKey: p.APIKey, User: user, Pass: pass,
			Project: p.Project, SkipTLSVerify: p.SkipTLS,
		})
		return store, p, c, err
	}
	haveCreds := creds.Server != "" && (creds.APIKey != "" || user != "")

	if profileName != "" {
		p, err := store.Get(profileName)
		if err != nil {
			return nil, profiles.Profile{}, nil, err
		}
		c, err := fromProfile(p)
		return store, p, c, err
	}
	if flagCreds && haveCreds {
		return fromCreds()
	}
	if store != nil {
		if name, err := store.Default(); err == nil && name != "" {
			if p, err := store.Get(name); err == nil {
				if c, err := fromProfile(p); err == nil {
					return store, p, c, nil
				}
			}
		}
	}
	if haveCreds {
		return fromCreds()
	}
	return store, profiles.Profile{}, nil, nil
}
