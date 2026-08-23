# clangd-query

Agents tend to have a hard time exploring C++ codebases and waste a lot of tokens in their context window searching for declarations, definitions and source code. The header/source file structure of C-style languages does not help here.

The `clangd-query` tool helps agents explore C++ codebases using the clangd LSP for indexing. It provides commands to search for symbols, view implementations, find usages, and show class hierarchies. The output includes both source code and `file:line:column` locations that agents can use to navigate directly to the relevant code. See the examples below.

## Examples

### Searching for Symbols

```bash
# Find the file+line position of a symbol
$ clangd-query search GameObject
Found 7 symbols matching "GameObject":

- `game_engine::GameObject` at include/core/game_object.h:26:7 [class]
- `game_engine::GameObject::GameObject` at src/core/game_object.cpp:12:13 [constructor]
- `game_engine::Engine::game_objects_` at include/core/engine.h:120:44 [field]
- `game_engine::Engine::CreateGameObject` at src/core/engine.cpp:136:37 [method]
- `game_engine::Engine::DestroyGameObject` at src/core/engine.cpp:142:14 [method]
- `game_engine::Engine::GetGameObjects` at include/core/engine.h:94:51 [method]
- `game_engine::GameObject::~GameObject` at src/core/game_object.cpp:17:13 [constructor]
```

### Show the complete source code of a Class or function

``````bash
# Show the full source code of a class
$ clangd-query show GameObject
Found class 'game_engine::GameObject' (7 matches total, showing most relevant)

From include/core/game_object.h:26:7
```cpp
/**
 * @brief Base class for all game objects in the engine
 *
 * GameObject represents any entity in the game world. It can contain
 * multiple components that define its behavior and properties.
 */
class GameObject : public Updatable,
                   public Renderable,
                   public std::enable_shared_from_this<GameObject> {
 public:
  explicit GameObject(const std::string& name);
  virtual ~GameObject();

  // Updatable interface
  void Update(float delta_time) override;
  bool IsActive() const override { return active_; }

  // Renderable interface
  void Render(float interpolation) override;
  int GetRenderPriority() const override { return render_priority_; }
  bool IsVisible() const override { return visible_; }

  <<<< Middle part omitted for brevity  >>>>

 private:
  static uint64_t next_id_;

  uint64_t id_;
  std::string name_;
  bool active_ = true;
  bool visible_ = true;
  int render_priority_ = 0;
  Transform transform_;
  std::vector<std::shared_ptr<Component>> components_;
};
```
``````


``````bash
# Shows the declaration AND definition of a function in one invocation, together
# with the file locations.
$ clangd-query show GameObject::Update
Found method 'game_engine::GameObject::Update' (2 matches total, showing most relevant)

From include/core/game_object.h:34:8 (declaration)
```cpp
  // Updatable interface
  void Update(float delta_time) override;
```

From src/core/game_object.cpp:21:18 (definition)
```cpp
void GameObject::Update(float delta_time) {
  if (!IsActive()) {
    return;
  }

  // Call virtual update method
  OnUpdate(delta_time);

  // Update all components
  for (auto& component : components_) {
    if (component->IsActive()) {
      component->Update(delta_time);
    }
  }
}
```
``````


### Viewing Class Hierarchies
```bash
# Show inheritance hierarchy for a class
$ clangd-query hierarchy GameObject
Inherits from:
├── Renderable - include/core/interfaces.h:41
└── Updatable - include/core/interfaces.h:18

GameObject - include/core/game_object.h:26
└── Character - include/game/character.h:9
    ├── Enemy - include/game/enemy.h:9
    └── Player - include/game/player.h:11
```

### Find Usages of a Symbol

```bash
# Find where a symbol is used in a code base
$ clangd-query usages Transform::Translate
Selected symbol: game_engine::Transform::Translate
Found 3 references:

- include/core/transform.h:84:8
- src/components/rigidbody.cpp:45:13
- src/game/character.cpp:47:18
```

## Reading the public interface of a class or struct
```bash
# View all public methods of a class and their comments
$ clangd-query interface RenderSystem
class game_engine::RenderSystem - include/systems/render_system.h:15:7

Public Interface:

RenderSystem()

~RenderSystem()

bool Initialize(int width, int height, const std::string& title)
  Initializes the render system with the specified window dimensions and title.
  Returns true if the render system was successfully initialized, false
  otherwise. This must be called before any rendering operations can be
  performed.

void Shutdown()
  Shuts down the render system and releases all associated resources. After
  calling this, the render system must be reinitialized before use.

void Render(float interpolation)
  Renders all registered renderable objects to the screen. The interpolation
  factor is used for smooth rendering between physics updates, allowing visual
  positions to be interpolated for smoother motion.

void RegisterRenderable(Renderable* renderable)
  Registers a renderable object with the system. Once registered, the object
  will be drawn during each render pass until it is unregistered.

void UnregisterRenderable(Renderable* renderable)
  Unregisters a renderable object from the system. The object will no longer be
  drawn in subsequent render passes.

void SetActiveCamera(std::shared_ptr<Camera> camera)
  Sets the camera that will be used for rendering. All renderable objects will
  be transformed and projected using this camera's view and projection matrices.

std::shared_ptr<Camera> GetActiveCamera() const
  Returns the currently active camera used for rendering. May return nullptr if
  no camera has been set.

int GetWindowWidth() const
  Returns the current window width in pixels.

int GetWindowHeight() const
  Returns the current window height in pixels.
```


