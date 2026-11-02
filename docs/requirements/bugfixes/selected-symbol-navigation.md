# Bug: 选中函数名后引用查询找不到标识符

## 现象

在原生 Go 编辑器中双击 / 选择函数名，再执行导航，出现 `Go navigation failed: textDocument/references: no identifier found`。这与此前文件描述符耗尽是不同问题。

## 根因与证据

- Tern 的 `caret` 位于选区 head。正向选中整个函数名后，它处于名字的排他末尾，通常正好是后面的 `(`，并不在标识符内。
- `window.luau` 原来只发送 caret 行列。后端把这个点直接用于 definition 和 references；gopls 的引用查询要求命中标识符，失败会使整个组合查询报错。
- 实际编辑器快照中的 `collectTemplateDefinitions` 选区为 `{anchor:1191, head:1217}`，caret 为 `{line:28, col:32}`。把缓冲区按 UTF-8 字节切片 `[1191:1217]` 正好得到完整名字，证实原生选区使用零基 UTF-8 字节偏移。caret 行列与 LSP UTF-16 列不能混用。
- 修复前，真实 gopls 回归 `TestSelectedSymbolNavigation` 在选中声明名、caret 位于 `(` 时复现同一条 references 错误。

## 修复

- 请求增加可选 `selection: {anchor, head}`，直接传递原生零基 UTF-8 字节范围。
- 非空选区采用较小端点作为查询起点，正选与反选一致。后端基于请求中当前编辑器缓冲区转换为零基 LSP 行号 / UTF-16 列，不读取磁盘副本推算偏移。
- 选区端点必须在文档内且位于 UTF-8 码点边界；错误明确返回。无选区或折叠选区仍使用已有 caret 显示列转换。
- 窗口在异步读取缓冲区前、返回结果后和实际跳转前检查 selection anchor/head，防止选区已改变但 head 行列不变时应用过期结果。
- `session.go`、`rpc.go` 的语言服务错误处理不变：没有匹配错误文本、忽略 references 失败或退回残缺结果。

## 验证

- `selection_test.go` 覆盖正向 / 反向选择、折叠选区、CRLF、中文、emoji、组合字符、Tab，以及非法 UTF-8 端点。
- 真实 gopls 用例覆盖选中声明、反向选中调用和未保存改名；定义 / 引用及行列正确，磁盘内容不被测试中的导航修改。
- `go test -race ./...` 通过。
- `GOTOOLCHAIN=go1.23.12 CGO_ENABLED=0 go test -run '^(TestPrepareQueryUsesSelectionByteOffsets|TestSelectedSymbolNavigation)$' -count=1` 通过。
- 捕获的实际编辑器缓冲区请求返回 `collectTemplateDefinitions` 的 28 行定义及 36 / 41 / 65 行引用。
- 截图对应的 `templateDefinitionLanguage` 正选 / 反选均返回定义 `46:6` 和引用 `73:76`。
- 已更新本机实际安装的二进制和 `window.luau`，重新加载状态 `ready`。通过原生窗口控制执行真实导航：选择器显示这两条结果，选中引用后编辑器 caret 实际到达 `73:76`；测试后恢复原选区与任务 tab。
- 窗口截图命令的默认输出目录只读，系统窗口截屏也不可用，因此本次桌面证据为原生选择器状态及实际跳转状态，没有新增截图。

## 升级

必须同时更新后端和 `window.luau`，然后 `tern plugin reload go-navigation`。只更新后端时，旧窗口脚本仍不会发送选区。没有选区时，标点位置不等同于标识符位置；本修复不会把任意标点查询悄悄改成邻近符号查询。
