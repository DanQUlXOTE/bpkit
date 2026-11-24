# bpkit

A terminal toolkit for the Bindplane API. bpkit owns the connections — saved
as profiles, switchable at any time — and the tools plug into it:

| Tool | What it does |
|------|--------------|
| **Collector labeler** | Search collectors, then bulk set or remove labels, assign a fleet, or upgrade to an available version. |
| **Source cloner** | Bulk-add Windows Event sources to a configuration by cloning a template source already in it, one per CSV row. |
| **Config migrator** | Copy configurations, and every source/processor/destination/extension they reference, into another project or organization. |

Nothing changes until you confirm. Every tool shows a dry run first and asks
you to type `confirm`. Headless CSV runs are a dry run unless you pass
`--apply`.

```
bpkit
Bindplane API toolkit · connected to acme-prod · proj-aaa

 1. Collector labeler      Bulk set/remove labels, assign a fleet, upgrade collectors
 2. Source cloner          Clone a template Windows Event source once per CSV row
 3. Config migrator        Copy configurations and their dependencies to another project

up/down move  •  enter or 1-9 open  •  p profiles  •  q quit
inside a tool: ctrl+b back to this menu, ctrl+p profiles
```

## Build

Requires Go 1.27.1 or newer (see `go.mod`). There is no cgo, so no C toolchain
is needed.

```bash
git clone https://github.com/DanQUlXOTE/bpkit.git
cd bpkit
go build -o bpkit .
./bpkit --help
```

To build for another platform, set `GOOS` and `GOARCH`. For example, a Linux
arm64 binary from a Mac:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o bpkit .
```

Use `GOOS=windows` and an `.exe` output name for Windows.

To run the tests:

```bash
go test ./...
```

## Profiles

A profile is one connection: a server, an API key, and optionally a project.
Run `./bpkit` with no credentials and it opens the profile manager. Press `p`
on the menu, or `ctrl+p` from inside any tool, to get back to it.

Profiles live in `$BPKIT_CONFIG`, or `<user config dir>/bpkit/profiles.json`,
written `0600` because it holds API keys. Keys are masked everywhere in the
UI. If you used `bp_labeler` before, its profiles are imported automatically
the first time bpkit runs.

The connection is chosen in this order:

1. `--profile <name>`
2. Credentials passed as flags (`--server`, `--project`, `--user`, `--pass`, `--api-key-env`)
3. The saved default profile (`s` in the profile manager sets it)
4. `$BINDPLANE_URL` + `$BINDPLANE_API_KEY` from the environment

### Projects

Bindplane scopes a request to a project with the `X-Bindplane-Account-ID`
header. Which profiles you need depends on how your API key is scoped:

- **A key that carries its own project** — leave `Project` empty. The server
  uses the key's project.
- **A key scoped to several projects** — set `Project`. Such a key is
  rejected with a 401 if the header is missing.

In the profile form, `l` lists the projects the key can reach and fills the
field in for you, so you never have to paste an ID.

**Two profiles that differ only by project are how the migrator copies
between projects.** For two different organizations, use two profiles with
different servers and keys.

## Collector labeler

Search by Bindplane query, label selector and current version, select
collectors, then apply changes.

- `space` toggles a collector, `a` selects all, `n` none.
- `enter` opens the change screen: labels to set (`env=prod,team=db`), label
  keys to remove, and a fleet to assign. The fleet label is set by choosing a
  fleet, not by typing `fleet=...`, because it keys on the fleet's resource
  name rather than its display name.
- `u` upgrades the selection to a version Bindplane offers.
- `v` previews. Nothing is sent until you type `confirm`.

### Headless CSV labeling

```bash
./bpkit --profile acme-prod --csv hosts.csv          # dry run
./bpkit --profile acme-prod --csv hosts.csv --apply  # apply
```

Column 1 is a hostname or agent ID. Column 2 is either `key=value` pairs, or —
when the file has a header row — bare values for the header's key.

## Source cloner

Pick a configuration, pick the Windows Event source to use as a template,
point it at a CSV, and enter the Windows credentials shared by every new
source. Sources that are not Windows Event sources are shown dimmed and
cannot be chosen.

The template's inline processor chain is preserved verbatim on every clone,
and v2 `routes` are copied, so each new source wires to the same destinations.

CSV format: a header row is required and must contain `hostname`. An optional
`name` column becomes the clone's display name. Every other column header is
treated as a parameter to override on the clone; a header matching no
parameter on the template produces a warning rather than an error.

```csv
hostname,name,channels
dc01.corp.example.com,DC01 events,Security
dc02.corp.example.com,DC02 events,Application
```

Then choose what happens to the result:

| Mode | What it does |
|------|--------------|
| `dry-run` | Writes the modified configuration to a local YAML file. Nothing touches the server. |
| `verify` | Applies it as a **new** configuration under a name you choose, to inspect in the UI first. |
| `update` | Applies it **in place**, replacing the original. |

`dry-run` output contains the Windows password in plaintext. The file is
written `0600`; delete it after use.

## Config migrator

Pick a destination profile, then the configurations to copy. bpkit walks each
one for the sources, processors, destinations and extensions it references and
pulls those along too — components defined inline travel with their
configuration, library resources are copied as resources in their own right.

It also reports the **type definitions** those resources are built on
(`SourceType`, `ProcessorType`, `DestinationType`, `ExtensionType`). A project
that lacks them rejects the apply with `unknown DestinationType: dev_null`,
because Bindplane cannot render a configuration whose component types it does
not know.

Type definitions always start **skipped**, because they are the server's to
provide: Bindplane seeds its built-in library itself, and a freshly created
project can still be loading it. If the plan reports missing types, the usual
answer is to wait for the destination to finish seeding and re-run. Applying a
built-in type over the REST API can also fail validation outright, with an
error like:

```
template: logs.receivers:22:10: executing "logs.receivers" at <gt .initial_lookback 0>:
  error calling gt: incompatible types for comparison: float64 and int
