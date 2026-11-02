--!strict
-- Optional frontend integration; gopls and the Go helper stay unchanged.
local registry = assert(tern.plugin.data:match("^(.*)/plugin%-data/[^/]+$"), "Cannot locate Tern plugin registry") .. "/plugins"
local javaDirectory = registry .. "/java-navigation"
local link = registry .. "/java-navigation.path"
if tern.fs.exists(link) then javaDirectory = tern.fs.read(link, 16384):match("^%s*(.-)%s*$") end
local module = javaDirectory .. "/navigation.luau"
if not tern.fs.exists(module) then return false end
local factory = assert(loadstring(tern.fs.read(module, 65536), "@navigation.luau"))()
factory(javaDirectory, tern.plugin.dir)
return true
