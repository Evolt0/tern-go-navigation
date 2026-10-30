# Go Navigation — a Tern plugin

**English** | [简体中文](README.zh-CN.md)

Navigate Go code inside **Tern's native editor**. Find definitions and references together, search your project for functions and other symbols, preview the matching source line, and jump to the exact location. No replacement editor and no forced saves.

When installed alongside the updated `java-navigation` package, `jvm-navigation-host.luau` loads its shared native frontend. **Ctrl+Alt+G / S / B stay unchanged** for Go, Java and Kotlin; one window owns the picker and jump history. Go still uses this package's backend. Both packages must include the shared-frontend integration; run `tern plugin reload` after installing, updating or removing either package. No global keybind settings are changed.

![Definitions and references grouped in Tern's native picker, with source-line previews](assets/navigation-preview.png)

![Jumping to a Go function declaration in the native editor](assets/definition-jump.png)

![Searching the current project for functions, including files that are not open](assets/project-symbol-search.png)

## Highlights

- **One navigation command** — definitions first, references next, in one deduplicated list.
- **Project-wide symbol search** — find functions, methods, types, and package-level variables without opening their files first. Dependencies, the standard library, and unrelated modules stay out of the results.
- **Preview before jumping** — each result shows its file, line, column, source line, and path.
- **Unsaved buffers work** — queries and previews use the current window's editor content without modifying or saving source files.
- **Prewarmed, persistent gopls** — loading starts in the background after focusing a Go file. Navigation and search share the same language server throughout the editor process's lifetime.
- **Jump back safely** — keep a per-window jump history and reject stale navigation after the cursor or captured buffers change.
- **Go workspaces and Unicode** — support `go.mod`, `go.work`, tabs, CJK characters, emoji, and combining characters.

## Install

### Requirements

- **Tern**, with the `tern` command available. Tested with `0.9.7 (c2f7563)`.
- **The Go toolchain and gopls**, even when using a prebuilt release. gopls needs Go to load and analyze your project. The examples use the tested gopls version `v0.21.1`.
- **Git and GitHub SSH access** only if installing from source.

Release packages cover **macOS (`darwin-arm64`, `darwin-amd64`)** and **Linux (`linux-amd64`, `linux-arm64`)**. `amd64` means x86-64 and works on both Intel and AMD CPUs. CI runs the backend tests and packaged CLI on native runners for each target; Tern desktop integration has been checked on macOS arm64. Windows is not supported by the current Unix-socket/process implementation.

The plugin source requires Go 1.23 or newer. Installing gopls may download a newer toolchain automatically according to gopls's requirements. Local verification used Go 1.27.3.

### Release package — prebuilt plugin backend

Download **`go-navigation-<version>-<target>.tar.gz`** and its **`.tar.gz.sha256`** checksum from [Releases](https://github.com/Evolt0/tern-go-navigation/releases). Choose your target below. Use these named assets, not GitHub's automatically generated “Source code” archives.

| System | CPU | Target |
| --- | --- | --- |
| macOS | Apple Silicon | `darwin-arm64` |
| macOS | Intel | `darwin-amd64` |
| Linux | Intel / AMD x86-64 | `linux-amd64` |
| Linux | ARM64 / AArch64 | `linux-arm64` |

For version `1.3.2`, choose the target from the table and run these commands in the download directory:

```sh
# Still required for semantic Go navigation:
go install golang.org/x/tools/gopls@v0.21.1

target=darwin-arm64 # Replace with your target from the table.
archive="go-navigation-1.3.2-${target}.tar.gz"
if [ "$(uname -s)" = Darwin ]; then
  shasum -a 256 -c "$archive.sha256"
else
  sha256sum -c "$archive.sha256"
fi &&
tar -xzf "$archive" &&
tern plugin install ./go-navigation &&
tern plugin list
```

The archive contains a complete plugin directory, including `bin/tern-go-navigation`, both READMEs, screenshots, the changelog, and third-party license notices. `tern plugin install` copies it into Tern's plugin directory, so the extracted download can be removed afterward.

If you already have a **linked source installation**, unlink it before switching to a release package:

```sh
tern plugin unlink go-navigation
tern plugin install ./go-navigation
```

Prebuilt binaries are not notarized. If macOS blocks execution, review the system's security prompt or build from source; the plugin does not disable system security checks.

### From source

The repository is named `tern-go-navigation`, so Git already creates a local directory with that name. No extra destination-directory argument is needed. The clone URL uses SSH:

```sh
git clone git@github.com:Evolt0/tern-go-navigation.git
cd tern-go-navigation

go install golang.org/x/tools/gopls@v0.21.1
go build -o bin/tern-go-navigation .

tern plugin link "$PWD"
tern plugin list
```

Look for `go-navigation` with a `ready` status. Open a `.go` file in your project and press **Ctrl+Option+G**.

A linked installation runs directly from the checkout: **do not delete or move that directory**. Installing only the Git URL does not build the backend automatically; use the build-and-link steps above.

### Update

**Release installation:** download and verify the new release, extract it into a clean directory, then replace the installed copy:

```sh
tern plugin install ./go-navigation --force
```

`--force` does not replace a linked source installation. Unlink that installation first if switching installation methods.

**Linked source installation:** run inside the checkout:

```sh
git pull --ff-only
go build -o bin/tern-go-navigation .
tern plugin reload go-navigation
```

The next query uses the rebuilt backend; restarting Tern is not required.

### Uninstall

For a copied release installation:

```sh
tern plugin remove go-navigation
```

For a linked source installation:

```sh
tern plugin unlink go-navigation
```

Closing a file or project tab while its Tern process remains alive retains the cached client. The plugin helper and its gopls forwarding client are reclaimed when their owning Tern process exits, or when the backend executable is replaced or removed. Other clients, including OMP, keep their shared daemon alive; gopls exits it by default one minute after the last client disconnects. Standalone CLI helpers without a Tern window context still expire after five idle minutes. Your source checkout, Go installation, and gopls installation are not removed automatically.

## Keys and navigation

Place the cursor on a Go function, type, or variable for navigation:

| Key on macOS | Action | Command palette |
| --- | --- | --- |
| **Ctrl+Option+G** | Find definitions and references together | `Go: Navigate symbol` |
| **Ctrl+Option+S** | Search symbols in the current project | `Go: Search project symbols` |
| **Ctrl+Option+B** | Return to the previous jump location | `Go: Jump back` |
| **↑ / ↓** | Select a result | — |
| **Enter** | Jump to the selected result | — |
| **Esc** | Dismiss the picker | — |

Definitions precede references; duplicate locations are removed. A single navigation result opens directly. References include calls, variable reads/writes, and type uses—not only function calls.

Source previews prefer unsaved buffers. They trim leading indentation and trailing whitespace while preserving whitespace inside the code. Previews reuse source text already read by the query; they do not issue extra gopls requests or file reads. Long rows are truncated by Tern's native picker.

Jump history lives in the current window and is cleared on plugin reload. Custom keybindings can target `plugin.go-navigation.navigate`, `plugin.go-navigation.symbols`, and `plugin.go-navigation.back`. The older separate `definition` and `references` actions have been removed.

## Search functions in your project

1. Open any local `.go` file in the project. The cursor does not need to be on a function name.
2. Press **Ctrl+Option+S**, enter a name such as `FindCustomer`, and press Enter to search.
3. Inspect the symbol name, source preview, and file location, then press Enter to jump. **Ctrl+Option+B** returns to the origin.

Search uses gopls `workspace/symbol` with fuzzy matching. It includes unopened files and unsaved changes in the current window. Its scope is the current module or the modules explicitly enrolled in `go.work`, including members outside the workspace directory. Standard-library symbols, external dependencies, vendor directories, unenrolled nested modules, and symlink escapes are excluded.

Tern's native prompt submits a query on Enter; it does not query the server on every keystroke. Typing in the result picker filters the results already returned. gopls v0.21.1 returns at most 100 top-ranked symbols, so use a more specific query when needed. This is symbol search, not full-text search over comments or function bodies.

## Indexes and caching

- **Live sessions:** keep independent buffers per window, workspace, and toolchain environment while connecting compatible clients to the native shared gopls daemon. A Go file triggers a background warmup attempt; short idle periods do not discard the editor's session. Queries synchronize changed files and buffers rather than rescanning the entire project.
- **Disk cache:** use gopls's own persistent package summaries, cross-reference indexes, and symbol indexes. On macOS the default location is under `~/Library/Caches/gopls`; `GOPLSCACHE` changes the cache root. A new process can reuse cached data but still needs to load workspace metadata.
- **No stale-result cache:** navigation and search ask the same continuously updated language service. Dependency-directory discovery remains necessary at initialization and after module configuration changes to preserve observation of external dependency edits.

References: [VS Code symbol navigation](https://code.visualstudio.com/docs/editing/editingevolved#open-symbol-by-name), [Zed's Go/gopls support](https://zed.dev/docs/languages/go), and [gopls persistent indexes and incremental updates](https://go.dev/blog/gopls-scalability).

### Sharing gopls with OMP

The current source backend runs `gopls -remote=auto serve`. It connects to the native daemon when available and starts one when needed, so navigation works without OMP. OMP and the editor retain separate unsaved buffers and Go environments while sharing reusable caches.

Merge this override into OMP's active user `lsp.json` (normally `~/.omp/agent/lsp.json`), preserving any other server entries:

```json
{
  "servers": {
    "gopls": {
      "args": ["-remote=auto", "serve"]
    }
  }
}
```

In each already-running OMP session, reload its LSP configuration with `action: "reload", file: "*"`, or reopen that OMP session. Existing clients retain their previous launch arguments until reloaded. Both sides must use a compatible gopls build and the same runtime directory (`XDG_RUNTIME_DIR`, or the system temporary directory).

Activity Monitor can still show several `gopls` entries: the shared daemon, lightweight forwarding clients, and Go telemetry helpers. Workspace views remain independent, so memory savings depend on the amount of reusable cache. Release packages include this mode starting with `v1.3.1`; OMP needs the configuration above to participate.

Version `1.3.2` restores gopls's default diagnostic scheduling and completes LSP session shutdown before disconnecting. The previous one-hour diagnostic delay could retain snapshots during shutdown and prevent the daemon from reaching its idle timeout. After upgrading from `v1.3.1`, restart the old shared daemon once to discard already-delayed work, then reload LSP in OMP and rerun navigation as needed. Closing a project tab still retains the plugin session while its Tern process is alive.



## Tool discovery and troubleshooting

The backend finds gopls through `GOPLS`, then `PATH`, then `$HOME/go/bin/gopls`.

It preserves a Go executable already on `PATH`. When a GUI-launched process cannot find Go, it checks `$GOROOT/bin`, `/opt/homebrew/bin`, `/usr/local/go/bin`, and `/usr/local/bin`, adding only the selected directory to the gopls child environment. It does not modify your system environment.

- **Cannot start gopls:** check `gopls version`; expose custom locations through `GOPLS` or `PATH` inherited by Tern.
- **Go toolchain not found:** check `go version`; expose nonstandard installations through Tern's `PATH` or `GOROOT`.
- **Workspace loading fails:** the error preserves the underlying gopls explanation instead of only reporting `no views`.
- **Source changes are not reflected in the plugin:** rebuild with `go build -o bin/tern-go-navigation .`, then run `tern plugin reload go-navigation`.

## Limitations

- **No Command+click on ordinary code symbols.** Tern 0.9.7's [public routing API](https://docs.stencil.so/tern/guides/routing.html#link-requests) exposes link clicks, not ordinary-symbol clicks with document coordinates. A real desktop probe confirmed that distinction. No global mouse interception or accessibility-permission workaround is installed.
- **Previews are inline source lines**, not a separate selected-item preview pane. The native picker does not expose such a pane or a selection-change callback.
- Only local files are supported; remote-host files receive an explicit unsupported message.
- Unsaved `go.mod` and `go.work` changes are not overlays; save module configuration changes first.
- First-time loading still costs time, especially for large projects. Requests have a 45-second deadline. A query issued before warmup finishes still waits; cancellation or a language-server failure causes the affected session to be initialized again.

## Performance

These local 1.3.0 measurements compare the backend against a frozen copy of the plugin's source. Desktop measurements use a separate small Go example. They are not a guarantee for large repositories: the improvement moves loading ahead of interaction and avoids repeated cold starts, rather than claiming a faster already-warm query algorithm.

| Scenario | Time |
| --- | ---: |
| 1.2.1 first direct navigation | 1179.1 ms |
| 1.2.1 subsequent warm navigation | 6.0–6.9 ms |
| 1.3.0 background warmup | 971.5 ms |
| 1.3.0 first user navigation after warmup | 24.7 ms |
| 1.3.0 subsequent warm navigation | 6.3–7.9 ms |
| Desktop first navigation after automatic warmup | 34.8 ms |
| Desktop function-search submission to results | 54.4 ms |

## Development and verification

```sh
go test -race ./...
go build -o bin/tern-go-navigation .
tern plugin reload go-navigation
```

Integration tests require gopls and use temporary projects without network dependencies. They cover unsaved buffers, close/reopen transitions, previews, Unicode positions, immediate disk edits, external workspace modules, configuration changes, concurrent startup, atomic saves, editor lifetime, and symbol-search scope.

Real Tern desktop verification covers the combined picker, previews, jumps, history, stale-result rejection, automatic warmup, unopened-file searches, and unsaved renames. The screenshots above show the running plugin, not mockups.

`window.luau` integrates the native editor, warmup, and picker; `main.go` handles requests and positions; `daemon.go`, `session.go`, and `rpc.go` maintain language-server sessions; `symbols.go` implements scoped project search; `watch.go` synchronizes file changes.

## Publishing releases

**Yes—GitHub Actions packages and publishes releases automatically.** The workflow is [`.github/workflows/release.yml`](https://github.com/Evolt0/tern-go-navigation/blob/main/.github/workflows/release.yml).

On a pushed `vX.Y.Z` tag, it:

1. Requires the tag version to match `plugin.toml`.
2. Uses native macOS arm64, macOS amd64, Linux amd64, and Linux arm64 runners, installs Go and the pinned gopls, and runs the race-enabled tests on each.
3. Builds a fresh plugin backend, packages the runtime files and documentation, and generates a SHA-256 checksum.
4. Extracts the package and exercises the packaged binary against a real temporary Go project.
5. Publishes all four archives and their checksums to GitHub Releases after every target succeeds.

Only the publishing job receives `contents: write`; it uses GitHub's built-in token, so no personal access token is required. Manual **Run workflow** executions with an empty `release_tag` build downloadable Actions artifacts without publishing. Enable Actions in the repository if it is disabled.

For a new release, update `plugin.toml` and the changelog, commit and push those changes, then push a matching `vX.Y.Z` tag. Do not move or reuse a published tag.

To add missing platform assets to an existing release, run the updated workflow on `main` with that release tag:

```sh
gh workflow run release.yml --ref main -f release_tag=v1.3.0
```

This uses the selected tag's source with the current packaging tools, checks all four native targets, and uploads only absent asset names. Existing tags, release notes, and assets are preserved. Runtime corrections require a new version.

For local packaging, maintainers need **Go and Python 3.9+**; smoke verification also needs gopls and a host matching the archive target:

```sh
python3 scripts/package.py --target darwin-arm64 --output dist
python3 scripts/smoke_release.py dist/go-navigation-1.3.2-darwin-arm64.tar.gz
```

`--target` defaults to the native platform and accepts the four targets listed above. Cross-compilation is supported; executing the smoke requires the matching OS and architecture. `--source PATH` selects a separate source checkout; the default is this repository.

Consumers of the prebuilt package do not need Python or the plugin source. They still need Go and gopls at runtime.

## Changelog

See [Changelog](docs/reference/changelog.md) (Chinese).
