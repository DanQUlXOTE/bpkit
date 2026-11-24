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
	"regexp"
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
	// item indexes the resource this parameter belongs to, so a secret on
	// a resource nobody is applying can be left alone.
	item int
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

// Pending returns the secrets that actually matter: those on a resource the
// plan is going to apply. A credential on a resource the destination already
// has is never sent, so asking for it is noise.
func (p *Plan) Pending() []*Secret {
	var out []*Secret
	for _, s := range p.Secrets {
		if s.item >= 0 && s.item < len(p.Items) && !p.Items[s.item].Skip {
			out = append(out, s)
		}
	}
	return out
}

// Unfilled returns the pending secrets still waiting for a value.
func (p *Plan) Unfilled() []*Secret {
	var out []*Secret
	for _, s := range p.Pending() {
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
			// Only brand-new resources apply by default. That includes a
			// missing type definition: the resources built on it cannot
			// render without it, so skipping one only moves the failure.
			item.Skip = item.Status != StatusNew
			if item.TypeDef && item.Status == StatusNew {
				item.Note = "missing in the destination — normally the server seeds this itself"
			}
			if item.Status == StatusUnknown {
				p.Warnings = append(p.Warnings, fmt.Sprintf("%s %q: %s", strings.TrimSuffix(string(kind), "s"), item.Title(), item.Note))
			}
			p.Items = append(p.Items, item)
		}
	}
	p.Secrets = findSecrets(p.Items)
	p.Warnings = appendUnique(p.Warnings, survivingPins(p.Items)...)
	return p, nil
}

// pinnedKey matches a "name" or "type" field whose value still carries a
// ":<version>" suffix.
var pinnedKey = regexp.MustCompile(`"(?:name|type)"\s*:\s*"([^"]+:\d+)"`)

