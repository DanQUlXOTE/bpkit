// Package migrate copies configurations and everything they depend on from
// one Bindplane project to another. It builds a plan first — what is new,
// what already matches, what collides, and which credentials the source
// would not disclose — so nothing reaches the destination unreviewed.
package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"bpkit/internal/bpclient"
)

// SensitivePlaceholder is what Bindplane returns in place of a sensitive
// parameter's value. Such a value cannot be migrated: the destination needs
// the real one.
const SensitivePlaceholder = "(sensitive)"

// Status is what applying an item would do to the destination.
type Status int

const (
	// StatusNew means the destination has no resource of this name.
	StatusNew Status = iota
	// StatusSame means the destination already has an identical resource.
	StatusSame
	// StatusConflict means the destination has a resource of this name that
	// differs from the source's.
	StatusConflict
	// StatusUnknown means the destination could not be read, so whether
	// applying would create or replace is not known.
	StatusUnknown
)

func (s Status) String() string {
	switch s {
	case StatusSame:
		return "identical"
	case StatusConflict:
		return "conflict"
	case StatusUnknown:
		return "unreadable"
	}
	return "new"
}

// Item is one resource the migration would apply.
type Item struct {
	Kind     bpclient.Kind
	Resource bpclient.Resource
	Status   Status
	// Existing is the destination's current version, when there is one.
	Existing bpclient.Resource
	// Diff names the spec fields that differ, for a conflict.
	Diff []string
	// Skip leaves this resource alone. Conflicts and identical resources
	// start skipped, so a plan applied without review cannot clobber
	// anything in the destination.
	Skip bool
	// Root is true for the configurations the user picked, as opposed to the
	// dependencies pulled in behind them.
	Root bool
	// TypeDef marks a resource type definition — a SourceType and friends.
	// These are server-managed: Bindplane seeds its built-in library itself,
	// and a freshly created project can still be loading it. They are never
	// applied unless asked for; see Build.
	TypeDef bool
	// Note explains an unusual status to the user.
	Note string
}

// Title is the item as shown in a list.
func (i Item) Title() string { return i.Resource.Title() }

// Secret is a parameter whose value the source would not disclose. It holds
// the live parameter map, so Set writes the value straight into the resource
// that will be applied.
type Secret struct {
	// Where locates the parameter for a human, e.g.
	// `destination "Splunk prod" → token`.
	Where string
	Param string
	Value string
	param map[string]any
}

// Set fills in the parameter.
func (s *Secret) Set(v string) {
	s.Value = v
	s.param["value"] = v
}

// Filled reports whether the parameter has a value yet.
func (s *Secret) Filled() bool { return s.Value != "" }

// Plan is a reviewed migration, ready to apply.
type Plan struct {
	Items    []Item
	Secrets  []*Secret
	Warnings []string
}

// Counts summarizes the plan by status, over the items that are not skipped.
func (p *Plan) Counts() (apply, skip, conflict int) {
	for _, it := range p.Items {
		if it.Skip {
			skip++
		} else {
			apply++
		}
		if it.Status == StatusConflict {
			conflict++
		}
	}
	return
}

// Unfilled returns the secrets still waiting for a value.
func (p *Plan) Unfilled() []*Secret {
	var out []*Secret
	for _, s := range p.Secrets {
		if !s.Filled() {
			out = append(out, s)
		}
	}
	return out
}