## Batch Queries

The symbol-based commands (`search`, `show`, `view`, `usages`, `hierarchy`, `signature`, `interface`) accept multiple symbols in a single invocation. Every positional argument runs as an independent query against the daemon, and the results are printed under stable section headers:

```bash
# Show three classes in one call
$ clangd-query show GameObject Transform Vector3
=== show GameObject ===
Found class 'game_engine::GameObject' (7 matches total, showing most relevant)
...

=== show Transform ===
Found class 'game_engine::Transform' ...
...

=== show Vector3 ===
...
```

Queries are executed sequentially over the existing daemon connection, so batching avoids process and connection overhead but still waits for each result in order. A query that finds no symbols prints a "no symbols found" note inside its own section; this is not treated as an error. A hard failure such as a malformed location or a timeout is likewise confined to its own `Error:` line so that the remaining queries always complete, but the process exits with a non-zero status and a summary on stderr whenever any query failed:

```text
Error: 1 of 2 queries failed
```

This keeps batch mode safe for scripted use: stdout always contains every successful result, while the exit code signals that something needs attention.


### The `batch` command

When the queries mix different commands, use `batch`. It reads one complete query per line from standard input (or from a file named as its optional argument) and executes each line independently, including per-line `--limit` values:

```bash
$ clangd-query batch <<'EOF'
# Understand the render pipeline before refactoring it
show RenderSystem
interface RenderSystem
usages Renderable --limit 20
hierarchy GameObject
EOF
=== show RenderSystem ===
...

=== interface RenderSystem ===
...
```

Blank lines and lines starting with `#` are skipped. Each line is parsed like a normal invocation: double-quoted arguments are supported, and unsupported lines (operational commands such as `status` or `shutdown` cannot be batched) produce an error in their own section without stopping the remaining queries. The exit code is non-zero when any line failed hard, with a summary on stderr.

## Requirements

- Go 1.21 or higher for building
- `clangd` must be installed on your system. Version 15+ recommended for full feature support
- CMake-based C++ project (for compile_commands.json generation). Other build systems work through a `.clangd-query.json` configuration file, see Project Configuration below.
- By default your C++ project must have `CMakeLists.txt` at the project root. The tool automatically detects your C++ project by looking for `CMakeLists.txt` or `.clangd-query.json` in parent directories. Run `clangd-query` from anywhere within your project tree.

## Installation

1. Clone this repository
2. Make sure you have Go 1.21+ installed
3. Build the binary:
   ```bash
   ./build.sh
   ```
4. The binary will be available at `bin/clangd-query`. Alternatively, you can use one of the prebuilt binaries in `bin/releases` (for macOS+Apple Silicon and Linux+Intel).
5. I highly recommend creating a `clangd-query` symlink in your project root to the compiled binary, then @-link your agents to the AGENT.md file in this repository for instructions on how to use the tool.

## Project Configuration

Projects with a custom build setup (CMake presets, cross-compilation toolchains, non-CMake generators) can place a `.clangd-query.json` file at their root to declare how the compilation database is produced:

```json
{
  "compileCommands": "build/compile_commands.json",
  "generate": "cmake --preset local",
  "clangdArgs": ["--query-driver=/usr/bin/arm-none-eabi-*"],
  "autoReconfigure": true,
  "reconfigureDelay": "30s"
}
```

