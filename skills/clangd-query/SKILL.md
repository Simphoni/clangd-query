---
name: clangd-query
description: Query C++ codebases with clangd-query to find symbols, definitions, references, inheritance hierarchies, signatures, and public interfaces. Use this instead of grep/find when exploring C++ code, understanding a class or function, or checking the impact of a change.
---

# clangd-query

Use `clangd-query` for semantic C++ code exploration. It talks to a per-project clangd daemon and returns concise, token-efficient results with `file:line:column` locations.

Assume the binary is already in `PATH`. Run commands from anywhere inside the project tree; the tool finds the project root automatically.

## When to use it

- Find where a symbol is declared or defined: `search`, `show`
- See every reference before editing a function/class: `usages`
- Understand inheritance relationships: `hierarchy`
- Inspect function overloads: `signature`
- Inspect the public API of a class: `interface`
- Read the implementation of a symbol: `view` or `show`

Do not use `grep`/`find` for these tasks when a C++ project is available; clangd-query understands namespaces, overloads, templates, and inheritance.

## Batch queries

The symbol commands (`search`, `show`, `view`, `usages`, `hierarchy`, `signature`, `interface`) accept multiple symbols in one invocation. Each symbol runs as an independent query and the results are printed under stable section headers:

```bash
clangd-query show GameObject Transform Renderer
clangd-query usages GameObject::Update RenderSystem::Draw
```

```text
=== show GameObject ===
...

=== show Transform ===
...
```

Use batch form when you already know several independent queries; it saves one tool-call round trip per query. Keep the single-symbol form when the next query depends on the previous result.

A query that cannot find its symbol prints a "no symbols found" note inside its own section without affecting other sections or the exit code. A hard failure (bad location, timeout, daemon error) is likewise confined to its section, but makes the command exit non-zero so failures are never silent.

## Command reference

### `search`

Find symbols by name. Fuzzy matching works, but no regex, wildcards, or multi-word queries.

```bash
clangd-query search GameObject
clangd-query search Update --limit 10
```

Output includes a location and kind for each match, for example:

```text
- `game_engine::GameObject` at include/core/game_object.h:26:7 [class]
```

### `show`

Show declaration and definition source code for a symbol.

```bash
clangd-query show GameObject::Update
clangd-query show Transform
```

For functions, `show` prints both the declaration and the definition. Output is fenced code blocks preceded by locations.

### `view`

Show complete source code of a class or function, omitting large middle sections for brevity.

```bash
clangd-query view GameObject
```

### `usages`

Find all references to a symbol. Accept either a symbol name or a `file:line:column` location.

```bash
clangd-query usages GameObject
clangd-query usages src/game.cpp:45:10
```

Use this before modifying a function, class, or variable to understand its impact.

### `hierarchy`

Show what a class inherits from and what inherits from it.

```bash
clangd-query hierarchy GameObject
```

### `signature`

Show function signatures and overloads.

```bash
clangd-query signature Update
```

### `interface`

Show the public methods and members of a class, including their doc comments when available.

```bash
clangd-query interface Engine
```

### Operational commands

```bash
clangd-query status       # daemon status, build/index paths, indexing state
clangd-query logs         # daemon logs; add --verbose for detail
clangd-query shutdown     # stop the daemon (normally automatic)
```

## First run and indexing

The first query in a project starts a background daemon, generates or locates `compile_commands.json`, and builds the clangd index. Initial indexing of a large project can take minutes.

During indexing, code queries print progress to stderr and retry automatically, for example:

```text
clangd still indexing (elapsed 6s); waiting 3s...
```

This is expected, not a failure. The command eventually returns the complete result. `clangd-query status` reports `Indexing: yes/no`, `Index Elapsed`, `Build Dir`, and `Index Dir`.

## Project configuration

Projects with custom build setups can declare their compilation database in `.clangd-query.json` at the project root:

```json
{
  "compileCommands": "build/compile_commands.json",
  "generate": "cmake --preset local",
  "autoReconfigure": true,
  "reconfigureDelay": "30s"
}
```

- `compileCommands`: path to an existing `compile_commands.json`, reused as-is.
- `generate`: shell command run only when the database is missing.
- `autoReconfigure`: re-run `generate` after C++ files are added or removed, useful for `file(GLOB_RECURSE)` CMake projects.
- `reconfigureDelay`: debounce for auto-reconfigure, Go duration syntax.

Newly created files are searchable immediately before any reconfigure: the daemon opens them in clangd directly.

## Best practices for agents

1. Start with `search` to discover a symbol.
2. Use `show` or `view` to read its implementation.
3. Use `usages` before changing behavior to find every call site.
4. Use `hierarchy` for inheritance questions and `interface` for public API questions.
5. Search accepts only a symbol name, not regex; for a method, prefer `Namespace::Class::Method` to disambiguate.
6. If results look incomplete, check `clangd-query status` to confirm `Indexing: no`; retry after indexing finishes rather than assuming a bug.
7. Prefer `--limit` on broad searches to keep output small.
8. When you have already decided on several independent lookups, combine them into one multi-symbol invocation to reduce round trips.

## Common workflows

Understand a class:

```bash
clangd-query search GameObject
clangd-query show GameObject
clangd-query hierarchy GameObject
clangd-query interface GameObject
```

Understand a method before editing it:

```bash
clangd-query search Update
clangd-query show GameObject::Update
clangd-query usages GameObject::Update
```

Find implementations across overloads:

```bash
clangd-query signature Update
clangd-query search Update --limit 20
```
