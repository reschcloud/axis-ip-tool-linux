// SPDX-License-Identifier: GPL-3.0-or-later

// axisctl – Kommandozeilenwerkzeug zum Finden und Konfigurieren von Axis-Geräten.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"
	"unsafe"

	"axis-ip-tool/internal/axis"
)

// version wird beim Bauen gesetzt (-ldflags "-X main.version=…", siehe Makefile).
var version = "dev"

const usage = `axisctl – Axis-Geräte finden und konfigurieren

Aufruf: axisctl [--https-only] BEFEHL …

Befehle:
  list     [-t 5s]                                  Geräte im Netz suchen
  info     <ip>                                     Modell, Firmware, Netzwerk anzeigen
  init     <ip> [-u root]                           Ersten Admin anlegen (Werkszustand)
  net      <ip> (-dhcp | -ip A/PRÄFIX [-gw G])
                [-hostname H] [-dns A,B]            Netzwerk konfigurieren
  time     <ip> [-tz Europe/Vienna] [-sync=true]
                [-ntp dhcp|off|SERVER,SERVER]       Zeitzone/Uhrzeit/NTP setzen
  restart  <ip>                                     Neustart
  reset    <ip> [-all] [-y]                         Werkseinstellungen (-all: auch IP-Einstellungen)
  batch    [GERÄT …] [-csv PLAN] …                  Mehrere Geräte konfigurieren (axisctl batch -h)
  version                                           Version anzeigen

Anmeldung: -u BENUTZER (Standard root), Passwort aus $AXIS_PASSWORD oder Abfrage.
Verbindung: HTTPS, wenn das Gerät es anbietet (Zertifikat wird nicht geprüft), sonst HTTP.
  --https-only bzw. AXIS_HTTPS_ONLY=1 verbietet den Rückfall auf HTTP.
`

func main() {
	if v := os.Getenv("AXIS_HTTPS_ONLY"); v == "1" || v == "true" || v == "yes" {
		axis.SetHTTPSOnly(true)
	}
	if len(os.Args) > 1 && (os.Args[1] == "--https-only" || os.Args[1] == "-https-only") {
		axis.SetHTTPSOnly(true)
		os.Args = append(os.Args[:1], os.Args[2:]...)
	}
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "list", "ls":
		err = cmdList(ctx, args)
	case "info":
		err = cmdInfo(ctx, args)
	case "init":
		err = cmdInit(ctx, args)
	case "net":
		err = cmdNet(ctx, args)
	case "time":
		err = cmdTime(ctx, args)
	case "restart":
		err = cmdRestart(ctx, args)
	case "reset":
		err = cmdReset(ctx, args)
	case "batch":
		err = cmdBatch(ctx, args)
	case "version", "--version":
		fmt.Println("axisctl", version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		err = fmt.Errorf("unbekannter Befehl %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Fehler:", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------- list

func cmdList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	t := fs.String("t", "5s", "Suchdauer (z. B. 5 oder 10s)")
	fs.Parse(args)
	list, err := discover(ctx, *t)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tHOSTNAME\tIP\tSERIENNUMMER\tMODELL\tFIRMWARE\tVERBINDUNG\tSTATUS")
	for _, d := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", d.Name, d.Hostname, d.IP, d.Serial, d.Model, d.Firmware, d.Connection(), d.Status())
	}
	tw.Flush()
	if len(list) == 0 {
		fmt.Fprintln(os.Stderr, "Keine Geräte gefunden.")
	}
	return nil
}

// discover sucht für die Dauer t (z. B. "5" oder "10s") nach Geräten.
func discover(ctx context.Context, t string) ([]*axis.Device, error) {
	var dur time.Duration
	if n, err := strconv.Atoi(t); err == nil {
		dur = time.Duration(n) * time.Second
	} else if dur, err = time.ParseDuration(t); err != nil {
		return nil, fmt.Errorf("ungültige Suchdauer %q", t)
	}
	sctx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()
	var mu sync.Mutex
	reg := axis.NewRegistry()
	axis.Discover(sctx, func(d axis.Device) {
		mu.Lock()
		reg.Update(d)
		mu.Unlock()
	}, func(err error) { fmt.Fprintln(os.Stderr, "Hinweis:", err) })
	<-sctx.Done()
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	return reg.All(), ctx.Err()
}

// ---------------------------------------------------------------- info

func cmdInfo(ctx context.Context, args []string) error {
	fs, user := authFlags("info")
	fs.Parse(reorder(args))
	ip, err := oneArg(fs)
	if err != nil {
		return err
	}
	info, err := axis.NewClient(ip, 80, "", "").BasicInfo(ctx)
	var c *axis.Client
	if errors.Is(err, axis.ErrAuth) { // neuere Firmware: nur mit Anmeldung
		if c, err = login(ctx, ip, *user); err != nil {
			return err
		}
		info, err = c.BasicInfo(ctx)
	}
	if err != nil {
		return err
	}
	fmt.Printf("Modell:        %s\nSeriennummer:  %s\nFirmware:      %s\n", info.Model, info.Serial, info.Firmware)
	conn := "HTTP (unverschlüsselt)"
	if axis.NewClient(ip, 80, "", "").Secure(ctx) {
		conn = "HTTPS"
	}
	fmt.Printf("Verbindung:    %s\n", conn)
	if needs, _ := axis.NewClient(ip, 80, "", "").NeedsInitialUser(ctx); needs {
		fmt.Println("Status:        Werkszustand, noch kein Passwort gesetzt (axisctl init)")
		return nil
	}
	if c == nil {
		if c, err = login(ctx, ip, *user); err != nil {
			return err
		}
	}
	n, err := c.GetNetwork(ctx)
	if err != nil {
		return err
	}
	mode := "statisch"
	if n.DHCP {
		mode = "DHCP"
	}
	fmt.Printf("IPv4:          %s/%d (%s), Gateway %s\n", n.Address, n.Prefix, mode, n.Gateway)
	fmt.Printf("Hostname:      %s%s\n", n.Hostname, map[bool]string{true: " (DHCP)"}[n.HostnameFromDHCP])
	fmt.Printf("DNS:           %s%s\n", strings.Join(n.DNS, ", "), map[bool]string{true: " (DHCP)"}[n.DNSFromDHCP])
	if t, err := c.GetNTP(ctx); err == nil {
		fmt.Printf("NTP:           %s\n", describeNTP(t))
	}
	return nil
}

// ---------------------------------------------------------------- init

func cmdInit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	user := fs.String("u", "root", "Benutzername des Administrators")
	fs.Parse(reorder(args))
	ip, err := oneArg(fs)
	if err != nil {
		return err
	}
	pw := os.Getenv("AXIS_PASSWORD")
	if pw == "" {
		if pw, err = readPassword("Neues Passwort: "); err != nil {
			return err
		}
		pw2, err := readPassword("Wiederholen: ")
		if err != nil {
			return err
		}
		if pw != pw2 {
			return errors.New("Passwörter stimmen nicht überein")
		}
	}
	if len(pw) < 8 {
		return errors.New("Passwort zu kurz (mindestens 8 Zeichen)")
	}
	c := axis.NewClient(ip, 80, "", "")
	if !c.Secure(ctx) && !axis.HTTPSOnly() {
		fmt.Fprintln(os.Stderr, "Warnung: Gerät bietet kein HTTPS – das Passwort wird unverschlüsselt übertragen.")
	}
	if err := c.CreateInitialUser(ctx, *user, pw); err != nil {
		return err
	}
	fmt.Printf("Benutzer %q angelegt.\n", *user)
	return nil
}

