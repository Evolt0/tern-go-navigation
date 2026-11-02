# Go Navigation — a Tern plugin

[English](README.md) | **简体中文**

在 Tern **原生 Go 编辑器**中，一次查看符号的定义与调用 / 引用，或按名称搜索当前项目的函数、方法、类型和变量，预览源码后跳转。不替换编辑器，不要求先保存文件。

与更新后的 `java-navigation` 插件同时安装时，`jvm-navigation-host.luau` 会加载它提供的共享原生前端：Go / Java / Kotlin 都使用 **Ctrl+Alt+G / S / B**，共用窗口内的选择器和跳转历史；Go 仍调用本插件后端。两边都需包含共享前端更新，安装、更新或移除任一插件后执行 `tern plugin reload`。不改写全局键位配置；未安装 Java / Kotlin 插件时行为不变。

![Tern 原生选择器：定义与调用分组，文件行列和命中行代码预览](assets/navigation-preview.png)

![选择定义后跳转到 Go 源文件，光标定位到函数声明](assets/definition-jump.png)

![在当前项目中按名称搜索函数，查找尚未打开文件里的定义](assets/project-symbol-search.png)

## 功能亮点

- **一个入口** — 一次查询定义与引用，在同一列表分组展示；无需分别执行两个命令。
- **项目内函数查询** — 输入名称即可搜索项目中未打开文件里的函数、方法、类型和变量，排除标准库、第三方依赖和未加入工作区的嵌套模块。
- **先看代码，再跳转** — 每条结果显示文件名、行列和命中行代码，完整路径放在代码后面。
- **支持未保存修改** — 查询和预览使用当前窗口的编辑器缓冲区，不隐式保存或修改源文件。
- **支持选中标识符** — 双击函数名或正向 / 反向选择名字时，查询选区起点，不把位于选区末尾的光标误当成标识符位置。
- **提前加载并保留索引** — 聚焦本地 Go 文件后尝试后台预热；编辑器进程存活期间保留 gopls，不再因空闲 5 分钟退出。导航和项目搜索共用同一服务。
- **可以返回** — 保留窗口内跳转历史；光标、选区或缓冲区改变后，不应用过期的跳转结果。
- **Go 工作区与 Unicode** — 支持 `go.mod`、`go.work`，处理 Tab、中文、emoji 和组合字符的位置转换。

## 环境要求

- **Tern**，且终端中可以运行 `tern` 命令。已验证 `0.9.7 (c2f7563)`。
- **Go 工具链和 gopls**：即使使用预编译插件，也必须在运行时安装，插件二进制不替代它们。本文固定使用已验证的 `gopls v0.21.1`。
- **从源码构建插件**最低要求 Go 1.23；安装 `gopls v0.21.1` 可能需要更新的 Go 工具链，Go 可按其要求自动下载。本机验证版本为 Go 1.27.3。
- **Git** 仅在克隆和更新源码时需要；SSH 克隆要求已经配置 GitHub SSH 访问。
- **Python 3.9 或更新版本**仅供维护者打包和运行发布包冒烟检查，普通用户运行插件或从源码构建 Go 后端不需要 Python。

发布包提供 **macOS arm64、macOS amd64、Linux amd64、Linux arm64**。`amd64` 即 x86-64，同时适用于 Intel 和 AMD 处理器。CI 在各平台的原生 runner 上运行后端测试与发布包 CLI 冒烟检查；Tern 桌面集成已在 macOS arm64 验证。当前后端依赖 Unix socket 和进程信号，不支持 Windows。macOS 发布二进制未经 Apple 公证；无法使用发布包时可选择从源码构建。

先安装运行时语言服务：

```sh
go install golang.org/x/tools/gopls@v0.21.1
```

## 安装

### 预编译 Release 安装

