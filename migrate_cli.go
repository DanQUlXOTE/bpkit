package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"bpkit/internal/bpclient"
	"bpkit/internal/migrate"
	"bpkit/internal/profiles"
)

// migrateOptions is the headless migrator, driven by flags instead of the
// TUI. Everything it needs lives in internal/migrate, which has no UI of
// its own.
type migrateOptions struct {
	toProject   string // destination project id, reusing the source's server and key
	toProfile   string // destination profile, for another server or key
	configs     string // comma-separated names; empty means all
	placeholder string // fill credentials with this; empty means skip them
	usePlace    bool   // --placeholder-credentials was passed
	apply       bool
	assumeYes   bool
}

// runMigrate performs a migration without the TUI. It is a dry run unless
// --apply is passed, and even then it asks before writing.
func runMigrate(store *profiles.Store, active profiles.Profile, src *bpclient.Client, o migrateOptions, in io.Reader, out io.Writer) error {
	if src == nil {
		return fmt.Errorf("no source connection: use --profile <name>, or set --server/$BINDPLANE_URL and an API key")
	}
	dst, dstName, err := migrateDestination(store, active, src, o)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Migrating %s → %s\n\n", sourceName(active, src), dstName)

	ctx := context.Background()
	names, err := migrateConfigNames(ctx, src, o.configs)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return fmt.Errorf("no configurations to migrate")
	}
	fmt.Fprintf(out, "Reading %d configuration(s) and everything they depend on...\n", len(names))

	plan, err := migrate.Build(ctx, src, dst, names)
	if err != nil {
		return err
	}

	// Credentials the source would not disclose. Either fill them with a
	// placeholder, or leave those resources out; applying them unresolved
	// would simply be refused.
	var filled []*migrate.Secret
	var skipped []string
	if o.usePlace {
		filled = plan.FillPlaceholders(o.placeholder)
	} else {
		skipped = plan.SkipNeedingCredentials()
	}

	fmt.Fprintln(out)
	plan.RenderPlan(out)
	migrate.RenderSkipped(out, skipped)

	apply, _, _ := plan.Counts()
	if !o.apply {
		fmt.Fprintf(out, "\nDRY RUN — nothing was sent. Pass --apply to migrate these %d resource(s).\n", apply)
		migrate.RenderCredentials(out, filled, o.placeholder)
		return nil
	}
	if apply == 0 {
		fmt.Fprintln(out, "\nNothing to apply.")
		return nil
	}

	if !o.assumeYes {
		ok, err := confirm(in, out, fmt.Sprintf(
			"\nApply %d resource(s) to %s. Type the destination name to confirm: ", apply, dstName), dstName)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("confirmation did not match; nothing was applied")
		}
	}

	fmt.Fprintln(out, "\nApplying...")
	results, applyErr := plan.Apply(ctx, dst)
	fmt.Fprintln(out)
	migrate.RenderResults(out, results)
	migrate.RenderCredentials(out, filled, o.placeholder)
	if applyErr != nil {
		return applyErr
	}
	return nil
}

// migrateDestination resolves where the migration writes to.
func migrateDestination(store *profiles.Store, active profiles.Profile, src *bpclient.Client, o migrateOptions) (*bpclient.Client, string, error) {
	switch {
	case o.toProfile != "" && o.toProject != "":
		return nil, "", fmt.Errorf("pass --to-profile or --to-project, not both")

	case o.toProfile != "":
		if store == nil {
			return nil, "", fmt.Errorf("no profile store available")
		}
		p, err := store.Get(o.toProfile)
		if err != nil {
			return nil, "", err
		}
		if strings.EqualFold(p.Name, active.Name) && p.Project == active.Project {
			return nil, "", fmt.Errorf("the destination must differ from the source")
		}
		c, err := p.Connect()
		return c, p.Title(), err

	case o.toProject != "":
		if o.toProject == src.Project() {
			return nil, "", fmt.Errorf("the destination project must differ from the source")
		}
		// Same server and key, a different project. This is what an
		// org-scoped key is for.
		return src.WithProject(o.toProject), o.toProject, nil
	}
	return nil, "", fmt.Errorf("choose a destination with --to-project <id> or --to-profile <name>")
}

func sourceName(active profiles.Profile, src *bpclient.Client) string {
	if active.Name != "" {
		return active.Title()
	}
	if p := src.Project(); p != "" {
		return p
	}
	return src.BaseURL()
}

// migrateConfigNames is the named configurations, or every one in the source.
func migrateConfigNames(ctx context.Context, src *bpclient.Client, list string) ([]string, error) {
	if strings.TrimSpace(list) != "" {
		var out []string
		for _, n := range strings.Split(list, ",") {
			if n = strings.TrimSpace(n); n != "" {
				out = append(out, n)
			}
		}
		return out, nil
	}
	configs, err := src.ListResources(ctx, bpclient.KindConfiguration)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range configs {
		if n := c.Name(); n != "" {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// confirm asks the user to type want before a write.
func confirm(in io.Reader, out io.Writer, prompt, want string) (bool, error) {
	fmt.Fprint(out, prompt)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return false, err
	}
	return strings.TrimSpace(line) == want, nil
}
