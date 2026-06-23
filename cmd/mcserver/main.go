// Command mcserver is the admin + hosting tool that runs on your server.
//
// It wraps packwiz to assemble a Forge modpack (mods from Modrinth, CurseForge,
// or dropped in by hand), then "builds" the pack into a self-contained dist/
// folder (manifest.json + every jar) and serves it over HTTP. Friends point the
// client at this server and never touch Modrinth or CurseForge themselves.
package main

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mmm/internal/hashutil"
	"mmm/internal/manifest"
	"mmm/internal/packwiz"
)

const usage = `mcserver — build and host a Forge modpack for your friends

USAGE
  mcserver <command> [args]

PACK MANAGEMENT
  init                 Create the pack + config (Minecraft + Forge)
  add <slug...>        Add mod(s) from Modrinth (pinned to exact versions)
  add-cf <slug...>     Add mod(s) from CurseForge (needs CF API key, server-side only)
  import <file.jar...> Drop in a local jar (mods not on Modrinth/CurseForge)
  remove <name...>     Remove a mod
  list                 List mods in the pack

HOSTING
  build                Resolve + download every jar into dist/ and write manifest.json
  serve                Serve dist/ over HTTP for clients to pull
  info                 Show the address + instructions to give your friends

Run "mcserver init" first. Config lives in mmm.json.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "init":
		err = cmdInit(args)
	case "add":
		err = cmdPackwizInstall("modrinth", args)
	case "add-cf":
		err = cmdPackwizInstall("curseforge", args)
	case "import":
		err = cmdImport(args)
	case "remove":
		err = cmdRemove(args)
	case "list":
		err = cmdList()
	case "build":
		err = cmdBuild()
	case "serve":
		err = cmdServe(args)
	case "info":
		err = cmdInfo()
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		fmt.Print(usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// cli builds a packwiz handle pointed at the configured pack dir.
func cli(cfg *Config) (*packwiz.CLI, error) {
	bin, err := packwiz.Locate()
	if err != nil {
		return nil, err
	}
	return &packwiz.CLI{Bin: bin, PackDir: cfg.PackDir}, nil
}

// ---- init ----------------------------------------------------------------

func cmdInit(args []string) error {
	fs := newFlagSet("init")
	name := fs.String("name", "FriendsPack", "modpack name")
	author := fs.String("author", "", "modpack author")
	version := fs.String("version", "1.0.0", "modpack version")
	mc := fs.String("mc", "1.20.1", "Minecraft version")
	forge := fs.String("forge-version", "", "exact Forge version (blank = latest for that MC)")
	port := fs.Int("port", 8080, "local HTTP port to serve on")
	publicHost := fs.String("public-host", "", "public IP or DDNS hostname friends use over the internet")
	publicPort := fs.Int("public-port", 0, "external (forwarded) port on your router; 0 = same as --port")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg := &Config{
		Name: *name, Author: *author, Version: *version,
		Minecraft: *mc, Loader: "forge", ForgeVersion: *forge,
		PackDir: "pack", DistDir: "dist", Host: "0.0.0.0", Port: *port,
		PublicHost: *publicHost, PublicPort: *publicPort,
	}
	if err := cfg.Save(configPath); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.PackDir, 0o755); err != nil {
		return err
	}

	c, err := cli(cfg)
	if err != nil {
		return err
	}
	packArgs := []string{
		"init", "--name", cfg.Name, "--version", cfg.Version,
		"--mc-version", cfg.Minecraft, "--modloader", "forge", "-y", "-r",
	}
	if cfg.Author != "" {
		packArgs = append(packArgs, "--author", cfg.Author)
	}
	if cfg.ForgeVersion != "" {
		packArgs = append(packArgs, "--forge-version", cfg.ForgeVersion)
	} else {
		packArgs = append(packArgs, "--forge-latest")
	}
	if err := c.Run(packArgs...); err != nil {
		return err
	}
	// Record the resolved Forge version back into the config for reference.
	if p, err := c.LoadPack(); err == nil {
		if v := p.Versions["forge"]; v != "" {
			cfg.ForgeVersion = v
			_ = cfg.Save(configPath)
		}
	}
	fmt.Printf("\nInitialised %q (Minecraft %s, Forge %s).\n", cfg.Name, cfg.Minecraft, cfg.ForgeVersion)
	fmt.Println("Next: add mods with `mcserver add <slug>`, then `mcserver build` and `mcserver serve`.")
	return nil
}

// ---- mod management ------------------------------------------------------

func cmdPackwizInstall(source string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: mcserver %s <slug...>", map[string]string{"modrinth": "add", "curseforge": "add-cf"}[source])
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	c, err := cli(cfg)
	if err != nil {
		return err
	}
	for _, slug := range args {
		fmt.Printf("Adding %s from %s...\n", slug, source)
		if err := c.Run(source, "install", slug, "-y"); err != nil {
			return fmt.Errorf("adding %q: %w", slug, err)
		}
	}
	return nil
}

func cmdImport(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: mcserver import <file.jar...>")
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	modsDir := filepath.Join(cfg.PackDir, "mods")
	if err := os.MkdirAll(modsDir, 0o755); err != nil {
		return err
	}
	for _, src := range args {
		if !strings.HasSuffix(strings.ToLower(src), ".jar") {
			return fmt.Errorf("%q is not a .jar", src)
		}
		dst := filepath.Join(modsDir, filepath.Base(src))
		if err := copyFile(src, dst); err != nil {
			return err
		}
		fmt.Printf("Imported %s\n", filepath.Base(src))
	}
	c, err := cli(cfg)
	if err != nil {
		return err
	}
	return c.Refresh() // indexes the dropped-in jars
}

func cmdRemove(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: mcserver remove <name...>")
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	c, err := cli(cfg)
	if err != nil {
		return err
	}
	for _, name := range args {
		if err := c.Run("remove", name); err != nil {
			return err
		}
	}
	return nil
}

func cmdList() error {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	c, err := cli(cfg)
	if err != nil {
		return err
	}
	return c.Run("list")
}

// ---- build ---------------------------------------------------------------

func cmdBuild() error {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	c, err := cli(cfg)
	if err != nil {
		return err
	}
	if err := c.Refresh(); err != nil {
		return err
	}
	pack, err := c.LoadPack()
	if err != nil {
		return err
	}
	idx, err := c.LoadIndex(pack)
	if err != nil {
		return err
	}

	mc := pack.Versions["minecraft"]
	forge := pack.Versions["forge"]
	if mc == "" || forge == "" {
		return fmt.Errorf("pack.toml missing minecraft/forge versions")
	}

	distMods := filepath.Join(cfg.DistDir, "mods")
	if err := os.MkdirAll(distMods, 0o755); err != nil {
		return err
	}

	man := &manifest.Manifest{
		Schema:         manifest.Schema,
		Name:           pack.Name,
		Version:        pack.Version,
		Minecraft:      mc,
		Loader:         "forge",
		LoaderVersion:  forge,
		ForgeInstaller: forgeInstaller(mc, forge),
	}

	for _, f := range idx.Files {
		// We only ship mods to clients. Other categories (resourcepacks,
		// shaderpacks) could be added later; for now restrict to mods/.
		if !strings.HasPrefix(f.File, "mods/") {
			continue
		}
		if f.Metafile {
			meta, err := c.LoadMeta(f.File)
			if err != nil {
				return fmt.Errorf("reading %s: %w", f.File, err)
			}
			dst := filepath.Join(distMods, meta.Filename)
			if err := ensureDownloaded(dst, meta.Download.URL, meta.Download.HashFormat, meta.Download.Hash); err != nil {
				return fmt.Errorf("%s: %w", meta.Name, err)
			}
			sum, size, err := statHash(dst)
			if err != nil {
				return err
			}
			man.Mods = append(man.Mods, manifest.Mod{
				Filename: meta.Filename, Path: "mods/" + meta.Filename,
				SHA256: sum, Size: size, Side: defaultSide(meta.Side),
			})
		} else {
			// Raw jar stored in the pack (manual import): copy it across.
			src := filepath.Join(cfg.PackDir, f.File)
			base := filepath.Base(f.File)
			dst := filepath.Join(distMods, base)
			if err := copyFile(src, dst); err != nil {
				return fmt.Errorf("copying %s: %w", f.File, err)
			}
			sum, size, err := statHash(dst)
			if err != nil {
				return err
			}
			man.Mods = append(man.Mods, manifest.Mod{
				Filename: base, Path: "mods/" + base,
				SHA256: sum, Size: size, Side: "both",
			})
		}
		fmt.Printf("  ✓ %s\n", man.Mods[len(man.Mods)-1].Filename)
	}

	// Prune jars in dist that are no longer in the pack.
	pruneStaleMods(distMods, man.Mods)

	manPath := filepath.Join(cfg.DistDir, "manifest.json")
	if err := man.Save(manPath); err != nil {
		return err
	}
	fmt.Printf("\nBuilt %d mod(s) into %s\n", len(man.Mods), cfg.DistDir)
	fmt.Printf("Forge installer: %s\n", man.ForgeInstaller.URL)
	fmt.Println("Run `mcserver serve` to host it.")
	return nil
}

// forgeInstaller builds the official maven URL for the Forge client installer.
func forgeInstaller(mc, forge string) manifest.ForgeInstaller {
	combo := mc + "-" + forge
	name := "forge-" + combo + "-installer.jar"
	return manifest.ForgeInstaller{
		URL:      "https://maven.minecraftforge.net/net/minecraftforge/forge/" + combo + "/" + name,
		Filename: name,
	}
}

func defaultSide(s string) string {
	if s == "" {
		return "both"
	}
	return s
}

// ensureDownloaded fetches url to dst unless dst already matches the expected
// packwiz hash (so rebuilds are incremental).
func ensureDownloaded(dst, url, hashFmt, want string) error {
	if existing, err := verifyAgainst(dst, hashFmt, want); err == nil && existing {
		return nil // already have the exact bytes
	}
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	resp, err := http.Get(url)
	if err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	h := newHasher(hashFmt)
	w := io.MultiWriter(out, h)
	if _, err := io.Copy(w, resp.Body); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	out.Close()
	if want != "" {
		got := hex.EncodeToString(h.Sum(nil))
		if !strings.EqualFold(got, want) {
			os.Remove(tmp)
			return fmt.Errorf("hash mismatch (%s): got %s want %s", hashFmt, got, want)
		}
	}
	return os.Rename(tmp, dst)
}

func verifyAgainst(path, hashFmt, want string) (bool, error) {
	if want == "" {
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	h := newHasher(hashFmt)
	if _, err := io.Copy(h, f); err != nil {
		return false, err
	}
	return strings.EqualFold(hex.EncodeToString(h.Sum(nil)), want), nil
}

func newHasher(format string) hash.Hash {
	switch strings.ToLower(format) {
	case "sha512":
		return sha512.New()
	default:
		return sha256.New()
	}
}

func statHash(path string) (string, int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", 0, err
	}
	sum, err := hashutil.File(path)
	if err != nil {
		return "", 0, err
	}
	return sum, st.Size(), nil
}

func pruneStaleMods(dir string, keep []manifest.Mod) {
	want := map[string]bool{}
	for _, m := range keep {
		want[m.Filename] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".part") {
			continue
		}
		if !want[e.Name()] {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// ---- serve ---------------------------------------------------------------

func cmdServe(args []string) error {
	fs := newFlagSet("serve")
	port := fs.Int("port", 0, "override local port")
	forward := fs.Bool("forward", false, "try to open the router port automatically via UPnP")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	if *port != 0 {
		cfg.Port = *port
	}
	if _, err := os.Stat(filepath.Join(cfg.DistDir, "manifest.json")); err != nil {
		return fmt.Errorf("no build found in %s — run `mcserver build` first", cfg.DistDir)
	}

	if *forward {
		if err := tryForward(cfg); err != nil {
			fmt.Printf("  (UPnP auto-forward failed: %v — forward the port manually, see below)\n", err)
		} else {
			defer tryUnforward(cfg)
		}
	}

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	fileSrv := http.FileServer(http.Dir(cfg.DistDir))
	mux := http.NewServeMux()
	mux.Handle("/", logRequests(jarContentType(fileSrv)))

	fmt.Printf("Serving %s on port %d\n", cfg.DistDir, cfg.Port)
	printAccess(cfg)
	fmt.Println("  (Ctrl+C to stop)")

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return srv.ListenAndServe()
}

func jarContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".jar") {
			w.Header().Set("Content-Type", "application/java-archive")
		}
		next.ServeHTTP(w, r)
	})
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("  %s %s %s\n", time.Now().Format("15:04:05"), r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

// printAccess prints every way a friend can reach the server: same-LAN, and —
// when behind a router — the internet address plus the exact port-forward rule.
func printAccess(cfg *Config) {
	lan := localIPs()
	for _, ip := range lan {
		fmt.Printf("  Same-LAN friends:   mcclient.exe %s:%d\n", ip, cfg.Port)
	}
	extPort := cfg.EffectivePublicPort()
	if cfg.PublicHost != "" {
		fmt.Printf("  Internet friends:   mcclient.exe %s:%d\n", cfg.PublicHost, extPort)
	} else {
		fmt.Println("  Internet friends:   set a public address with `mcserver info --detect-ip`")
		fmt.Println("                      (or `mcserver init --public-host <ip-or-hostname>`)")
	}
	// Port-forward instruction (only meaningful for internet access).
	lanIP := "<this-machine-LAN-IP>"
	if len(lan) > 0 {
		lanIP = lan[0]
	}
	fmt.Printf("  Router port-forward: external TCP %d  ->  %s:%d\n", extPort, lanIP, cfg.Port)
}

// ---- info ----------------------------------------------------------------

func cmdInfo() error {
	fs := newFlagSet("info")
	detect := fs.Bool("detect-ip", false, "look up your public IP and save it as the public host")
	forward := fs.Bool("forward", false, "try to open the router port now via UPnP, then exit")
	_ = fs.Parse(os.Args[2:])

	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}

	if *detect {
		ip, err := detectPublicIP()
		if err != nil {
			fmt.Printf("Could not detect public IP: %v\n", err)
		} else {
			cfg.PublicHost = ip
			if err := cfg.Save(configPath); err != nil {
				return err
			}
			fmt.Printf("Detected public IP %s — saved as publicHost in %s.\n\n", ip, configPath)
		}
	}
	if *forward {
		if err := tryForward(cfg); err != nil {
			fmt.Printf("UPnP forward failed: %v\n", err)
		} else {
			fmt.Printf("UPnP: opened external TCP %d on your router.\n", cfg.EffectivePublicPort())
		}
	}

	fmt.Printf("Pack: %s %s (Minecraft %s, Forge %s)\n\n", cfg.Name, cfg.Version, cfg.Minecraft, cfg.ForgeVersion)
	fmt.Println("Give friends mcclient.exe and the matching address:")
	printAccess(cfg)
	return nil
}

// detectPublicIP asks a public echo service for this network's WAN IP.
func detectPublicIP() (string, error) {
	c := &http.Client{Timeout: 8 * time.Second}
	resp, err := c.Get("https://api.ipify.org")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ipify returned %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(b))
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("got %q, not an IP", ip)
	}
	return ip, nil
}

// ---- helpers -------------------------------------------------------------

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// localIPs returns non-loopback IPv4 addresses for friend-facing instructions.
func localIPs() []string {
	var out []string
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if v4 := ipnet.IP.To4(); v4 != nil {
				out = append(out, v4.String())
			}
		}
	}
	return out
}
