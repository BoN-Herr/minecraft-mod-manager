package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// minecraftDir returns the real .minecraft the official launcher uses. Forge
// and launcher_profiles.json live here (shared across all instances).
func minecraftDir() string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(os.Getenv("APPDATA"), ".minecraft")
	case "darwin":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, "Library", "Application Support", "minecraft")
	default:
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".minecraft")
	}
}

// instanceDir returns a dedicated game directory (mods/config/saves) for this
// pack, kept separate from the player's normal Minecraft.
func instanceDir(slug string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("APPDATA"), "mmm-packs", slug)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".mmm-packs", slug)
}

// forgeInstalled reports whether the Forge version profile already exists.
func forgeInstalled(mcDir, versionID string) bool {
	p := filepath.Join(mcDir, "versions", versionID, versionID+".json")
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// ---- Java ----------------------------------------------------------------

// ensureJava finds a Java >= 17, searching PATH and the launcher's bundled
// runtimes, and downloads a portable Adoptium JRE into the instance if none is
// found. Returns the path to the java executable.
func ensureJava(workDir string) (string, error) {
	if j, ok := findSystemJava(); ok {
		return j, nil
	}
	fmt.Println("      No suitable Java found — downloading a portable Java 17 (one-time)...")
	return downloadJRE(workDir)
}

func javaExeNames() []string {
	if runtime.GOOS == "windows" {
		return []string{"javaw.exe", "java.exe"}
	}
	return []string{"java"}
}

// findSystemJava checks PATH, then the Minecraft launcher's bundled JREs.
func findSystemJava() (string, bool) {
	if p, err := exec.LookPath("java"); err == nil && javaAtLeast17(p) {
		return p, true
	}
	for _, root := range launcherRuntimeRoots() {
		if j, ok := searchForJava(root); ok {
			return j, true
		}
	}
	return "", false
}

// launcherRuntimeRoots lists where the official launcher stores bundled JREs.
func launcherRuntimeRoots() []string {
	var roots []string
	switch runtime.GOOS {
	case "windows":
		local := os.Getenv("LOCALAPPDATA")
		roots = append(roots,
			filepath.Join("C:\\", "Program Files (x86)", "Minecraft Launcher", "runtime"),
			filepath.Join(local, "Packages", "Microsoft.4297127D64EC6_8wekyb3d8bbwe", "LocalCache", "Local", "runtime"),
			filepath.Join(os.Getenv("APPDATA"), ".minecraft", "runtime"),
		)
	case "darwin":
		home, _ := os.UserHomeDir()
		roots = append(roots, filepath.Join(home, "Library", "Application Support", "minecraft", "runtime"))
	default:
		home, _ := os.UserHomeDir()
		roots = append(roots, filepath.Join(home, ".minecraft", "runtime"))
	}
	return roots
}

// searchForJava walks a runtime root looking for a java executable >= 17.
func searchForJava(root string) (string, bool) {
	names := javaExeNames()
	var found string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		for _, n := range names {
			if strings.EqualFold(d.Name(), n) {
				if javaAtLeast17(p) {
					found = p
					return io.EOF // stop walking
				}
			}
		}
		return nil
	})
	return found, found != ""
}

var javaVerRe = regexp.MustCompile(`version "(\d+)(?:\.(\d+))?`)