```

That is a Bindplane server bug, not something the client can work around: JSON
has no integer type, so an `int` parameter default arrives as a float, and the
apply-time validation path does not normalize it the way the render path does.
You can still opt a type in with `space` — useful for a type you authored
yourself — but expect built-ins to fail.

It then compares everything against the destination and reports each resource
as **new**, **identical**, or a **conflict**. Only new resources are selected
by default, so a plan applied without review cannot clobber anything. `space`
toggles one, `d` shows which fields differ, `o` takes every conflict at once.

Resources apply in dependency order — type definitions, then sources,
processors, destinations and extensions, then configurations — so nothing
references something that is not there yet.

### Version pins

Bindplane pins two kinds of reference to an exact version, and resolves both
pairs exactly:

- a **library reference**, `Aurepoll:17`, naming a resource and version
- a component's **type**, `tcp:9`, naming a type definition and version

Neither version usually exists in the destination — a copied resource starts
at version 1, and a seeded type sits at whatever version that server built —
so a pin that travelled unchanged would fail to render with
`unknown Source: Aurepoll:17` or `unknown SourceType: tcp:9`.

The migrator reads the pinned version from the source, since that is the
content the configuration actually uses, but writes every reference bare so it
tracks the destination's current version. Each pin it drops is listed in the
plan's warnings.

The trade-off is that a migrated configuration follows the destination's
current version rather than staying frozen, which is the only thing that can
work across projects.

### Credentials

Bindplane never discloses a sensitive parameter: it reads back as
`(sensitive)`. The migrator finds every one of them before applying and asks
you for the real value, naming the resource and parameter. You can skip them
and fix the destination afterwards, but the plan tells you how many are
unset before you confirm.

## Adding a tool

An extension is a bubbletea model plus a one-line description. It gets a
connected client and never has to think about credentials:

```go
type Ext struct{}

func (Ext) Meta() ext.Meta {
	return ext.Meta{
		ID:          "mytool",
		Title:       "My tool",
		Desc:        "What it does",
		Connections: 1, // 2 if it works across two projects
	}
}

func (Ext) New(c ext.Context) (tea.Model, tea.Cmd) {
	m := newModel(c.Client)
	return m, m.Init()
}
```

Return `ext.Leave` instead of `tea.Quit` so backing out returns to the menu
rather than exiting bpkit. `c.Store` and `c.Dial` are there for a tool that
needs a second connection, the way the migrator does. Add it to the list in
`main.go` and it appears on the menu and as `bpkit mytool`.

## License

MIT. See [LICENSE](LICENSE).