// Build walks the named configurations in src, collects everything they
// reference, and compares the result against dst.
func Build(ctx context.Context, src, dst *bpclient.Client, configNames []string) (*Plan, error) {
	p := &Plan{}
	w := &walker{
		ctx:    ctx,
		src:    src,
		seen:   map[string]bool{},
		byKind: map[bpclient.Kind][]bpclient.Resource{},
		roots:  map[string]bool{},
	}
	for _, name := range configNames {
		cfg, err := src.GetResource(ctx, bpclient.KindConfiguration, name)
		if err != nil {
			return nil, fmt.Errorf("read configuration %q: %w", name, err)
		}
		w.roots[name] = true
		w.add(bpclient.KindConfiguration, cfg)
	}
	p.Warnings = w.warnings

	// Dependencies before the configurations that need them.
	for _, kind := range bpclient.ResourceKinds {
		for _, r := range w.byKind[kind] {
			r.StripServerFields()
			// A resource read by a pinned reference must still be created
			// under its bare name, whatever the server echoed back.
			if meta, ok := r["metadata"].(map[string]any); ok {
				if n, _ := meta["name"].(string); n != "" {
					meta["name"] = bpclient.TrimVersion(n)
				}
			}
			p.Warnings = appendUnique(p.Warnings, unpin(r.Spec(), r.Title())...)
			item := Item{
				Kind:     kind,
				Resource: r,
				Root:     kind == bpclient.KindConfiguration && w.roots[r.Name()],
			}
			item.TypeDef = bpclient.IsResourceTypeKind(kind)
			compare(ctx, dst, &item)
			// Only brand-new resources apply by default, and never a type
			// definition: those are the server's to seed, and applying one
			// over the JSON API trips a Bindplane bug where an int
			// parameter default arrives as a float and template validation
			// rejects it.
			item.Skip = item.Status != StatusNew || item.TypeDef
			if item.TypeDef && item.Status == StatusNew {
				item.Note = "missing in the destination — built-in types are seeded by the server"
			}
			if item.Status == StatusUnknown {
				p.Warnings = append(p.Warnings, fmt.Sprintf("%s %q: %s", strings.TrimSuffix(string(kind), "s"), item.Title(), item.Note))
			}
			p.Items = append(p.Items, item)
		}
	}
	p.Secrets = findSecrets(p.Items)
	return p, nil
}

// compare fills in the item's status against the destination. A resource
// the destination does not have is new; one that cannot be read at all is
// left unknown rather than assumed absent, so a permissions or decoding
// problem never turns into an unintended create.
func compare(ctx context.Context, dst *bpclient.Client, item *Item) {
	existing, err := dst.GetResource(ctx, item.Kind, item.Resource.Name())
	if errors.Is(err, bpclient.ErrNotFound) {
		item.Status = StatusNew
		return
	}
	if err != nil {
		item.Status, item.Note = StatusUnknown, "could not read the destination: "+err.Error()
		return
	}
	item.Existing = existing
	// Compare like for like. Bindplane re-pins every reference and type to
	// its own current version on write (model/configuration.go:3706), so
	// the destination's copy of a resource we applied comes back pinned
	// while ours is bare. Without normalizing both sides, every resource
	// already migrated would report as a conflict against itself.
	cp := forDiff(existing)
	if diff := specDiff(cp, forDiff(item.Resource)); len(diff) == 0 {
		item.Status = StatusSame
	} else {
		item.Status, item.Diff = StatusConflict, diff
	}
}

// specDiff names the top-level spec fields that differ between two versions
// of a resource. It is a summary, not a line-by-line diff: it answers "what
// about this resource changed" at a glance.
func specDiff(a, b bpclient.Resource) []string {
	as, bs := a.Spec(), b.Spec()
	keys := map[string]bool{}
	for k := range as {
		keys[k] = true
	}
	for k := range bs {
		keys[k] = true
	}
	var out []string
	for k := range keys {
		if !jsonEqual(as[k], bs[k]) {
			out = append(out, k)
		}
	}
	// A renamed display name matters too, and lives outside the spec.
	if a.DisplayName() != b.DisplayName() {
		out = append(out, "displayName")
	}
	sort.Strings(out)
	return out
}

// forDiff returns a copy of r with the server's bookkeeping and all
// version pins removed, so two resources are compared on their substance.
func forDiff(r bpclient.Resource) bpclient.Resource {
	cp := bpclient.Resource(deepCopy(r))
	cp.StripServerFields()
	if meta, ok := cp["metadata"].(map[string]any); ok {
		if n, _ := meta["name"].(string); n != "" {
			meta["name"] = bpclient.TrimVersion(n)
		}
	}
	unpin(cp.Spec(), "")
	return cp
}

