// Package manifest defines the JSON document that the server publishes and the
// client consumes. It is the single contract between the two tools: the server
// resolves a packwiz pack into this, and the client reads it to reproduce the
// instance. Keep it self-contained (stdlib only) so both binaries stay small.
package manifest

import (
	"encoding/json"
	"os"
)

// Schema is bumped when the manifest layout changes in a breaking way. The
// client refuses manifests it does not understand.
const Schema = 1

// Manifest is served at <server>/manifest.json. Every mod path is relative to
// the manifest's own URL, so the client only ever talks to the server.
type Manifest struct {
	Schema    int    `json:"schema"`
	Name      string `json:"name"`
	Version   string `json:"version"` // pack version, e.g. "1.0.0"
	Minecraft string `json:"minecraft"`
	Loader    string `json:"loader"` // "forge" for now

	// LoaderVersion is the resolved Forge version (e.g. "47.4.20"), no MC prefix.
	LoaderVersion string `json:"loaderVersion"`

	// Forge installer coordinates, precomputed so the client needs no logic.
	ForgeInstaller ForgeInstaller `json:"forgeInstaller"`

	Mods []Mod `json:"mods"`
}

// ForgeInstaller points at the official Forge maven installer jar.
type ForgeInstaller struct {
	URL      string `json:"url"`
	Filename string `json:"filename"`
	// SHA256 is optional; Forge's maven does not always expose a stable hash,
	// so an empty value means "skip verification of the installer".
	SHA256 string `json:"sha256,omitempty"`
}

// Mod is one jar the client must place in <instance>/mods/.
type Mod struct {
	Filename string `json:"filename"`
	Path     string `json:"path"`   // server-relative, e.g. "mods/jei-....jar"
	SHA256   string `json:"sha256"` // recomputed by the server over the real bytes
	Size     int64  `json:"size"`
	Side     string `json:"side"` // "both" | "client" | "server"
}

// WantedOnClient reports whether this mod belongs in a client install.
// Server-only mods are skipped by the client.
func (m Mod) WantedOnClient() bool {
	return m.Side != "server"
}

// Save writes the manifest as indented JSON.
func (m *Manifest) Save(path string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// Parse decodes a manifest from raw JSON bytes (used by the client over HTTP).
func Parse(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
