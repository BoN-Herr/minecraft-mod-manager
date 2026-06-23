// Package packwiz wraps the packwiz CLI and parses the TOML files it produces.
// Only the server/admin tool uses this; the client never sees packwiz.
package packwiz

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	toml "github.com/pelletier/go-toml/v2"
)

// CLI is a handle to a packwiz binary plus the pack directory it operates on.
type CLI struct {
	Bin     string // path to packwiz / packwiz.exe
	PackDir string // directory containing pack.toml
}

// binName returns the platform-specific packwiz executable name.
func binName() string {
	if runtime.GOOS == "windows" {
		return "packwiz.exe"
	}
	return "packwiz"
}

// Locate finds a packwiz binary, preferring one bundled next to our own
// executable (./tools/ or ./.tools/bin/), then falling back to PATH.
func Locate() (string, error) {
	name := binName()
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, c := range []string{
			filepath.Join(dir, name),
			filepath.Join(dir, "tools", name),
			filepath.Join(dir, ".tools", "bin", name),
		} {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				return c, nil
			}
		}
	}
	// Dev fallback: relative to current working directory.
	for _, c := range []string{
		filepath.Join(".tools", "bin", name),
		filepath.Join("tools", name),
	} {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			abs, _ := filepath.Abs(c)
			return abs, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("packwiz binary (%s) not found next to this tool or on PATH", name)
}

// Run executes packwiz with the given args inside PackDir, streaming output.
func (c *CLI) Run(args ...string) error {
	cmd := exec.Command(c.Bin, args...)
	cmd.Dir = c.PackDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// Refresh rebuilds the packwiz index (picks up manually dropped jars too).
func (c *CLI) Refresh() error { return c.Run("refresh") }

// ---- TOML models ---------------------------------------------------------

// Pack mirrors the fields of pack.toml we care about.
type Pack struct {
	Name    string `toml:"name"`
	Author  string `toml:"author"`
	Version string `toml:"version"`
	Index   struct {
		File string `toml:"file"`
	} `toml:"index"`
	Versions map[string]string `toml:"versions"`
}

// Index mirrors index.toml.
type Index struct {
	HashFormat string      `toml:"hash-format"`
	Files      []IndexFile `toml:"files"`
}

// IndexFile is one [[files]] entry. Metafile=true means File is a .pw.toml
// describing a remote download; otherwise File is a real jar stored in the pack
// (e.g. a manually dropped-in mod).
type IndexFile struct {
	File     string `toml:"file"`
	Hash     string `toml:"hash"`
	Metafile bool   `toml:"metafile"`
	Alias    string `toml:"alias"`
}

// Meta mirrors a mods/*.pw.toml metadata file.
type Meta struct {
	Name     string `toml:"name"`
	Filename string `toml:"filename"`
	Side     string `toml:"side"`
	Download struct {
		URL        string `toml:"url"`
		HashFormat string `toml:"hash-format"`
		Hash       string `toml:"hash"`
	} `toml:"download"`
}

func loadTOML(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return toml.Unmarshal(b, v)
}

// LoadPack reads <PackDir>/pack.toml.
func (c *CLI) LoadPack() (*Pack, error) {
	var p Pack
	if err := loadTOML(filepath.Join(c.PackDir, "pack.toml"), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// LoadIndex reads the index file referenced by pack.toml (default index.toml).
func (c *CLI) LoadIndex(p *Pack) (*Index, error) {
	name := p.Index.File
	if name == "" {
		name = "index.toml"
	}
	var idx Index
	if err := loadTOML(filepath.Join(c.PackDir, name), &idx); err != nil {
		return nil, err
	}
	return &idx, nil
}

// LoadMeta reads a .pw.toml metadata file given its pack-relative path.
func (c *CLI) LoadMeta(relPath string) (*Meta, error) {
	var m Meta
	if err := loadTOML(filepath.Join(c.PackDir, relPath), &m); err != nil {
		return nil, err
	}
	return &m, nil
}