// appendUnique appends the items of add that out does not already hold.
// One configuration often uses the same type in several components, and
// repeating the same note per occurrence buries the rest of the plan.
func appendUnique(out []string, add ...string) []string {
	for _, s := range add {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func jsonEqual(a, b any) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(x) == string(y)
}

// Apply writes the plan's unskipped items to the destination, one kind at a
// time so dependencies land before the configurations that reference them.
func (p *Plan) Apply(ctx context.Context, dst *bpclient.Client) ([]Result, error) {
	byKind := map[bpclient.Kind][]bpclient.Resource{}
	for _, it := range p.Items {
		if it.Skip {
			continue
		}
		byKind[it.Kind] = append(byKind[it.Kind], it.Resource)
	}
	var out []Result
	for _, kind := range bpclient.ResourceKinds {
		rs := byKind[kind]
		if len(rs) == 0 {
			continue
		}
		_, err := dst.Apply(ctx, rs)
		out = append(out, Result{Kind: kind, Count: len(rs), Err: err})
		if err != nil {
			// A failed kind means later kinds would reference what is not
			// there, so stop rather than pile failures on failures.
			return out, err
		}
	}
	return out, nil
}

// Result is what applying one kind did.
type Result struct {
	Kind  bpclient.Kind
	Count int
	Err   error
}

// ------------------------------------------------------- dependency walk

type walker struct {
	ctx      context.Context
	src      *bpclient.Client
	seen     map[string]bool
	byKind   map[bpclient.Kind][]bpclient.Resource
	roots    map[string]bool
	warnings []string
}

// add records a resource, the type definition it is built on, and
// everything it references.
func (w *walker) add(kind bpclient.Kind, r bpclient.Resource) {
	key := string(kind) + "/" + r.Name()
	if r.Name() == "" || w.seen[key] {
		return
	}
	w.seen[key] = true
	w.byKind[kind] = append(w.byKind[kind], r)
	spec := r.Spec()
	if typ, _ := spec["type"].(string); typ != "" {
		w.needType(kind, typ)
	}
	w.follow(spec)
}

// componentFields are the places a spec lists other resources. A field
// mapped to "" has no REST collection of its own: such components only ever
// exist inline, so they are walked but never fetched.
var componentFields = map[string]bpclient.Kind{
	"sources":      bpclient.KindSource,
	"processors":   bpclient.KindProcessor,
	"destinations": bpclient.KindDestination,
	"extensions":   bpclient.KindExtension,
	"connectors":   "",
}

// follow walks a spec's component lists. A component is a reference to a
// library resource when it names one but carries no type of its own;
// otherwise it is defined inline and travels with its parent — though an
// inline component still needs its type definition, and can itself
// reference library processors.
func (w *walker) follow(spec map[string]any) {
	if spec == nil {
		return
	}
	for field, kind := range componentFields {
		list, _ := spec[field].([]any)
		for _, e := range list {
			c, _ := e.(map[string]any)
			if c == nil {
				continue
			}
			name, _ := c["name"].(string)
			typ, _ := c["type"].(string)
			if name != "" && typ == "" {
				if kind != "" {
					w.fetch(kind, name)
				}
				continue
			}
			// Inline: it carries its own definition, so only its type and
			// any nested references need pulling in.
			if typ != "" {
				w.needType(kind, typ)
			}
			w.follow(c)
		}
	}
}

// needType pulls in the type definition a component of this kind is built
// on. Without it the destination cannot render the configuration and
// rejects the apply with "unknown DestinationType: ...".
func (w *walker) needType(kind bpclient.Kind, typeRef string) {
	tk := bpclient.ResourceTypeKind(kind)
	if tk == "" {
		return
	}
	// Bindplane resolves a component's type by the bare name, so a
	// "googlecloud:3" reference and a "googlecloud" one are the same type.
	w.fetch(tk, bpclient.TrimVersion(typeRef))
}

// fetch reads one resource from the source. ref may carry a ":version"
// pin, which is kept for the read — the pinned version is the content the
// configuration actually uses — but dropped for deduplication, so a
// resource referenced at two versions is still fetched once.
func (w *walker) fetch(kind bpclient.Kind, ref string) {
	name := bpclient.TrimVersion(ref)
	key := string(kind) + "/" + name
	if w.seen[key] {
		return
	}
	r, err := w.src.GetResource(w.ctx, kind, ref)
	if err != nil {
		w.seen[key] = true
		w.warnings = append(w.warnings,
			fmt.Sprintf("could not read %s %q from the source: %v", strings.TrimSuffix(string(kind), "s"), ref, err))
		return
	}
	w.add(kind, r)
}

// unpin rewrites a spec's version-pinned references to bare names and
// reports each one it changed. It covers both kinds of pin:
//
//   - a library reference, "Aurepoll:17", naming a resource and version
//   - a component's type, "tcp:9", naming a type definition and version
//
// Bindplane resolves both pairs exactly, pin included — AsLibraryResource
// passes rc.Name through verbatim, and the render path passes spec.type
// through verbatim (model/configuration.go:3092). A version that exists in
// the source project rarely exists in the destination: a copied resource
// starts at version 1, and a seeded type sits at whatever version that
// server built. So a pin that travels unchanged fails to resolve, with
// "unknown Source: Aurepoll:17" or "unknown SourceType: tcp:9".
//
// Dropping the pin resolves to the destination's current version instead,
// which is the copy we just applied or the type that server already has.
func unpin(spec map[string]any, owner string) []string {
	if spec == nil {
		return nil
	}
	var out []string

	// The resource's own type, for a library resource.
	out = append(out, unpinType(spec, owner)...)

	for field := range componentFields {
		list, _ := spec[field].([]any)
		for _, e := range list {
			c, _ := e.(map[string]any)
			if c == nil {
				continue
			}
			name, _ := c["name"].(string)
			typ, _ := c["type"].(string)
			if name != "" && typ == "" {
				if bare := bpclient.TrimVersion(name); bare != name {
					c["name"] = bare
					out = append(out, fmt.Sprintf("%s: reference %q now tracks the destination's current version", owner, name))
				}
				continue
			}
			// Inline: unpin its type, then its own nested references.
			out = append(out, unpin(c, owner)...)
		}
	}
	return out
}

// unpinType drops a ":version" pin from a spec's own type reference.
func unpinType(spec map[string]any, owner string) []string {
	typ, _ := spec["type"].(string)
	if typ == "" {
		return nil
	}
	bare := bpclient.TrimVersion(typ)
	if bare == typ {
		return nil
	}
	spec["type"] = bare
	return []string{fmt.Sprintf("%s: type %q now tracks the destination's current version", owner, typ)}
}

// ------------------------------------------------------- secret detection

// findSecrets collects every parameter the source would not disclose, so the
// user can supply the real values before anything is applied.
func findSecrets(items []Item) []*Secret {
	var out []*Secret
	for _, it := range items {
		label := fmt.Sprintf("%s %q", strings.TrimSuffix(string(it.Kind), "s"), it.Title())
		out = append(out, scanParams(it.Resource.Spec(), label)...)
	}
	return out
}

// scanParams walks a spec's own parameters and those of its inline
// components, collecting redacted ones.
func scanParams(spec map[string]any, label string) []*Secret {
	if spec == nil {
		return nil
	}
	var out []*Secret
	params, _ := spec["parameters"].([]any)
	for _, p := range params {
		pm, _ := p.(map[string]any)
		if pm == nil {
			continue
		}
		if v, ok := pm["value"].(string); ok && v == SensitivePlaceholder {
			name, _ := pm["name"].(string)
			out = append(out, &Secret{Where: label, Param: name, param: pm})
		}
	}
	for field := range componentFields {
		list, _ := spec[field].([]any)
		for _, e := range list {
			c, _ := e.(map[string]any)
			if c == nil {
				continue
			}
			// Only inline components carry values worth scanning; a library
			// reference is scanned as its own item.
			if typ, _ := c["type"].(string); typ == "" {
				continue
			}
			name, _ := c["displayName"].(string)
			if name == "" {
				name, _ = c["type"].(string)
			}
			out = append(out, scanParams(c, label+" → "+name)...)
		}
	}
	return out
}

func deepCopy(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = deepCopyAny(v)
	}
	return out
}

func deepCopyAny(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopy(t)
	case []any:
		cp := make([]any, len(t))
		for i, e := range t {
			cp[i] = deepCopyAny(e)
		}
		return cp
	default:
		return v
	}
}
