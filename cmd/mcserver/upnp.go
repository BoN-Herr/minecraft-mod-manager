package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// This is a small, best-effort UPnP-IGD client (stdlib only). Many home routers
// support it; some have it disabled. On any failure we fall back to telling the
// admin to forward the port by hand.

func firstLANIP() string {
	ips := localIPs()
	if len(ips) == 0 {
		return ""
	}
	return ips[0]
}

// tryForward asks the router to map external TCP <publicPort> to this machine's
// <port>.
func tryForward(cfg *Config) error {
	lan := firstLANIP()
	if lan == "" {
		return fmt.Errorf("could not determine this machine's LAN IP")
	}
	ctrl, svc, err := upnpDiscover()
	if err != nil {
		return err
	}
	if err := upnpAddPortMapping(ctrl, svc, cfg.EffectivePublicPort(), cfg.Port, lan, "MinecraftModManager"); err != nil {
		return err
	}
	fmt.Printf("  UPnP: opened external TCP %d -> %s:%d\n", cfg.EffectivePublicPort(), lan, cfg.Port)
	return nil
}

// tryUnforward removes the mapping on shutdown (best effort, ignored on error).
func tryUnforward(cfg *Config) {
	ctrl, svc, err := upnpDiscover()
	if err != nil {
		return
	}
	_ = upnpDeletePortMapping(ctrl, svc, cfg.EffectivePublicPort())
}

// ---- SSDP discovery ------------------------------------------------------

func upnpDiscover() (controlURL, serviceType string, err error) {
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return "", "", err
	}
	defer conn.Close()
	mcast := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}

	targets := []string{
		"urn:schemas-upnp-org:device:InternetGatewayDevice:1",
		"urn:schemas-upnp-org:service:WANIPConnection:1",
		"urn:schemas-upnp-org:service:WANPPPConnection:1",
	}
	for _, st := range targets {
		msg := "M-SEARCH * HTTP/1.1\r\n" +
			"HOST: 239.255.255.250:1900\r\n" +
			"MAN: \"ssdp:discover\"\r\n" +
			"MX: 2\r\n" +
			"ST: " + st + "\r\n\r\n"
		_, _ = conn.WriteTo([]byte(msg), mcast)
	}

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	seen := map[string]bool{}
	buf := make([]byte, 2048)
	for {
		n, _, rerr := conn.ReadFrom(buf)
		if rerr != nil {
			break // deadline reached
		}
		loc := headerValue(string(buf[:n]), "LOCATION")
		if loc == "" || seen[loc] {
			continue
		}
		seen[loc] = true
		if ctrl, svc, ok := wanServiceFromDescription(loc); ok {
			return ctrl, svc, nil
		}
	}
	return "", "", fmt.Errorf("no UPnP internet gateway found (router may have UPnP disabled)")
}

func headerValue(resp, key string) string {
	for _, line := range strings.Split(resp, "\n") {
		if i := strings.Index(line, ":"); i > 0 {
			if strings.EqualFold(strings.TrimSpace(line[:i]), key) {
				return strings.TrimSpace(line[i+1:])
			}
		}
	}
	return ""
}

// ---- device description --------------------------------------------------

type upnpService struct {
	ServiceType string `xml:"serviceType"`
	ControlURL  string `xml:"controlURL"`
}

type upnpDevice struct {
	Services   []upnpService `xml:"serviceList>service"`
	SubDevices []upnpDevice  `xml:"deviceList>device"`
}

type upnpRoot struct {
	XMLName xml.Name   `xml:"root"`
	URLBase string     `xml:"URLBase"`
	Device  upnpDevice `xml:"device"`
}

func wanServiceFromDescription(location string) (controlURL, serviceType string, ok bool) {
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(location)
	if err != nil {
		return "", "", false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", false
	}
	var root upnpRoot
	if err := xml.Unmarshal(body, &root); err != nil {
		return "", "", false
	}
	svcType, ctrl := findWANService(&root.Device)
	if ctrl == "" {
		return "", "", false
	}
	base := location
	if root.URLBase != "" {
		base = root.URLBase
	}
	abs, err := resolveURL(base, ctrl)
	if err != nil {
		return "", "", false
	}
	return abs, svcType, true
}

func findWANService(d *upnpDevice) (serviceType, controlURL string) {
	for _, s := range d.Services {
		if strings.Contains(s.ServiceType, "WANIPConnection") || strings.Contains(s.ServiceType, "WANPPPConnection") {
			return s.ServiceType, s.ControlURL
		}
	}
	for i := range d.SubDevices {
		if st, cu := findWANService(&d.SubDevices[i]); cu != "" {
			return st, cu
		}
	}
	return "", ""
}

func resolveURL(base, ref string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	return b.ResolveReference(r).String(), nil
}

// ---- SOAP actions --------------------------------------------------------

func upnpAddPortMapping(controlURL, serviceType string, extPort, intPort int, intClient, desc string) error {
	body := fmt.Sprintf(`<NewRemoteHost></NewRemoteHost>`+
		`<NewExternalPort>%d</NewExternalPort>`+
		`<NewProtocol>TCP</NewProtocol>`+
		`<NewInternalPort>%d</NewInternalPort>`+
		`<NewInternalClient>%s</NewInternalClient>`+
		`<NewEnabled>1</NewEnabled>`+
		`<NewPortMappingDescription>%s</NewPortMappingDescription>`+
		`<NewLeaseDuration>0</NewLeaseDuration>`,
		extPort, intPort, intClient, desc)
	return upnpSOAP(controlURL, serviceType, "AddPortMapping", body)
}

func upnpDeletePortMapping(controlURL, serviceType string, extPort int) error {
	body := fmt.Sprintf(`<NewRemoteHost></NewRemoteHost>`+
		`<NewExternalPort>%d</NewExternalPort>`+
		`<NewProtocol>TCP</NewProtocol>`, extPort)
	return upnpSOAP(controlURL, serviceType, "DeletePortMapping", body)
}

func upnpSOAP(controlURL, serviceType, action, innerXML string) error {
	envelope := `<?xml version="1.0"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" ` +
		`s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
		`<s:Body><u:` + action + ` xmlns:u="` + serviceType + `">` +
		innerXML +
		`</u:` + action + `></s:Body></s:Envelope>`

	req, err := http.NewRequest(http.MethodPost, controlURL, bytes.NewReader([]byte(envelope)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+serviceType+"#"+action+`"`)

	c := &http.Client{Timeout: 6 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("router rejected %s (%s): %s", action, resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}
