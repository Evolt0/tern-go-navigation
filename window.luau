--!strict

local history: { any } = {}
local generation = 0

local function fileURL(path: string, line: number, col: number): string
	local escaped = path:gsub("[^%w%-%._~/]", function(c)
		return string.format("%%%02X", string.byte(c))
	end)
	return "file://" .. escaped .. "#L" .. line .. "C" .. col
end

local function focusedGo(cx: any): any
	local pane = cx.session:focused()
	if not pane then return nil end
	local info = cx.session:pane(pane)
	if not info or info.kind ~= "file" or not info.path or info.path:sub(-3) ~= ".go" then
		return nil
	end
	return cx.session:read(pane, { lines = 1, max_chars = 1 })
end

local function unchangedPosition(cx: any, origin: any): boolean
	if cx.session:focused() ~= origin.pane then return false end
	local current = cx.session:read(origin.pane, { lines = 1, max_chars = 1 })
	return current ~= nil and current.path == origin.path and current.caret ~= nil
		and current.caret.line == origin.line and current.caret.col == origin.col
end

-- Re-read every captured overlay before navigating: edits during indexing invalidate the result.
local function readDocuments(cx: any, paths: { string }, token: number, done: any)
	local documents: { [string]: string } = {}
	local remaining = #paths
	local failed = false
	for _, path in paths do
		cx.docs:read(path):next(function(text: any, err: any, nextCx: any)
			if failed or token ~= generation then return end
			if err or text == nil then
				failed = true
				nextCx:toast("error", "Go navigation: cannot read editor buffer", tostring(err or path))
				return
			end
			documents[path] = text
			remaining -= 1
			if remaining == 0 then done(documents, nextCx) end
		end)
	end
end

local function goTo(cx: any)
	local current = focusedGo(cx)
	if not current or not current.caret or current.load ~= "ready" then
		cx:toast("info", "Focus a loaded Go source editor first")
		return
	end
	if current.host ~= 0 then
		cx:toast("error", "Go navigation currently requires a local file", "The language server runs on this Mac, not a remote host.")
		return
	end
	generation += 1
	local token = generation
	local origin = {
		pane = current.info.pane, path = current.path,
		line = current.caret.line, col = current.caret.col,
	}
	local paths = { origin.path }
	local seen = { [origin.path] = true }
	for _, pane in cx.session:panes() do
		if pane.kind == "file" and pane.path and pane.path:sub(-3) == ".go" and not seen[pane.path] then
			local snapshot = cx.session:read(pane.pane, { lines = 1, max_chars = 1 })
			if snapshot and snapshot.host == 0 and snapshot.load == "ready" then
				seen[pane.path] = true
				table.insert(paths, pane.path)
			end
		end
	end
	cx:toast("info", "Go: finding definitions and references…")
	readDocuments(cx, paths, token, function(documents: any, live: any)
		if not unchangedPosition(live, origin) then return end
		local request = tern.json.encode({
			path = origin.path, line = origin.line, col = origin.col, documents = documents,
			tabWidth = live.settings:get("files.tab_width"),
		})
		tern.process.run({ tern.plugin.dir .. "/bin/tern-go-navigation" }, {
			stdin = request, timeout_ms = 50000,
		}):next(function(result: any, err: any, nextCx: any)
			if token ~= generation then return end
			if err or not result or result.status ~= 0 then
				nextCx:toast("error", "Go navigation failed", tostring(err or (result and result.stderr) or "No process result"))
				return
			end
			local ok, targets = pcall(tern.json.decode, result.stdout)
			if not ok or type(targets) ~= "table" then
				nextCx:toast("error", "Go navigation returned invalid JSON")
				return
			end
			if #targets == 0 then
				nextCx:toast("info", "No Go definition or references at this position")
				return
			end
			local function navigate(target: any, navCx: any)
				if token ~= generation then return end
				readDocuments(navCx, paths, token, function(latest: any, checkedCx: any)
					if not unchangedPosition(checkedCx, origin) then
						checkedCx:toast("info", "Go: cursor moved; run navigation again")
						return
					end
					for path, text in documents do
						if latest[path] ~= text then
							checkedCx:toast("info", "Go: buffer changed; run navigation again")
							return
						end
					end
					checkedCx:open(target.url)
					table.insert(history, origin)
				end)
			end
			if #targets == 1 then
				navigate(targets[1], nextCx)
			else
				local items = {}
				for _, target in targets do
					local name = target.path:match("[^/]+$") or target.path
					table.insert(items, {
						label = name .. ":" .. target.line .. ":" .. target.col,
						detail = target.preview .. "  —  " .. target.path,
						group = if target.kind == "definition" then "定义" else "调用 / 引用",
						target = target,
					})
				end
				nextCx:pick({ items = items, placeholder = "Go: 定义与调用 / 引用", action = "jump" }):next(function(choice: any, pickErr: any, pickCx: any)
					if pickErr then pickCx:toast("error", "Go navigation picker failed", tostring(pickErr)) end
					if choice then navigate(choice.target, pickCx) end
				end)
			end
		end)
	end)
end

tern.command({
	id = "navigate", title = "Go: Navigate symbol", group = "Go Navigation",
	keys = { "ctrl+alt+g" }, run = goTo,
})

tern.command({
	id = "back", title = "Go: Jump back", group = "Go Navigation",
	keys = { "ctrl+alt+b" },
	run = function(cx)
		generation += 1
		local previous = table.remove(history)
		if not previous then
			cx:toast("info", "No Go jump history")
			return
		end
		if cx.session:focused() ~= previous.pane and cx.session:pane(previous.pane) then
			cx.layout:focus(previous.pane)
		end
		cx:open(fileURL(previous.path, previous.line, previous.col))
	end,
})