// ---------------------------------------------------------------- net

func cmdNet(ctx context.Context, args []string) error {
	fs, user := authFlags("net")
	dhcp := fs.Bool("dhcp", false, "IPv4 per DHCP")
	addr := fs.String("ip", "", "statische Adresse, z. B. 192.168.1.20/24")
	gw := fs.String("gw", "", "Standard-Gateway")
	host := fs.String("hostname", "", "statischer Hostname (\"dhcp\" = per DHCP)")
	dns := fs.String("dns", "", "DNS-Server, kommagetrennt (\"dhcp\" = per DHCP)")
	fs.Parse(reorder(args))
	ip, err := oneArg(fs)
	if err != nil {
		return err
	}
	c, err := login(ctx, ip, *user)
	if err != nil {
		return err
	}
	cfg, err := c.GetNetwork(ctx)
	if err != nil {
		return err
	}
	switch {
	case *dhcp:
		cfg.DHCP = true
	case *addr != "":
		a, p, ok := strings.Cut(*addr, "/")
		cfg.DHCP, cfg.Address = false, a
		if ok {
			if cfg.Prefix, err = axis.ParsePrefix(p); err != nil {
				return err
			}
		}
		if *gw != "" {
			cfg.Gateway = *gw
		}
	}
	switch *host {
	case "":
	case "dhcp":
		cfg.HostnameFromDHCP = true
	default:
		cfg.HostnameFromDHCP, cfg.Hostname = false, *host
	}
	switch *dns {
	case "":
	case "dhcp":
		cfg.DNSFromDHCP = true
	default:
		cfg.DNSFromDHCP, cfg.DNS = false, strings.Split(*dns, ",")
	}
	if err := c.SetNetwork(ctx, cfg); err != nil {
		return err
	}
	fmt.Println("Netzwerkeinstellungen übernommen.")
	return nil
}

// ---------------------------------------------------------------- time / restart

