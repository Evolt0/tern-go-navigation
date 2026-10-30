# Go Navigation — a Tern plugin

在 Tern **原生 Go 编辑器**中，一次查看符号的定义与调用 / 引用，或按名称搜索当前项目的函数、方法、类型和变量，预览源码后跳转。不替换编辑器，不要求先保存文件。

![Tern 原生选择器：定义与调用分组，文件行列和命中行代码预览](assets/navigation-preview.png)

![选择定义后跳转到 Go 源文件，光标定位到函数声明](assets/definition-jump.png)

![在当前项目中按名称搜索函数，查找尚未打开文件里的定义](assets/project-symbol-search.png)

## 功能亮点

- **一个入口** — 一次查询定义与引用，在同一列表分组展示；无需分别执行两个命令。
- **项目内函数查询** — 输入名称即可搜索项目中未打开文件里的函数、方法、类型和变量，排除标准库、第三方依赖和未加入工作区的嵌套模块。
- **先看代码，再跳转** — 每条结果显示文件名、行列和命中行代码，完整路径放在代码后面。
- **支持未保存修改** — 查询和预览使用当前窗口的编辑器缓冲区，不隐式保存或修改源文件。
- **提前加载并保留索引** — 聚焦本地 Go 文件后尝试后台预热；编辑器进程存活期间保留 gopls，不再因空闲 5 分钟退出。导航和项目搜索共用同一服务。
- **可以返回** — 保留窗口内跳转历史；光标或缓冲区改变后，不应用过期的跳转结果。
- **Go 工作区与 Unicode** — 支持 `go.mod`、`go.work`，处理 Tab、中文、emoji 和组合字符的位置转换。

## 安装

### 环境要求

- **Tern**，且终端中可以运行 `tern` 命令。已验证 `0.9.7 (c2f7563)`。
- **Go 工具链**。本插件构建最低要求 Go 1.23；安装 gopls 时，Go 可能按 gopls 的要求自动下载更新的工具链。本机验证版本为 Go 1.27.3。
- **gopls**。下面使用已验证的 `v0.21.1`。
- **Git**，用于下载和更新源码。

已验证平台：**macOS Apple Silicon（arm64）**。其他 Unix 平台尚未验证；当前后端依赖 Unix socket 和进程信号，不支持 Windows。

### 从源码安装

当前安装方式是**下载源码 → 编译后端 → 链接插件**。仓库不包含后端二进制，也没有首次使用时自动构建的逻辑。

```sh
git clone git@github.com:Evolt0/tern-go-navigation.git
cd tern-go-navigation

go install golang.org/x/tools/gopls@v0.21.1
go build -o bin/tern-go-navigation .

tern plugin link "$PWD"
tern plugin list
```

列表中应能看到 `go-navigation`，状态为 `ready`。打开一个 Go 项目中的 `.go` 文件，把光标放在符号上，按 **Ctrl+Option+G** 即可。

如果已经下载源码，可直接在源码根目录执行 `go install`、`go build` 和 `tern plugin link`。链接安装直接使用这个目录，**安装后不要删除或移动它**。

> 不能仅运行 `tern plugin install <Git URL>` 就完成安装：插件需要上面的 Go 后端编译步骤。当前没有发布预编译安装包。

### 更新

在已链接的源码目录中执行：

```sh
git pull --ff-only
go build -o bin/tern-go-navigation .
tern plugin reload go-navigation
```

重新构建后，下一次查询会使用新版本的后端服务；不需要重启 Tern。

### 卸载

```sh
tern plugin unlink go-navigation
```

编辑器启动的语言服务会在所属 Tern 进程退出、后端二进制被更新或删除后回收；卸载后如需立即释放，可退出相关 Tern 进程。无 Tern 窗口上下文的独立 CLI 服务仍在空闲 5 分钟后退出。源码目录和自己安装的 gopls 不会被自动删除。

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