// javaAtLeast17 runs `<java> -version` and parses the major version.
func javaAtLeast17(javaPath string) bool {
	// Prefer the non-windowed java for -version output if given javaw.
	probe := javaPath
	if runtime.GOOS == "windows" && strings.EqualFold(filepath.Base(javaPath), "javaw.exe") {
		alt := filepath.Join(filepath.Dir(javaPath), "java.exe")
		if _, err := os.Stat(alt); err == nil {
			probe = alt
		}
	}
	out, _ := exec.Command(probe, "-version").CombinedOutput()
	m := javaVerRe.FindStringSubmatch(string(out))
	if len(m) < 2 {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	if major == 1 && len(m) >= 3 { // old "1.8" scheme
		major, _ = strconv.Atoi(m[2])
	}
	return major >= 17
}

// downloadJRE fetches a portable Adoptium (Temurin) JRE 17 into workDir/.runtime
// and returns the path to its java executable.
func downloadJRE(workDir string) (string, error) {
	osName := map[string]string{"windows": "windows", "darwin": "mac", "linux": "linux"}[runtime.GOOS]
	arch := map[string]string{"amd64": "x64", "arm64": "aarch64"}[runtime.GOARCH]
	if osName == "" || arch == "" {
		return "", fmt.Errorf("no portable JRE available for %s/%s — please install Java 17", runtime.GOOS, runtime.GOARCH)
	}
	url := fmt.Sprintf("https://api.adoptium.net/v3/binary/latest/17/ga/%s/%s/jre/hotspot/normal/eclipse", osName, arch)
	dest := filepath.Join(workDir, ".runtime")
	os.RemoveAll(dest)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}

	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading JRE: %s", resp.Status)
	}

	if runtime.GOOS == "windows" {
		if err := unzipStream(resp.Body, dest); err != nil {
			return "", err
		}
	} else {
		if err := untarGz(resp.Body, dest); err != nil {
			return "", err
		}
	}
	if j, ok := searchForJava(dest); ok {
		return j, nil
	}
	return "", fmt.Errorf("downloaded JRE but could not locate java executable")
}

// ---- Forge ---------------------------------------------------------------

// installForge downloads the Forge installer and runs it headlessly against the
// shared .minecraft, which registers the Forge version + libraries there.
func installForge(java, mcDir, installerURL, installerName, workDir string) error {
	cacheDir := filepath.Join(workDir, ".cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}
	installer := filepath.Join(cacheDir, installerName)
	if st, err := os.Stat(installer); err != nil || st.Size() == 0 {
		fmt.Printf("      Downloading %s...\n", installerName)
		if err := downloadVerified(installerURL, installer, ""); err != nil {
			return err
		}
	}

	// The Forge installer refuses to run unless launcher_profiles.json exists.
	if err := ensureLauncherProfilesFile(mcDir); err != nil {
		return err
	}
	if err := os.MkdirAll(mcDir, 0o755); err != nil {
		return err
	}

	cmd := exec.Command(java, "-jar", installer, "--installClient", mcDir)
	cmd.Dir = cacheDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ---- launcher profile ----------------------------------------------------

// ensureLauncherProfilesFile creates a minimal launcher_profiles.json if the
// player has never opened the launcher.
func ensureLauncherProfilesFile(mcDir string) error {
	p := filepath.Join(mcDir, "launcher_profiles.json")
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	if err := os.MkdirAll(mcDir, 0o755); err != nil {
		return err
	}
	base := map[string]any{
		"profiles": map[string]any{},
		"settings": map[string]any{},
		"version":  3,
	}
	return writeJSON(p, base)
}

// ensureLauncherProfile adds/updates an installation entry so the pack shows up
// in the vanilla launcher, pointing at our instance directory.
func ensureLauncherProfile(mcDir, slug, name, versionID, instanceDir string) error {
	if err := ensureLauncherProfilesFile(mcDir); err != nil {
		return err
	}
	p := filepath.Join(mcDir, "launcher_profiles.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	profiles, ok := doc["profiles"].(map[string]any)
	if !ok {
		profiles = map[string]any{}
		doc["profiles"] = profiles
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	profiles["mmm-"+slug] = map[string]any{
		"name":          name,
		"type":          "custom",
		"created":       now,
		"lastUsed":      now,
		"lastVersionId": versionID,
		"gameDir":       instanceDir,
		"javaArgs":      "-Xmx4G -XX:+UseG1GC",
	}
	return writeJSON(p, doc)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// ---- archive extraction --------------------------------------------------

func unzipStream(r io.Reader, dest string) error {
	tmp, err := os.CreateTemp("", "jre-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()
	zr, err := zip.OpenReader(tmp.Name())
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		target := filepath.Join(dest, f.Name) //nolint:gosec // trusted Adoptium archive
		if f.FileInfo().IsDir() {
			os.MkdirAll(target, 0o755)
			continue
		}
		os.MkdirAll(filepath.Dir(target), 0o755)
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			rc.Close()
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			rc.Close()
			out.Close()
			return err
		}
		rc.Close()
		out.Close()
	}
	return nil
}

func untarGz(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dest, hdr.Name) //nolint:gosec // trusted Adoptium archive
		switch hdr.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, 0o755)
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0o755)
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		}
	}
}