- `compileCommands` (string): path to your `compile_commands.json`, relative to the project root or absolute. When the file already exists it is reused as-is, so you can point the tool at the build directory you configured yourself. clangd's index is stored in `.cache/clangd` inside that file's directory.
- `generate` (string, optional): shell command that produces the database. It runs via `sh -c` from the project root, and only when `compileCommands` does not exist yet. Regenerating an existing database is left to you: clangd watches the file and reloads changed compilation flags on its own, so re-running your configure command takes effect without restarting the daemon. Requires `compileCommands` to be set (so the daemon can verify the command's output).
- `clangdArgs` (array of strings, optional): extra arguments appended to the clangd invocation, e.g. `--query-driver` when the compilation database references a non-host compiler.
- `autoReconfigure` (boolean, optional): when true, the daemon watches for C++ source files being added or removed and re-runs `generate` after a quiet period, so build systems that compute their file lists dynamically (e.g. CMake's `file(GLOB_RECURSE)`) pick up the changed file set without a manual reconfigure. Requires `generate`. Note: a reconfigure writes into your build directory, so enable this only when you are not running concurrent builds in that same directory.
- `reconfigureDelay` (string, optional): how long after the last file addition or removal `generate` is re-run, in Go duration syntax (e.g. `"30s"`, `"1m"`). Defaults to `"30s"`. Only meaningful with `autoReconfigure`.

New files are searchable immediately even before any reconfigure: the daemon opens freshly created source files in clangd directly, which infers their compile flags from neighboring files. The auto-reconfigure exists to restore the authoritative compile_commands.json afterwards, not to make new files visible.

### Example: CMake Presets with Globbed Sources

A typical setup: the project globs its sources and configures through a preset, with the compilation database landing in `build/`:

```cmake
# CMakeLists.txt
file(GLOB_RECURSE armillary_src CONFIGURE_DEPENDS
    ${CMAKE_CURRENT_SOURCE_DIR}/src/*.cc)
add_executable(armillary ${armillary_src})
```

```json
{
  "compileCommands": "build/compile_commands.json",
  "generate": "cmake --preset local",
  "autoReconfigure": true
}
```

With this in place, the daemon reuses `build/compile_commands.json` from your own `cmake --preset local` runs instead of configuring its own build directory. When you (or an agent) add or delete a `.cc` file under `src/`, the new file's symbols are searchable right away, and about 30 seconds after the file operations settle the daemon re-runs `cmake --preset local` so the compilation database reflects the new file set. clangd picks up the regenerated database by itself.

All fields are optional and independent, except that `generate` requires `compileCommands`. Without a configuration file the built-in behavior applies (see Compilation Database below). Editing the file while the daemon is running is safe: the next command detects the change and restarts the daemon automatically. A malformed file is a hard error rather than a silent fallback to defaults.

The configuration file also acts as a project root marker: when searching ancestor directories for the project root, the first directory containing `.clangd-query.json` or `CMakeLists.txt` wins. A non-CMake project can therefore be served purely from a configuration file.

## Other Commands

```bash
# Check daemon status
clangd-query status

# Show all logs of the daemon. Use --verbose, --info (the default) or --error to
# filter on log entries.
clangd-query logs

# Quits the daemon process. This is not required as the daemon shutdown
# automatically after idling for too long.
clangd-query shutdown

# Shows help output of the tool.
clangd-query --help
```

### Technical Details

`clangd-query` is a command-line tool and not an MCP, as agents seem to have an easier time using command-line tools. It uses a client/server architecture to make it fast and keeps output to a minimum to save tokens.

On first run, `clangd-query` starts a background daemon for your project. The tool looks for `.clangd-query.json` or `CMakeLists.txt` in the current directory and all its ancestor directories. The first directory containing either file is used as the project root. It then ensures a `compile_commands.json` exists (see Project Configuration above for custom setups) and starts `clangd` to index the codebase. Daemon startup can take a while on a cold start; the client wait budget defaults to two minutes and can be adjusted with the `CLANGD_QUERY_STARTUP_TIMEOUT` environment variable (e.g. `30s`, `5m`).

Subsequent runs of the tool are fast as the daemon is already running. The daemon shuts down automatically after 30 minutes of being idle.

```
┌─────────────┐       JSON-RPC        ┌──────────────┐
│clangd-query ├──────────────────────►│clangd-daemon │
│  (client)   │◄──────────────────────┤  (server)    │
└─────────────┘    Unix Socket        └──────┬───────┘
                                             │
                                             ▼
                                       ┌──────────┐
                                       │  clangd  │
                                       │   LSP    │
                                       └──────────┘
```

#### Compilation Database

Without a `.clangd-query.json` configuration file, the tool builds a `compile_commands.json` from the `CMakeLists.txt`, which must be in the project root. The database is stored in `.cache/clangd-query/build/compile_commands.json`. With a configuration file, the location declared by `compileCommands` is used instead.

### Index
The clangd index is stored in a `.cache/clangd` directory inside the compilation database directory (`.cache/clangd-query/build/.cache/clangd` by default, or e.g. `build/.cache/clangd` when `compileCommands` points at `build/compile_commands.json`).

### Lock Files

The daemon uses a lock file `<project-root>/.clangd-query.lock`. These are automatically cleaned when the daemon shuts down.

### Daemon Log File
Stored in  `.cache/clangd-query/daemon.log`. Can also be directly accessed using the `clangd-query logs` command as long as the daemon is running.

## License

MIT License - see [LICENSE](LICENSE) file for details.

## In Development

This tool is under active development and suggestions and feedback is welcome.

## Acknowledgments

Built on top of the excellent [clangd](https://clangd.llvm.org/) language server.
