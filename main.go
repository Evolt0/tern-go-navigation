package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf16"

	"github.com/rivo/uniseg"
)

type query struct {
	Path      string            `json:"path"`
	Line      int               `json:"line"`
	Col       int               `json:"col"`
	TabWidth  int               `json:"tabWidth"`
	Documents map[string]string `json:"documents"`
}

type position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type sourceRange struct {
	Start position `json:"start"`
	End   position `json:"end"`
}

type location struct {
	URI   string      `json:"uri"`
	Range sourceRange `json:"range"`
}

type destination struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Col     int    `json:"col"`
	URL     string `json:"url"`
	Kind    string `json:"kind"`
	Preview string `json:"preview"`
}

func canonical(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	// Newly created unsaved files may not yet exist on disk.
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filepath.Base(path)), nil
}

func workspace(path string) string {
	start := filepath.Dir(path)
	module := ""
	for dir := start; ; dir = filepath.Dir(dir) {
		if info, err := os.Stat(filepath.Join(dir, "go.work")); err == nil && !info.IsDir() {
			return dir
		}
		if module == "" {
			if info, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !info.IsDir() {
				module = dir
			}
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	if module != "" {
		return module
	}
	return start
}

func fileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

func sourceLine(text string, line int) (string, error) {
	if line < 1 {
		return "", errors.New("line must be positive")
	}
	for n := 1; n < line; n++ {
		_, rest, ok := strings.Cut(text, "\n")
		if !ok {
			return "", errors.New("line outside document")
		}
		text = rest
	}
	lineText, _, _ := strings.Cut(text, "\n")
	return strings.TrimSuffix(lineText, "\r"), nil
}

// Native editor columns count display cells (including tab stops), not Unicode scalars.
// gopls positions count UTF-16 code units. Convert only at grapheme boundaries.
func lspPosition(text string, line, col, tabWidth int) (position, error) {
	s, err := sourceLine(text, line)
	if err != nil {
		return position{}, err
	}
	if tabWidth < 1 {
		return position{}, errors.New("tab width must be positive")
	}
	cells, units := 0, 0
	g := uniseg.NewGraphemes(s)
	for g.Next() {
		if cells == col-1 {
			return position{line - 1, units}, nil
		}
		if g.Str() == "\t" {
			cells += tabWidth - cells%tabWidth
		} else {
			cells += g.Width()
		}
		for _, ch := range g.Str() {
			units += utf16.RuneLen(ch)
		}
	}
	if cells == col-1 {
		return position{line - 1, units}, nil
	}
	return position{}, errors.New("column outside document or inside a grapheme")
}

func ternColumn(text string, pos position, tabWidth int) (int, error) {
	s, err := sourceLine(text, pos.Line+1)
	if err != nil {
		return 0, err
	}
	if tabWidth < 1 {
		return 0, errors.New("tab width must be positive")
	}
	units, cells := 0, 0
	g := uniseg.NewGraphemes(s)
	for g.Next() {
		if units == pos.Character {
			return cells + 1, nil
		}
		for _, ch := range g.Str() {
			units += utf16.RuneLen(ch)
		}
		if g.Str() == "\t" {
			cells += tabWidth - cells%tabWidth
		} else {
			cells += g.Width()
		}
	}
	if units == pos.Character {
		return cells + 1, nil
	}
	return 0, errors.New("navigation position outside document or inside a grapheme")
}

// GUI applications may inherit only the system PATH, without the installed Go toolchain.
// Keep an existing PATH selection; otherwise add only the directory containing Go.
func goEnvironment() ([]string, error) {
	if _, err := exec.LookPath("go"); err == nil {
		return os.Environ(), nil
	}
	dirs := []string{"/opt/homebrew/bin", "/usr/local/go/bin", "/usr/local/bin"}
	if root := os.Getenv("GOROOT"); root != "" {
		dirs = append([]string{filepath.Join(root, "bin")}, dirs...)
	}
	for _, dir := range dirs {
		if _, err := exec.LookPath(filepath.Join(dir, "go")); err != nil {
			continue
		}
		env := os.Environ()
		path := "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")
		for i, entry := range env {
			if strings.HasPrefix(entry, "PATH=") {
				env[i] = path
				return env, nil
			}
		}
		return append(env, path), nil
	}
	return nil, errors.New("Go toolchain not found: install Go or make go available in Tern's PATH or GOROOT/bin")
}

func prepareQuery(q query) (query, position, error) {
	path, err := canonical(q.Path)
	if err != nil {
		return q, position{}, err
	}
	documents := make(map[string]string, len(q.Documents))
	for name, text := range q.Documents {
		name, err = canonical(name)
		if err != nil {
			return q, position{}, err
		}
		documents[name] = text
	}
	text, ok := documents[path]
	if !ok {
		return q, position{}, errors.New("current editor buffer is missing")
	}
	pos, err := lspPosition(text, q.Line, q.Col, q.TabWidth)
	q.Path, q.Documents = path, documents
	return q, pos, err
}

func destinations(locations []location, kind string, q query, seen map[string]bool) ([]destination, error) {
	out := make([]destination, 0, len(locations))
	for _, loc := range locations {
		u, err := url.Parse(loc.URI)
		if err != nil || u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") {
			return nil, fmt.Errorf("unsupported navigation URI %q", loc.URI)
		}
		name, err := canonical(u.Path)
		if err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%s:%d:%d", name, loc.Range.Start.Line, loc.Range.Start.Character)
		if seen[key] {
			continue
		}
		seen[key] = true
		content, ok := q.Documents[name]
		if !ok {
			data, err := os.ReadFile(name)
			if err != nil {
				return nil, err
			}
			content = string(data)
			// prepareQuery owns this map, and overlays were already synchronized.
			// Reuse disk text for other references in this query, never across queries.
			q.Documents[name] = content
		}
		line := loc.Range.Start.Line + 1
		code, err := sourceLine(content, line)
		if err != nil {
			return nil, err
		}
		col, err := ternColumn(code, position{Character: loc.Range.Start.Character}, q.TabWidth)
		if err != nil {
			return nil, err
		}
		u.Fragment = fmt.Sprintf("L%dC%d", line, col)
		out = append(out, destination{Path: name, Line: line, Col: col, URL: u.String(), Kind: kind, Preview: strings.TrimSpace(code)})
	}
	return out, nil
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "--daemon" {
		if err := serveDaemon(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	var q query
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, maxQueryBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&q); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	shutdown := len(os.Args) == 2 && os.Args[1] == "--shutdown"
	locations, err := daemonQuery(ctx, q, shutdown)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(locations); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
