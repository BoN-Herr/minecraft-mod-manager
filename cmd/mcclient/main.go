// Command mcclient is the zero-setup tool your friends run on Windows.
//
//	mcclient <server-address>
//
// It reads the manifest from your server, installs the matching Forge version,
// creates a dedicated game instance (so it never disturbs their normal
// Minecraft), and downloads the mods straight from your server. Re-running it
// syncs the instance to whatever the server currently has.
//
// It only ever talks to your server (plus Forge's maven for the loader, and —
// only if no Java is found — Adoptium for a portable JRE). No Modrinth, no
// CurseForge, no launcher mod-manager.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"mmm/internal/manifest"
)

func main() {
	fmt.Println("=== Minecraft Mod Manager — client ===")
	addr := ""
	modsOnly := false
	for _, a := range os.Args[1:] {
		switch a {
		case "--mods-only":
			modsOnly = true
		case "-h", "--help":
			fmt.Println("Usage: mcclient <server-address> [--mods-only]")
			return
		default:
			if !strings.HasPrefix(a, "-") {
				addr = a
			}
		}
	}
	if addr == "" {
		addr = prompt("Enter the server address (e.g. 192.168.1.50:8080): ")
	}
	if err := run(addr, modsOnly); err != nil {
		fmt.Fprintln(os.Stderr, "\nERROR:", err)
		fmt.Println("\nPress Enter to close.")
		bufio.NewReader(os.Stdin).ReadString('\n')
		os.Exit(1)
	}
	fmt.Println("\nPress Enter to close.")
	bufio.NewReader(os.Stdin).ReadString('\n')
}

func run(addr string, modsOnly bool) error {
	base, err := normalizeBase(addr)
	if err != nil {
		return err
	}
	fmt.Printf("Server: %s\n", base)

	man, err := fetchManifest(base)
	if err != nil {
		return err
	}
	if man.Schema != manifest.Schema {
		return fmt.Errorf("server manifest schema %d not supported by this client (expected %d) — update the client", man.Schema, manifest.Schema)
	}
	if man.Loader != "forge" {
		return fmt.Errorf("unsupported loader %q", man.Loader)
	}
	fmt.Printf("Pack:   %s %s  (Minecraft %s, Forge %s)\n", man.Name, man.Version, man.Minecraft, man.LoaderVersion)

	slug := slugify(man.Name)
	mcDir := minecraftDir()
	instanceDir := instanceDir(slug)
	fmt.Printf("Instance: %s\n", instanceDir)
	if err := os.MkdirAll(filepath.Join(instanceDir, "mods"), 0o755); err != nil {
		return err
	}

	versionID := man.Minecraft + "-forge-" + man.LoaderVersion

	if !modsOnly {
		if forgeInstalled(mcDir, versionID) {
			fmt.Printf("Forge %s already installed — skipping.\n", man.LoaderVersion)
		} else {
			fmt.Println("\n[1/3] Locating Java...")
			java, err := ensureJava(instanceDir)
			if err != nil {
				return fmt.Errorf("Java: %w", err)
			}
			fmt.Printf("      Using: %s\n", java)

			fmt.Println("[2/3] Installing Forge (this can take a minute)...")
			if err := installForge(java, mcDir, man.ForgeInstaller.URL, man.ForgeInstaller.Filename, instanceDir); err != nil {
				return fmt.Errorf("Forge install: %w", err)
			}
		}
		if err := ensureLauncherProfile(mcDir, slug, man.Name, versionID, instanceDir); err != nil {
			// Non-fatal: mods still sync; friend can pick the version manually.
			fmt.Printf("      (warning: could not update launcher profile: %v)\n", err)
		}
	}

	fmt.Println("[3/3] Syncing mods...")
	if err := syncMods(base, man, instanceDir); err != nil {
		return err
	}

	fmt.Printf("\n✓ Done. Open the Minecraft Launcher, choose the \"%s\" installation, and play.\n", man.Name)
	return nil
}

// ---- manifest + mods over HTTP ------------------------------------------

func fetchManifest(base string) (*manifest.Manifest, error) {
	u := base + "/manifest.json"
	resp, err := http.Get(u)
	if err != nil {
		return nil, fmt.Errorf("reaching server at %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned %s for %s", resp.Status, u)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	return manifest.Parse(b)
}

func syncMods(base string, man *manifest.Manifest, instanceDir string) error {
	modsDir := filepath.Join(instanceDir, "mods")
	want := map[string]string{} // filename -> sha256
	for _, m := range man.Mods {
		if !m.WantedOnClient() {
			continue
		}
		want[m.Filename] = m.SHA256
		dst := filepath.Join(modsDir, m.Filename)
		if sha256File(dst) == m.SHA256 {
			fmt.Printf("      = %s\n", m.Filename)
			continue
		}
		fmt.Printf("      ↓ %s\n", m.Filename)
		if err := downloadVerified(base+"/"+m.Path, dst, m.SHA256); err != nil {
			return fmt.Errorf("%s: %w", m.Filename, err)
		}
	}
	// Remove mods the server no longer lists, so the instance stays in sync.
	entries, _ := os.ReadDir(modsDir)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, keep := want[e.Name()]; !keep && strings.HasSuffix(e.Name(), ".jar") {
			fmt.Printf("      ✗ %s (removed on server)\n", e.Name())
			os.Remove(filepath.Join(modsDir, e.Name()))
		}
	}
	return nil
}

// downloadVerified streams url to dst and fails if the sha256 doesn't match.
func downloadVerified(url, dst, wantSHA string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), resp.Body); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	out.Close()
	if wantSHA != "" {
		if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, wantSHA) {
			os.Remove(tmp)
			return fmt.Errorf("hash mismatch: got %s want %s", got, wantSHA)
		}
	}
	return os.Rename(tmp, dst)
}

// ---- small helpers -------------------------------------------------------

// normalizeBase turns "1.2.3.4", "1.2.3.4:8080", or "http://host:8080" into a
// clean base URL, defaulting to port 8080 when none is given.
func normalizeBase(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", fmt.Errorf("no server address given")
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid address %q", addr)
	}
	if u.Port() == "" {
		u.Host = u.Host + ":8080"
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

func sha256File(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func slugify(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteRune('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "pack"
	}
	return s
}

func prompt(msg string) string {
	fmt.Print(msg)
	r := bufio.NewReader(os.Stdin)
	line, _ := r.ReadString('\n')
	return strings.TrimSpace(line)
}