func cmdTime(ctx context.Context, args []string) error {
	fs, user := authFlags("time")
	tz := fs.String("tz", localTZ(), "Zeitzone (IANA)")
	sync := fs.Bool("sync", true, "Uhrzeit von diesem Rechner übernehmen (ignoriert, wenn NTP aktiv)")
	ntp := fs.String("ntp", "", "NTP: \"dhcp\", \"off\" oder Server (kommagetrennt)")
	fs.Parse(reorder(args))
	ip, err := oneArg(fs)
	if err != nil {
		return err
	}
	c, err := login(ctx, ip, *user)
	if err != nil {
		return err
	}
	if *ntp != "" {
		cfg := axis.NTPConfig{Enabled: true}
		switch strings.ToLower(*ntp) {
		case "off", "aus":
			cfg.Enabled = false
		case "dhcp":
			cfg.FromDHCP = true
		default:
			cfg.Servers = strings.Split(*ntp, ",")
		}
		if old, err := c.GetNTP(ctx); err == nil {
			cfg.Legacy = old.Legacy
			if !cfg.Enabled {
				cfg.FromDHCP, cfg.Servers = old.FromDHCP, old.Servers
			}
		}
		if err := c.SetNTP(ctx, cfg); err != nil {
			return err
		}
		*sync = *sync && !cfg.Enabled
	} else if t, err := c.GetNTP(ctx); err == nil && t.Enabled {
		*sync = false // Uhrzeit kommt per NTP
	}
	if err := c.SetTime(ctx, *tz, *sync); err != nil {
		return err
	}
	if t, err := c.GetNTP(ctx); err == nil {
		fmt.Println("NTP:", describeNTP(t))
	}
	fmt.Println("Datum & Zeit gesetzt.")
	return nil
}

func cmdRestart(ctx context.Context, args []string) error {
	fs, user := authFlags("restart")
	fs.Parse(reorder(args))
	ip, err := oneArg(fs)
	if err != nil {
		return err
	}
	c, err := login(ctx, ip, *user)
	if err != nil {
		return err
	}
	if err := c.Restart(ctx); err != nil {
		return err
	}
	fmt.Println("Neustart ausgelöst.")
	return nil
}

func cmdReset(ctx context.Context, args []string) error {
	fs, user := authFlags("reset")
	all := fs.Bool("all", false, "auch Netzwerkeinstellungen zurücksetzen (Gerät bekommt neue IP)")
	yes := fs.Bool("y", false, "ohne Rückfrage")
	fs.Parse(reorder(args))
	ip, err := oneArg(fs)
	if err != nil {
		return err
	}
	c, err := login(ctx, ip, *user)
	if err != nil {
		return err
	}
	what := "Alle Einstellungen, Benutzer und Passwörter (IP-Einstellungen bleiben)"
	if *all {
		what = "Alle Einstellungen, Benutzer, Passwörter UND die IP-Einstellungen"
	}
	if !*yes && !confirm(fmt.Sprintf("%s auf %s werden gelöscht. Fortfahren? [j/N] ", what, ip)) {
		return errors.New("abgebrochen")
	}
	if err := c.FactoryReset(ctx, !*all); err != nil {
		return err
	}
	fmt.Println("Werkseinstellungen werden geladen, das Gerät startet neu (ca. 1–2 Minuten).")
	return nil
}

func authFlags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	return fs, fs.String("u", "root", "Benutzername")
}

// reorder erlaubt Flags auch nach der IP („axisctl net 1.2.3.4 -dhcp“).
func reorder(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") &&
				!isBoolFlag(a) {
				flags = append(flags, args[i+1])
				i++
			}
		} else {
			pos = append(pos, a)
		}
	}
	return append(flags, pos...)
}

func isBoolFlag(a string) bool {
	a = strings.TrimLeft(a, "-")
	switch a {
	case "dhcp", "sync", "init", "y", "dry-run", "all":
		return true
	}
	return false
}

func oneArg(fs *flag.FlagSet) (string, error) {
	if fs.NArg() != 1 {
		return "", errors.New("genau eine IP-Adresse angeben")
	}
	return fs.Arg(0), nil
}

func login(ctx context.Context, ip, user string) (*axis.Client, error) {
	pw := os.Getenv("AXIS_PASSWORD")
	if pw == "" {
		var err error
		if pw, err = readPassword(fmt.Sprintf("Passwort für %s@%s: ", user, ip)); err != nil {
			return nil, err
		}
	}
	c := axis.NewClient(ip, 80, user, pw)
	return c, c.CheckLogin(ctx)
}

func localTZ() string {
	if l, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(l, "zoneinfo/"); i >= 0 {
			return l[i+9:]
		}
	}
	return ""
}

// readPassword liest ohne Echo vom Terminal (ohne externe Abhängigkeit).
func readPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	fd := os.Stdin.Fd()
	var old syscall.Termios
	isTTY := ioctl(fd, syscall.TCGETS, &old) == nil
	if isTTY {
		t := old
		t.Lflag &^= syscall.ECHO
		ioctl(fd, syscall.TCSETS, &t)
		defer func() { ioctl(fd, syscall.TCSETS, &old); fmt.Fprintln(os.Stderr) }()
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func ioctl(fd uintptr, req uintptr, t *syscall.Termios) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(unsafe.Pointer(t))); e != 0 {
		return e
	}
	return nil
}

func describeNTP(t axis.NTPConfig) string {
	if !t.Enabled {
		return "aus"
	}
	s := strings.Join(t.Servers, ", ")
	if t.FromDHCP {
		s = "per DHCP"
	}
	if len(t.Active) > 0 {
		s += " (aktiv: " + strings.Join(t.Active, ", ") + ")"
	}
	if t.Synced != nil && *t.Synced {
		s += ", synchronisiert"
	}
	return s
}
