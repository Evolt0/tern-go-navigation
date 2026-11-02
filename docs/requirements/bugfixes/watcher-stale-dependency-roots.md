# Bug: 模块切换后残留文件监听根

## 现象与复现

在同一长驻 helper 中连续切换本地 `replace` 目标:

- 39 次切换后，文件描述符从 41 线性增至 345（约每个已移除依赖根 8 个），RSS 从 16.6 MiB 增至 20.8 MiB。
- 已移除依赖文件的新编辑仍作为 `didChangeWatchedFiles` 上报，gopls 会重新分析不再参与构建的模块。
- Linux 的 inotify 每个目录占一个 watch descriptor，同样只增不减；跨 `go.work` 成员变化时更明显。

## 根因

`watch.go` 的 `discover` 每次只调用 `add` 增加监听根，从不删除上一轮发现、但本轮已不存在的根。v1.3.3 的 FSEvents 改动限制了每个文件持有的句柄，也合并了原生监听根，但没有限制历史上出现过的根数量。因此编辑器存活越久、模块配置变化越多，残留根、文件快照和待发事件越多。

## 修复

- `discover` 收集本轮实际需要的根（工作区、`replace` 目标、主模块、依赖包目录），完成后调用 `retainObserved`，删除不再需要的根及其文件快照和待发事件。
- gopls 通过 `client/registerCapability` 动态注册的根单独记录，永不因模块变化被裁剪；覆盖这些注册根的递归根也保留。
- 裁剪后立即重配原生流（macOS）或移除 inotify watch（Linux），句柄在本次查询内回收，而不是等下一次查询。
- 模块枚举失败时不裁剪，避免把暂时无法解析的模块误判为根消失。
- Linux 裁剪基于目录快照计算保留集，结果与 map 迭代顺序无关。

## 验证

- 新增 `TestWatcherDropsReplacedDependencyRoots`：切换 `replace` 后旧依赖编辑不再上报，新依赖仍上报。修复前失败，修复后通过；macOS 与 Linux 共用。
- 新增 `TestWatcherDarwinReplacedRootsReleaseDescriptors`：8 次切换后描述符增量不超过 24。修复前残留 84 个，修复后通过。
- `CGO_ENABLED=0 go test -count=3 ./...`、`go test -race ./...` 通过；既有描述符用例连续 5 轮通过。
- 真实 CLI 连续切换 300 次 `replace`：描述符恒为 41，RSS 从 16.6 MiB 增至 22.3 MiB 后趋于平稳，300 次导航结果均指向当前替换模块。
- Linux arm64 / amd64 交叉编译通过；运行时用例由发布工作流在 Linux 执行。
