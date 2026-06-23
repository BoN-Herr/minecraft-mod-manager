package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

// configPath is the admin config file, kept next to where you run mcserver.
const configPath = "mmm.json"

// Config is the server-side project file (mmm.json).
type Config struct {
	Name         string `json:"name"`
	Author       string `json:"author"`
	Version      string `json:"version"`
	Minecraft    string `json:"minecraft"`
	Loader       string `json:"loader"`
	ForgeVersion string `json:"forgeVersion"` // resolved/exact, recorded after init
	PackDir      string `json:"packDir"`
	DistDir      string `json:"distDir"`
	Host         string `json:"host"`
	Port         int    `json:"port"`

	// Router / port-forwarding. PublicHost is the address friends OUTSIDE your
	// LAN use — a public IP or a DDNS hostname. PublicPort is the external port
	// on the router (after forwarding); 0 means "same as Port".
	PublicHost string `json:"publicHost"`
	PublicPort int    `json:"publicPort"`
}

// EffectivePublicPort is the external port friends connect to over the internet.
func (c *Config) EffectivePublicPort() int {
	if c.PublicPort != 0 {
		return c.PublicPort
	}
	return c.Port
}

func (c *Config) Save(path string) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no %s here — run `mcserver init` first", path)
		}
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.PackDir == "" {
		c.PackDir = "pack"
	}
	if c.DistDir == "" {
		c.DistDir = "dist"
	}
	if c.Host == "" {
		c.Host = "0.0.0.0"
	}
	if c.Port == 0 {
		c.Port = 8080
	}
	return &c, nil
}

// newFlagSet returns a flag set that prints errors but does not os.Exit on its
// own beyond the standard ContinueOnError behaviour.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	return fs
}