// survivingPins reports version pins left anywhere in what is about to be
// applied. Everything unpin understands is already bare, so a hit means a
// resource shape it does not yet traverse — a pin that reaches the
// destination fails to resolve there, with "unknown ProcessorType:
// google_filter_regex:15" or similar. Reporting beats silently rewriting
// something whose structure we do not actually understand.
func survivingPins(items []Item) []string {
	var out []string
	for _, it := range items {
		if it.Skip {
			continue
		}
		data, err := json.Marshal(map[string]any(it.Resource))
		if err != nil {
			continue
		}
		seen := map[string]bool{}
		for _, m := range pinnedKey.FindAllStringSubmatch(string(data), -1) {
			if seen[m[1]] {
				continue
			}
			seen[m[1]] = true
			out = append(out, fmt.Sprintf(
				"%s still pins %q — bpkit could not reach it, so the destination may reject this; please report the shape",
				it.Title(), m[1]))
		}
	}
	return out
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
	if spec := cp.Spec(); spec != nil {
		// featureGate records where the server got a type from, not what it
		// is: the seeder stamps "base-sources" on a built-in
		// (store/store_helpers.go:734) and the apply endpoint stamps
		// "custom-resource-types" on anything sent over REST
		// (rest/api.go:3287). Copying a built-in type therefore changes its
		// provenance in the destination, and comparing the two would report
		// every type bpkit has migrated as a conflict against itself.
		delete(spec, "featureGate")
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

// Apply writes the plan's unskipped items to the destination, one kind at
// a time so dependencies land before the resources that reference them.
//
// Bindplane rejects an apply whole, so one bad resource would otherwise
// cost every sibling in its batch. When a batch fails, each resource is
// retried on its own and the failures are retried once more, in case one
// only failed because a sibling had not landed yet. Everything that can
// apply does, and every resource that cannot is reported by name.
func (p *Plan) Apply(ctx context.Context, dst *bpclient.Client) ([]Result, error) {
	byKind := map[bpclient.Kind][]bpclient.Resource{}
	for _, it := range p.Items {
		if it.Skip {
			continue
		}
		byKind[it.Kind] = append(byKind[it.Kind], it.Resource)
	}

	var out []Result
	var failed bool
	for _, kind := range bpclient.ResourceKinds {
		rs := byKind[kind]
		if len(rs) == 0 {
			continue
		}
		res := Result{Kind: kind, Count: len(rs)}
		if _, err := dst.Apply(ctx, rs); err == nil {
			res.Applied = len(rs)
			out = append(out, res)
			continue
		}
		res.Applied, res.Failures = applyIndividually(ctx, dst, rs)
		if len(res.Failures) > 0 {
			failed = true
		}
		out = append(out, res)
	}
	if failed {
		return out, fmt.Errorf("some resources could not be applied")
	}
	return out, nil
}

// applyIndividually applies each resource on its own, then retries the
// failures once so a resource that only needed a sibling to land first is
// not reported as broken.
func applyIndividually(ctx context.Context, dst *bpclient.Client, rs []bpclient.Resource) (applied int, failures []Failure) {
	var pending []bpclient.Resource
	errOf := map[string]error{}
	for _, r := range rs {
		if _, err := dst.Apply(ctx, []bpclient.Resource{r}); err != nil {
			pending = append(pending, r)
			errOf[r.Name()] = err
			continue
		}
		applied++
	}
	for _, r := range pending {
		if _, err := dst.Apply(ctx, []bpclient.Resource{r}); err != nil {
			failures = append(failures, Failure{Title: r.Title(), Err: err})
			continue
		}
		applied++
	}
	return applied, failures
}

// Failure is one resource the destination refused.
type Failure struct {
	Title string
	Err   error
}

// Result is what applying one kind did.
type Result struct {
	Kind bpclient.Kind
	// Count is how many resources of this kind the plan tried to apply,
	// Applied how many landed.
	Count   int
	Applied int
	// Failures names the resources the destination refused, with its reason.
	Failures []Failure
}

// OK reports whether every resource of this kind applied.
func (r Result) OK() bool { return len(r.Failures) == 0 }

// Explain turns a known server-side failure into something actionable,
// returning "" when there is nothing useful to add.
func (f Failure) Explain() string {
	if f.Err == nil {
		return ""
	}
	msg := f.Err.Error()
	switch {
	case strings.Contains(msg, "incompatible types for comparison: float64 and int"),
		strings.Contains(msg, "incompatible types for comparison: int and float64"):
		return "This is a Bindplane bug, not a problem with the copy. A type's int " +
			"parameter default arrives as a float over the JSON API, and the apply-time " +
			"validation does not convert it back the way rendering does. Built-in types " +
			"are normally seeded by the server, so the fix is to let the destination " +
			"finish seeding rather than to copy this type."
	case strings.Contains(msg, "unknown ProcessorType"),
		strings.Contains(msg, "unknown SourceType"),
		strings.Contains(msg, "unknown DestinationType"),
		strings.Contains(msg, "unknown ExtensionType"):
		return "The destination has no such type definition. Go back and include the " +
			"matching type in the plan, then apply again."
	case strings.Contains(msg, "unknown Source"),
		strings.Contains(msg, "unknown Destination"),
		strings.Contains(msg, "unknown Processor"):
		return "The destination has no such resource. It was probably skipped in the " +
			"plan; include it and apply again."
	case strings.Contains(msg, "masked (sensitive) values that cannot be restored"):
		return "Bindplane will not accept the \"(sensitive)\" placeholder the source hands " +
			"out in place of a credential. Go back to the credentials screen and enter a " +
			"real value for this resource, or exclude it from the plan — leaving it unset " +
			"does not create a half-configured resource, it creates none at all."
	case strings.Contains(msg, "parameter value for"):
		return "The copy is faithful: this is the value stored in the source being " +
			"rejected by the destination. Bindplane validates on save, not on read, so a " +
			"value saved before its rule existed survives in the source and only fails " +
			"when something writes it again. Fix the parameter in the source and migrate " +
			"again, or exclude this resource from the plan with space. Nothing else of " +
			"this kind was applied, since the whole batch is rejected together."
	}
	return ""
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

// eachComponent calls visit for every component in spec, at any depth.
func eachComponent(spec map[string]any, visit func(kind bpclient.Kind, c map[string]any)) {
	if spec == nil {
		return
	}
	for field, kind := range componentFields {
		for _, c := range componentList(spec, field) {
			visit(kind, c)
			// A bundle holds components of its own.
			eachComponent(c, visit)
		}
	}
}

// componentList returns the components a spec stores under field. They live
// in one of two places: spec.<field>, and — for a processor bundle saved
// before Bindplane moved them — a parameter named <field> whose value is the
// list (model/configuration.go:3722). A bundle in the older shape keeps its
// children out of spec.processors entirely, so a traversal that only reads
// spec.<field> never sees them, and the types they need never travel.
func componentList(spec map[string]any, field string) []map[string]any {
	var out []map[string]any
	add := func(v any) {
		list, _ := v.([]any)
		for _, e := range list {
			if c, ok := e.(map[string]any); ok {
				out = append(out, c)
			}
		}
	}
	add(spec[field])

	params, _ := spec["parameters"].([]any)
	for _, p := range params {
		pm, _ := p.(map[string]any)
		if pm == nil {
			continue
		}
		if n, _ := pm["name"].(string); n == field {
			add(pm["value"])
		}
	}
	return out
}

// isReference reports whether a component points at a library resource
// rather than carrying its own definition. An inline component always has a
// type; a reference names a resource and has none.
func isReference(c map[string]any) (name string, ok bool) {
	name, _ = c["name"].(string)
	typ, _ := c["type"].(string)
	return name, name != "" && typ == ""
}

// follow pulls in everything a spec's components need: the library
// resources they reference, and the type definitions they are built on.
func (w *walker) follow(spec map[string]any) {
	eachComponent(spec, func(kind bpclient.Kind, c map[string]any) {
		if name, ok := isReference(c); ok {
			if kind != "" { // a kind with no REST collection is always inline
				w.fetch(kind, name)
			}
			return
		}
		if typ, _ := c["type"].(string); typ != "" {
			w.needType(kind, typ)
		}
	})
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
	// The resource's own type, for a library resource.
	out := unpinType(spec, owner)

	eachComponent(spec, func(_ bpclient.Kind, c map[string]any) {
		if name, ok := isReference(c); ok {
			if bare := bpclient.TrimVersion(name); bare != name {
				c["name"] = bare
				out = append(out, fmt.Sprintf("%s: reference %q now tracks the destination's current version", owner, name))
			}
			return
		}
		out = append(out, unpinType(c, owner)...)
	})
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
	for i, it := range items {
		label := fmt.Sprintf("%s %q", strings.TrimSuffix(string(it.Kind), "s"), it.Title())
		found := scanParams(it.Resource.Spec(), label)
		for _, s := range found {
			s.item = i
		}
		out = append(out, found...)
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