发布入口：[GitHub Releases](https://github.com/Evolt0/tern-go-navigation/releases)。按系统和处理器选择附件：

| 系统 | 处理器 | target |
| --- | --- | --- |
| macOS | Apple Silicon | `darwin-arm64` |
| macOS | Intel | `darwin-amd64` |
| Linux | Intel / AMD x86-64 | `linux-amd64` |
| Linux | ARM64 / AArch64 | `linux-arm64` |

以下以 `1.3.3` 为例。下载 `go-navigation-1.3.3-<target>.tar.gz` 及相邻的 `.tar.gz.sha256`，放入同一目录后执行：

```sh
target=darwin-arm64 # 按上表替换为你的平台。
archive="go-navigation-1.3.3-${target}.tar.gz"
if [ "$(uname -s)" = Darwin ]; then
  shasum -a 256 -c "$archive.sha256"
else
  sha256sum -c "$archive.sha256"
fi &&
tar -xzf "$archive" &&
tern plugin install ./go-navigation &&
tern plugin list
```

**只有校验成功后才解压和安装**。校验文件使用归档文件的 basename，不依赖维护者本机路径；SHA-256 用于检查下载内容是否与校验文件一致，并不是代码签名或 Apple 公证。

归档根目录是 `go-navigation/`，其中包含 `plugin.toml`、`window.luau`、可执行文件 `bin/tern-go-navigation`、中英文 README、`assets/`、`docs/reference/changelog.md` 和第三方许可证声明，不包含 Go 源码、构建缓存或 Git 元数据。此方式复制安装插件，安装成功后不需要保留下载和解压目录。

若已有链接安装，必须先执行 `tern plugin unlink go-navigation`，再进行复制安装；不要直接用覆盖参数替代解除链接。

### 从源码安装

源码仓库不包含后端二进制，也没有首次使用时自动构建的逻辑。克隆、编译后端，然后链接插件：

```sh
git clone git@github.com:Evolt0/tern-go-navigation.git
cd tern-go-navigation

go install golang.org/x/tools/gopls@v0.21.1
go build -o bin/tern-go-navigation .

tern plugin link "$PWD"
tern plugin list
```

Git 默认克隆到同名的 `tern-go-navigation` 目录，不需要额外指定目标目录。若已下载源码，直接从源码根目录执行安装 gopls、构建和链接步骤即可。不能仅运行 `tern plugin install <Git URL>` 就完成源码安装：必须先构建 Go 后端。

链接安装直接使用这个源码目录，**安装后不要删除或移动它**。若此前是复制安装，请先运行 `tern plugin remove go-navigation` 再链接源码，避免两种安装方式冲突。

两种方式安装后，`tern plugin list` 中都应显示插件 ID `go-navigation`，状态为 `ready`。打开 Go 项目中的本地 `.go` 文件，把光标放在符号上，按 **Ctrl+Option+G** 即可。

## 更新与卸载

### 更新复制安装的发布包

从 Releases 下载目标平台的归档和对应 `.sha256` 文件，在新的解压目录操作，避免旧文件残留。以下以 `1.3.3` 为例，升级时须将版本和 target 换为目标附件：

```sh
target=darwin-arm64 # 按你的系统和处理器选择。
archive="go-navigation-1.3.3-${target}.tar.gz"
if [ "$(uname -s)" = Darwin ]; then
  shasum -a 256 -c "$archive.sha256"
else
  sha256sum -c "$archive.sha256"
fi &&
tar -xzf "$archive" &&
tern plugin install ./go-navigation --force &&
tern plugin reload go-navigation
```

同样只有校验成功后才能解压安装。`--force` 用于覆盖**复制安装**；已有链接安装需先 `tern plugin unlink go-navigation`，然后按首次发布包安装步骤操作。

### 更新源码链接安装

在已链接的源码目录中执行：

```sh
git pull --ff-only
go build -o bin/tern-go-navigation .
tern plugin reload go-navigation
```

后端二进制更新后，下一次查询会使用新版本的后端服务，不需要重启 Tern。

### 卸载

复制安装的发布包：

```sh
tern plugin remove go-navigation
```

源码链接安装：

```sh
tern plugin unlink go-navigation
```

关闭 Go 文件或项目 tab 后，只要所属 Tern 进程仍在，插件会保留会话。插件 helper 和对应的 gopls 转发客户端会在所属 Tern 进程退出、后端二进制被更新或删除后回收；OMP 等其他客户端仍在使用时，共享 daemon 会继续运行。最后一个客户端断开后，gopls 默认等待 1 分钟退出。无 Tern 窗口上下文的独立 CLI helper 仍在空闲 5 分钟后退出。源码目录和自己安装的 gopls 不会被自动删除。

## 使用与快捷键

把光标放在函数、类型或变量名上：

| 快捷键（macOS） | 操作 | 命令面板 |
| --- | --- | --- |
| **Ctrl+Option+G** | 同时查询定义与调用 / 引用 | `Go: Navigate symbol` |
| **Ctrl+Option+S** | 按名称搜索当前项目的函数 / 符号 | `Go: Search project symbols` |
| **Ctrl+Option+B** | 返回跳转前的位置 | `Go: Jump back` |
| **↑ / ↓** | 在结果列表中选择位置 | — |
| **Enter** | 跳到所选位置 | — |
| **Esc** | 关闭结果列表 | — |

- **定义**排在前面，**调用 / 引用**排在后面；重复位置只保留一次，单个结果直接跳转。
- 引用包括函数调用、变量读写和类型使用，不仅是函数调用。
- 代码预览优先取未保存缓冲区，只去掉行首缩进和末尾空白，保留代码内部空白。
- 预览复用查询已读取的源文本，不增加 gopls 请求或文件读取；较长内容由原生列表省略显示。
- 历史保存在当前窗口内存中，插件重载后清空。

自定义键位使用动作 `plugin.go-navigation.navigate`、`plugin.go-navigation.symbols` 和 `plugin.go-navigation.back`。旧版的 `plugin.go-navigation.definition`、`plugin.go-navigation.references` 已合并移除，原独立引用快捷键不再注册。

## 搜索当前项目的函数

1. 打开当前项目中的任意本地 `.go` 文件，不必把光标放在函数名上。
2. 按 **Ctrl+Option+S**，输入函数或符号名，例如 `FindCustomer`，按 Enter 查询。
3. 在结果列表中查看符号名、文件行列和代码预览，再按 Enter 跳转；Ctrl+Option+B 返回原位置。

搜索由 gopls 的 `workspace/symbol` 提供，支持模糊匹配；未打开文件和当前窗口的未保存修改都参与查询。范围是当前模块，或 `go.work` 显式加入的模块（可以位于工作区目录外）；标准库、第三方依赖、vendor、未加入的嵌套模块和符号链接逃逸位置不作为结果。

当前 Tern 原生输入框在按 Enter 后才提交查询，不是逐键向服务器查询。结果列表中的输入用于进一步过滤已返回的结果。gopls v0.21.1 最多返回 100 个排名最高的符号，结果较多时请缩小关键词；这不是全文搜索，注释和函数体中的普通文本不作为符号命中。

## 索引与缓存

- **内存会话**：窗口、工作区和工具链环境各自保留独立缓冲区，兼容的客户端连接原生共享 gopls daemon。打开 Go 文件后提前加载；编辑器存活期间不会因短暂空闲丢弃会话。文件和未保存内容按变化同步，查询不重新扫描整个项目。
- **磁盘缓存**：直接使用 gopls 自身持久化的包摘要、交叉引用和符号索引，macOS 默认位于 `~/Library/Caches/gopls` 下，可由 `GOPLSCACHE` 改变缓存根目录。重启后可以复用这些缓存，但仍需加载工作区元数据。
- **不缓存过期结果**：插件不把上一次查询结果当成新答案；导航与搜索读取同一持续更新的语言服务。依赖目录发现仍在初始化和模块配置变化时运行，用于保留外部依赖修改的监听覆盖。

参考：[VS Code 跨文件符号导航](https://code.visualstudio.com/docs/editing/editingevolved#open-symbol-by-name)、[Zed 的 Go / gopls 支持](https://zed.dev/docs/languages/go)、[gopls 持久化索引与增量更新](https://go.dev/blog/gopls-scalability)。

### 与 OMP 共用 gopls

当前源码后端使用 `gopls -remote=auto serve`：服务存在时连接，没有时自动启动，不依赖 OMP 正在运行。OMP 与编辑器保留各自的未保存缓冲区和 Go 环境，共享可复用的缓存。

将下列配置合并到 OMP 当前用户配置目录的 `lsp.json`（通常为 `~/.omp/agent/lsp.json`），保留已有的其他语言服务配置：

```json
{
  "servers": {
    "gopls": {
      "args": ["-remote=auto", "serve"]
    }
  }
}
```

每个已经打开的 OMP 会话都需要重载一次 LSP 配置（`action: "reload", file: "*"`），或重新打开该 OMP 会话；旧客户端在重载前会继续使用原启动参数。两边需使用兼容的 gopls 构建和相同的运行目录（`XDG_RUNTIME_DIR`，否则使用系统临时目录）。

活动监视器中仍可能出现多个 `gopls`：共享 daemon、轻量转发客户端及 Go telemetry 辅助进程。各客户端的工作区视图仍独立，实际内存收益取决于可共享缓存的比例。从 `v1.3.1` 起发布包包含此模式，OMP 需采用上面的配置才能参与共享。

`v1.3.2` 恢复 gopls 默认诊断调度，并在断开前完成 LSP 会话关闭。此前的一小时诊断延迟可能在关闭时继续持有快照，阻止 daemon 进入空闲退出。从 `v1.3.1` 升级后，需要重启旧共享服务一次，清掉已排队的延迟任务，再重载 OMP 的 LSP、重新发起导航。只关闭项目 tab 时，插件仍按 Tern 进程生命周期保留会话。



## 工具链查找与排障

`gopls` 的查找顺序：环境变量 `GOPLS` 指定的可执行文件 → `PATH` → `$HOME/go/bin/gopls`。

Go 工具链优先沿用 `PATH`；GUI 环境找不到 `go` 时，依次检查 `$GOROOT/bin`、`/opt/homebrew/bin`、`/usr/local/go/bin`、`/usr/local/bin`，只把找到的目录加入 gopls 子进程环境，不修改系统环境。

macOS 使用原生 FSEvents，通过 [purego](https://github.com/ebitengine/purego) 调用，无需 cgo，也不再为每个源文件持有一个文件描述符。标准库和模块缓存合并原生监听根，事件仍按已注册的包目录过滤。每次查询先等待日志标记往返并排空待处理回调，再查询 gopls；不轮询、不按查询扫描整个工作区。事件丢失或监听根失效会使会话失败并重建，不继续返回可能过期的结果。Linux 继续使用 inotify。

- 提示无法启动 gopls：检查 `gopls version`；自定义安装目录需通过 Tern 继承的 `GOPLS` 或 `PATH` 指定。
- 提示找不到 Go：检查 `go version`；非标准安装位置需通过 Tern 继承的 `PATH` 或 `GOROOT` 指定。
- 工作区加载失败：错误提示会保留 gopls 的具体原因，而不是只显示 `no views`。
- 源码有修改但插件行为没变：重新运行 `go build -o bin/tern-go-navigation .` 和 `tern plugin reload go-navigation`。
- macOS 提示 **`too many open files`**：当前源码已替换旧 kqueue 后端及其关闭泄漏路径。更新实际安装的插件后端并重新加载插件；升级时重启 Tern 一次，释放旧后台进程残留的句柄。单独构建仓库不会更新另一份复制安装的插件。本次修复不要求调高终端 `ulimit`，调高限制也不能修复旧版泄漏。
- 选中名字后提示 **`textDocument/references: no identifier found`**：需要同时更新 `window.luau` 和后端，再重新加载插件。当前源码将原生选区的 UTF-8 字节范围转换为 LSP 查询位置，不再使用选区末尾的光标。没有选区时，光标应位于标识符内，而不是括号等标点上。

## 当前边界

- **不支持普通符号的 Command+点击。** Tern 0.9.7 的[公开路由接口](https://docs.stencil.so/tern/guides/routing.html#link-requests)只提供链接点击事件，没有普通代码符号的点击位置回调。此前已用真实符号点击和 URL 点击对照验证；插件没有引入全局鼠标监听或辅助功能权限。
- **预览是结果行内的一行代码**，不是独立的选中项预览面板。当前原生选择器没有对应面板或选中变化回调。
- 仅支持本机文件；远程主机文件会明确提示不支持。
- 未保存的 `go.mod` / `go.work` 不作为 overlays，修改模块配置后需保存。
- 首次加载仍需读取包元数据和建立索引，大项目可能较慢；请求上限 45 秒。预热尚未完成就发起查询仍需等待，查询取消或语言服务失败后会重新初始化。

## 性能

以下为 1.3.0 原有本机实测记录，不是本次发布工作流的测试结果。后端对比使用同一份插件源码副本；桌面数据使用独立 Go 示例项目，不代表大型仓库表现。优化重点是把首次加载移到后台并避免空闲后的重复冷启动，不是宣称已有热查询算法突然变快。

| 场景 | 耗时 |
| --- | ---: |
| 1.2.1 首次直接导航 | 1179.1 ms |
| 1.2.1 后续热导航 | 6.0–6.9 ms |
| 1.3.0 后台预热 | 971.5 ms |
| 1.3.0 预热后的第一次用户导航 | 24.7 ms |
| 1.3.0 后续热导航 | 6.3–7.9 ms |
| 桌面自动预热后，首次按键至导航结果 | 34.8 ms |
| 桌面提交函数名至项目搜索结果 | 54.4 ms |

## 开发与验证

在源码根目录执行：

```sh
go test -race ./...
go build -o bin/tern-go-navigation .
tern plugin reload go-navigation
```

集成测试需要 gopls，使用临时项目且禁用网络依赖；覆盖未保存缓冲区、关闭 / 重开、源码行预览、Unicode 坐标、即时磁盘增删改、工作区外模块、模块配置变化、并发启动、原子保存、编辑器生命周期，以及项目符号搜索的更新与范围隔离。

此前已在真实 Tern 桌面验证合并列表、代码预览、定义 / 调用跳转、返回、过期结果保护和项目函数查询；确认按键前已自动预热，搜索未打开文件与未保存改名成功，标准库符号被排除，连续操作复用同一 gopls 进程。上方截图来自实际运行的插件与示例 Go 项目，不是设计稿。发布结果请以对应标签的 Actions 日志和 Release 附件为准。

实现分工：`window.luau` 负责原生编辑器、预热和选择器；`main.go` 负责请求与位置转换；`daemon.go`、`session.go`、`rpc.go` 负责常驻语言服务；`symbols.go` 负责项目符号查询和范围过滤；`watch.go` 负责发现监听范围；`watch_darwin.go`、`fsevents_darwin.go` 负责 macOS 事件同步；`watch_fsnotify.go` 保留 inotify 后端。

### 本地打包与冒烟检查

维护者需要 Python 3.9+（仅标准库，不依赖 Python 3.11 的 `tomllib`）、Go 和 gopls。默认构建本机平台，也可用 `--target` 指定上表中的任意平台；冒烟检查必须在与归档系统和架构一致的主机上执行：

```sh
python3 scripts/package.py --target darwin-arm64 --output dist
python3 scripts/smoke_release.py dist/go-navigation-1.3.3-darwin-arm64.tar.gz
```

`--output` 默认为 `dist`，`--source PATH` 可选择单独的源码 checkout，默认使用本仓库。打包输出归档和相邻的 `.tar.gz.sha256`，冒烟检查验证实际二进制格式与架构，并运行归档中的 CLI。可通过 `--tag v1.3.3` 校验版本，或只校验而不打包：

```sh
python3 scripts/package.py --check-tag v1.3.3
```

### 发布流程

[发布工作流](https://github.com/Evolt0/tern-go-navigation/blob/main/.github/workflows/release.yml)在 `v*` 标签推送或手动 `workflow_dispatch` 时运行：

- 构建矩阵使用 macOS arm64、macOS amd64、Linux amd64、Linux arm64 的原生 runner、稳定版 Go 和 `gopls@v0.21.1`，分别执行 `go test -race ./...`、打包和实际 CLI 冒烟检查，再上传各平台 Actions artifact。官方 Actions 固定到提交 SHA。
- 手动触发时，`release_tag` 留空只构建和检查，不创建 Release。
- 推送 `vX.Y.Z` 标签时，四个平台全部通过后，独立发布任务创建 Release 并上传归档与校验文件。标签必须严格等于 `v` 加 `plugin.toml` 的版本。仅发布任务拥有 `contents: write`，不需要自定义 secrets。
- 为已有 Release 补充缺失平台时，在 `main` 上手动运行并填写 `release_tag`。使用该标签的源码和当前打包工具，原生验证四个平台后只上传不存在的附件；保留原标签、Release 说明和已有附件。

```sh
gh workflow run release.yml --ref main -f release_tag=v1.3.0
```

新版本应先更新 `plugin.toml`、[变更记录](docs/reference/changelog.md)和文档示例，提交并推送所有改动，再创建并推送相匹配的版本标签。不要移动或复用已发布标签；修复运行时行为应发布新版本。

## 变更记录

见 [Changelog](docs/reference/changelog.md)。