- **内存会话**：按窗口、工作区和工具链环境复用 gopls。打开 Go 文件后提前加载；编辑器存活期间不会因短暂空闲丢弃索引。文件和未保存内容按变化同步，查询不重新扫描整个项目。
- **磁盘缓存**：直接使用 gopls 自身持久化的包摘要、交叉引用和符号索引，macOS 默认位于 `~/Library/Caches/gopls` 下，可由 `GOPLSCACHE` 改变缓存根目录。重启后可以复用这些缓存，但仍需加载工作区元数据。
- **不缓存过期结果**：插件不把上一次查询结果当成新答案；导航与搜索读取同一持续更新的语言服务。依赖目录发现仍在初始化和模块配置变化时运行，用于保留外部依赖修改的监听覆盖。

参考：[VS Code 跨文件符号导航](https://code.visualstudio.com/docs/editing/editingevolved#open-symbol-by-name)、[Zed 的 Go / gopls 支持](https://zed.dev/docs/languages/go)、[gopls 持久化索引与增量更新](https://go.dev/blog/gopls-scalability)。

## 工具链查找与排障

`gopls` 的查找顺序：环境变量 `GOPLS` 指定的可执行文件 → `PATH` → `$HOME/go/bin/gopls`。

Go 工具链优先沿用 `PATH`；GUI 环境找不到 `go` 时，依次检查 `$GOROOT/bin`、`/opt/homebrew/bin`、`/usr/local/go/bin`、`/usr/local/bin`，只把找到的目录加入 gopls 子进程环境，不修改系统环境。

- 提示无法启动 gopls：检查 `gopls version`；自定义安装目录需通过 Tern 继承的 `GOPLS` 或 `PATH` 指定。
- 提示找不到 Go：检查 `go version`；非标准安装位置需通过 Tern 继承的 `PATH` 或 `GOROOT` 指定。
- 工作区加载失败：错误提示会保留 gopls 的具体原因，而不是只显示 `no views`。
- 源码有修改但插件行为没变：重新运行 `go build -o bin/tern-go-navigation .` 和 `tern plugin reload go-navigation`。

## 当前边界

- **不支持普通符号的 Command+点击。** Tern 0.9.7 的[公开路由接口](https://docs.stencil.so/tern/guides/routing.html#link-requests)只提供链接点击事件，没有普通代码符号的点击位置回调。已用真实符号点击和 URL 点击对照验证；插件没有引入全局鼠标监听或辅助功能权限。
- **预览是结果行内的一行代码**，不是独立的选中项预览面板。当前原生选择器没有对应面板或选中变化回调。
- 仅支持本机文件；远程主机文件会明确提示不支持。
- 未保存的 `go.mod` / `go.work` 不作为 overlays，修改模块配置后需保存。
- 首次加载仍需读取包元数据和建立索引，大项目可能较慢；请求上限 45 秒。预热尚未完成就发起查询仍需等待，查询取消或语言服务失败后会重新初始化。

## 性能

1.3.0 本机实测如下。后端对比使用同一份插件源码副本；桌面数据使用独立 Go 示例项目，不代表大型仓库表现。优化重点是把首次加载移到后台并避免空闲后的重复冷启动，不是宣称已有热查询算法突然变快。

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

```sh
go test -race ./...
go build -o bin/tern-go-navigation .
tern plugin reload go-navigation
```

集成测试需要 gopls，使用临时项目且禁用网络依赖；覆盖未保存缓冲区、关闭 / 重开、源码行预览、Unicode 坐标、即时磁盘增删改、工作区外模块、模块配置变化、并发启动、原子保存、编辑器生命周期，以及项目符号搜索的更新与范围隔离。

已在真实 Tern 桌面验证合并列表、代码预览、定义 / 调用跳转、返回、过期结果保护和项目函数查询；确认按键前已自动预热，搜索未打开文件与未保存改名成功，标准库符号被排除，连续操作复用同一 gopls 进程。上方截图来自实际运行的插件与示例 Go 项目，不是设计稿。

实现分工：`window.luau` 负责原生编辑器、预热和选择器；`main.go` 负责请求与位置转换；`daemon.go`、`session.go`、`rpc.go` 负责常驻语言服务；`symbols.go` 负责项目符号查询和范围过滤；`watch.go` 负责文件变更同步。

## 变更记录

见 [Changelog](docs/reference/changelog.md)。
