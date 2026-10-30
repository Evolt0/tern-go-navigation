package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func (s *session) symbols(ctx context.Context, q query) ([]destination, error) {
	search := strings.TrimSpace(q.Search)
	if search == "" {
		return nil, errors.New("symbol search must not be empty")
	}
	result, err := s.rpc.call(ctx, "workspace/symbol", map[string]any{"query": search})
	if err != nil {
		return nil, err
	}
	// gopls returns SymbolInformation, including full locations, without a
	// workspaceSymbol/resolve round trip. Keep its ranked names and ordering.
	var symbols []struct {
		Name     string   `json:"name"`
		Location location `json:"location"`
	}
	if err := json.Unmarshal(result, &symbols); err != nil {
		return nil, err
	}
	locations := make([]location, 0, len(symbols))
	names := make([]string, 0, len(symbols))
	seen := make(map[string]bool, len(symbols))
	directories := make(map[string]bool)
	for _, symbol := range symbols {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		u, err := url.Parse(symbol.Location.URI)
		if err != nil || u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") {
			return nil, fmt.Errorf("unsupported navigation URI %q", symbol.Location.URI)
		}
		path, err := canonical(u.Path)
		if err != nil {
			return nil, err
		}
		if filepath.Ext(path) != ".go" {
			continue
		}
		dir := filepath.Dir(path)
		included, cached := directories[dir]
		if !cached {
			included, err = projectSymbolDirectory(dir, s.projectRoots)
			if err != nil {
				return nil, err
			}
			directories[dir] = included
		}
		if !included {
			continue
		}
		key := fmt.Sprintf("%s:%d:%d", path, symbol.Location.Range.Start.Line, symbol.Location.Range.Start.Character)
		if seen[key] {
			continue
		}
		seen[key] = true
		symbol.Location.URI = fileURI(path)
		locations = append(locations, symbol.Location)
		names = append(names, symbol.Name)
	}
	// Filter before destinations reads any source. Canonical, unique locations
	// ensure its output remains aligned with the server-provided names.
	clear(seen)
	out, err := destinations(locations, "symbol", q, seen)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Name = names[i]
	}
	return out, nil
}

// Both dir and roots are canonical. Only main modules are enrolled: a local
// replacement, nested module, or symlink escape is not a project member merely
// because gopls loaded it. The deepest enrolled root also permits explicit
// go.work members nested inside another main module.
func projectSymbolDirectory(dir string, roots []string) (bool, error) {
	root := ""
	for _, candidate := range roots {
		if dir == candidate || strings.HasPrefix(dir, strings.TrimSuffix(candidate, string(filepath.Separator))+string(filepath.Separator)) {
			if len(candidate) > len(root) {
				root = candidate
			}
		}
	}
	if root == "" {
		return false, nil
	}
	for current := dir; current != root; current = filepath.Dir(current) {
		if filepath.Base(current) == "vendor" {
			return false, nil
		}
		info, err := os.Stat(filepath.Join(current, "go.mod"))
		if err == nil && !info.IsDir() {
			return false, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return false, err
		}
	}
	return true, nil
}
