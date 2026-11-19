// SPDX-License-Identifier: AGPL-3.0-or-later

// axisctl – command line tool for finding and configuring Axis devices.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"
	"unsafe"

	"axis-ip-tool/internal/axis"
)

// version is set at build time (-ldflags "-X main.version=…", see Makefile).
var version = "dev"

const usage = `axisctl – find and configure Axis devices

Usage: axisctl [--https-only] COMMAND …

Commands:
  list     [-t 5s]                                  Discover devices on the network
  info     <ip>                                     Show model, firmware, network
  init     <ip> [-u root]                           Create the first admin (factory state),
                [-freq 50] [-mount ceiling]         set power line frequency/mounting position
  net      <ip> (-dhcp | -ip A/PREFIX [-gw G])
                [-hostname H] [-dns A,B]            Configure network
  time     <ip> [-tz Europe/Vienna] [-sync=true]
                [-ntp dhcp|off|SERVER,SERVER]       Set time zone/time/NTP
  restart  <ip>                                     Reboot
  reset    <ip> [-all] [-y]                         Factory defaults (-all: IP settings too)
  batch    [DEVICE …] [-csv PLAN] …                 Configure several devices (axisctl batch -h)
  firmware <ip> [-version V | -file F.bin]          Compare with axis.com / update firmware
                [-factory none|soft|hard] [-y]      (axisctl firmware -h)
  ptz      <ip> [-zoom in|out] [-af] [-calibrate]   Show PTZ/lens capabilities, test zoom/AF/calibration
  version                                           Show version

Login: -u USER (default root), password from $AXIS_PASSWORD or prompt.
Connection: HTTPS if the device offers it (certificate is not verified), otherwise HTTP.
  --https-only or AXIS_HTTPS_ONLY=1 forbids the fallback to HTTP.
`

func main() {
	axis.UserAgent = "axisctl/" + version + " (axis-ip-tool)"
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
	case "ptz":
		err = cmdPTZ(ctx, args)
	case "firmware", "fw":
		err = cmdFirmware(ctx, args)
	case "version", "--version":
		fmt.Println("axisctl", version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------- list

func cmdList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	t := fs.String("t", "5s", "discovery duration (e.g. 5 or 10s)")
	fs.Parse(args)
	list, err := discover(ctx, *t)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL\tSERIAL\tHOSTNAME\tIPV4\tIPV6\tINTERFACE\tFIRMWARE\tPING\tCONNECTION\tSTATUS")
	for _, d := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", d.Model, d.Serial, d.Hostname, d.IP,
			d.PreferredIPv6(), d.Iface, d.Firmware, d.PingText(), d.Connection(), d.Status())
	}
	tw.Flush()
	if len(list) == 0 {
		fmt.Fprintln(os.Stderr, "No devices found.")
	}
	return nil
}

// discover searches for devices for the duration t (e.g. "5" or "10s").
func discover(ctx context.Context, t string) ([]*axis.Device, error) {
	var dur time.Duration
	if n, err := strconv.Atoi(t); err == nil {
		dur = time.Duration(n) * time.Second
	} else if dur, err = time.ParseDuration(t); err != nil {
		return nil, fmt.Errorf("invalid discovery duration %q", t)
	}
	sctx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()
	var mu sync.Mutex
	reg := axis.NewRegistry()
	axis.Discover(sctx, func(d axis.Device) {
		mu.Lock()
		reg.Update(d)
		mu.Unlock()
	}, func(err error) { fmt.Fprintln(os.Stderr, "Note:", err) })
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
	if errors.Is(err, axis.ErrAuth) { // newer firmware: only with login
		if c, err = login(ctx, ip, *user); err != nil {
			return err
		}
		info, err = c.BasicInfo(ctx)
	}
	if err != nil {
		return err
	}
	fmt.Printf("Model:         %s\nSerial:        %s\nFirmware:      %s\n", info.Model, info.Serial, info.Firmware)
	conn := "HTTP (unencrypted)"
	if axis.NewClient(ip, 80, "", "").Secure(ctx) {
		conn = "HTTPS"
	}
	fmt.Printf("Connection:    %s\n", conn)
	if needs, _ := axis.NewClient(ip, 80, "", "").NeedsInitialUser(ctx); needs {
		fmt.Println("Status:        factory state, no password set yet (axisctl init)")
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
	mode := "static"
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
	user := fs.String("u", "root", "user name of the administrator")
	freq := fs.Int("freq", 50, "power line frequency in Hz (50, 60; 0 = don't change)")
	mount := fs.String("mount", "ceiling", "mounting position: ceiling, wall, desk, none")
	fs.Parse(reorder(args))
	ip, err := oneArg(fs)
	if err != nil {
		return err
	}
	setup, err := setupFlags(*freq, *mount)
	if err != nil {
		return err
	}
	pw := os.Getenv("AXIS_PASSWORD")
	if pw == "" {
		if pw, err = readPassword("New password: "); err != nil {
			return err
		}
		pw2, err := readPassword("Repeat: ")
		if err != nil {
			return err
		}
		if pw != pw2 {
			return errors.New("passwords don't match")
		}
	}
	if len(pw) < 8 {
		return errors.New("password too short (at least 8 characters)")
	}
	c := axis.NewClient(ip, 80, "", "")
	if !c.Secure(ctx) && !axis.HTTPSOnly() {
		fmt.Fprintln(os.Stderr, "Warning: device doesn't offer HTTPS – the password is transmitted unencrypted.")
	}
	if err := c.CreateInitialUser(ctx, *user, pw); err != nil {
		return err
	}
	fmt.Printf("User %q created.\n", *user)
	return applySetup(ctx, axis.NewClient(ip, 80, *user, pw), setup)
}

// setupFlags checks -freq/-mount for commissioning.
func setupFlags(freq int, mount string) (axis.InitialSetup, error) {
	m, err := axis.ParseMount(mount)
	if err != nil {
		return axis.InitialSetup{}, err
	}
	if freq != 0 && freq != 50 && freq != 60 {
		return axis.InitialSetup{}, errors.New("-freq: 50, 60 or 0 (don't change)")
	}
	return axis.InitialSetup{PowerFrequency: freq, Mount: m}, nil
}

// applySetup sets power line frequency and mounting position and reports what the device doesn't know.
func applySetup(ctx context.Context, c *axis.Client, s axis.InitialSetup) error {
	if s.Empty() {
		return nil
	}
	skipped, err := c.ApplyInitialSetup(ctx, s)
	if err != nil {
		return fmt.Errorf("power line frequency/mounting position: %w", err)
	}
	if s.PowerFrequency != 0 && !slices.Contains(skipped, axis.SettingPowerFrequency) {
		fmt.Printf("Power line frequency: %d Hz\n", s.PowerFrequency)
	}
	if s.Mount != "" && !slices.Contains(skipped, axis.SettingMount) {
		fmt.Printf("Mounting position: %s\n", axis.MountName(s.Mount))
	}
	for _, k := range skipped {
		fmt.Printf("%s: not supported by the device\n", k)
	}
	return nil
}

// ---------------------------------------------------------------- net

func cmdNet(ctx context.Context, args []string) error {
	fs, user := authFlags("net")
	dhcp := fs.Bool("dhcp", false, "IPv4 via DHCP")
	addr := fs.String("ip", "", "static address, e.g. 192.168.1.20/24")
	gw := fs.String("gw", "", "default gateway")
	host := fs.String("hostname", "", "static hostname (\"dhcp\" = via DHCP)")
	dns := fs.String("dns", "", "DNS servers, comma-separated (\"dhcp\" = via DHCP)")
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
	fmt.Println("Network settings applied.")
	return nil
}

// ---------------------------------------------------------------- time / restart

func cmdTime(ctx context.Context, args []string) error {
	fs, user := authFlags("time")
	tz := fs.String("tz", localTZ(), "time zone (IANA)")
	sync := fs.Bool("sync", true, "take the time from this computer (ignored if NTP is active)")
	ntp := fs.String("ntp", "", "NTP: \"dhcp\", \"off\" or servers (comma-separated)")
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
		case "off":
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
		*sync = false // time comes via NTP
	}
	if err := c.SetTime(ctx, *tz, *sync); err != nil {
		return err
	}
	if t, err := c.GetNTP(ctx); err == nil {
		fmt.Println("NTP:", describeNTP(t))
	}
	fmt.Println("Date & time set.")
	return nil
}

func cmdFirmware(ctx context.Context, args []string) error {
	fs, user := authFlags("firmware")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: axisctl firmware <ip> [options]

Without -version/-file: shows the installed firmware and the versions on axis.com.
Downloading from axis.com needs a My Axis sign-in: sign in once in the GUI
(Firmware update → Sign in) or set $AXIS_MYAXIS_SESSION to the value of the
axis_connect_session_sid cookie of www.axis.com.

`)
		fs.PrintDefaults()
	}
	version := fs.String("version", "", "update to this version from axis.com (\"latest\", \"lts\", \"track\" or e.g. 9.80.132)")
	file := fs.String("file", "", "update with a local firmware file (.bin)")
	factory := fs.String("factory", "none", "factory defaults after the update: none, soft (keep network), hard")
	yes := fs.Bool("y", false, "don't ask")
	dlOnly := fs.Bool("download-only", false, "only download into the cache, don't update")
	fs.Parse(reorder(args))
	ip, err := oneArg(fs)
	if err != nil {
		return err
	}
	switch *factory {
	case axis.FactoryDefaultNone, axis.FactoryDefaultSoft, axis.FactoryDefaultHard:
	default:
		return errors.New("-factory: none, soft or hard")
	}
	if *version != "" && *file != "" {
		return errors.New("-version and -file exclude each other")
	}

	c, err := login(ctx, ip, *user)
	if err != nil {
		return err
	}
	info, err := c.BasicInfo(ctx)
	if err != nil {
		return err
	}
	prod, _ := c.ProductNumber(ctx)
	fmt.Printf("Device:    %s (%s)\nInstalled: %s\n", info.Model, ip, info.Firmware)

	path := *file
	if path == "" {
		cat := axis.NewFirmwareCatalog()
		p, ok, err := cat.FindProduct(ctx, prod, info.Model)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("no firmware for %q on axis.com – use -file", info.Model)
		}
		versions, err := cat.Versions(ctx, p)
		if err != nil {
			return err
		}
		cur, _ := axis.ParseFWVersion(info.Firmware)
		choices := axis.FirmwareChoices(cur, versions)
		fmt.Printf("axis.com:  %s/%s\n", p.Source, p.Folder)
		if len(choices) == 0 {
			fmt.Println("Up to date – no newer firmware on axis.com.")
		}
		for _, ch := range choices {
			fmt.Printf("  %-12s %-30s %s\n", ch.Version, ch.Label, ch.Version.TrackName())
		}
		if *version == "" {
			return nil
		}
		var target axis.FWVersion
		pick := func(label string) {
			for _, ch := range choices {
				if strings.Contains(ch.Label, label) {
					target = ch.Version
				}
			}
		}
		switch strings.ToLower(*version) {
		case "latest":
			pick("Latest")
			if target == nil && len(versions) > 0 {
				target = versions[0]
			}
		case "lts":
			pick("Latest LTS")
		case "track":
			pick("Current track")
		default:
			v, ok := axis.ParseFWVersion(*version)
			if !ok {
				return fmt.Errorf("invalid version %q", *version)
			}
			for _, x := range versions {
				if x.Compare(v) == 0 {
					target = x
				}
			}
			if target == nil {
				return fmt.Errorf("version %s not found on axis.com", v)
			}
		}
		if target == nil {
			return errors.New("no matching newer version")
		}
		f, err := cat.File(ctx, p, target)
		if err != nil {
			return err
		}
		sess, _ := axis.LoadMyAxisSession()
		fmt.Printf("Downloading %s (%.1f MB)…\n", f.Name, float64(f.Size)/(1<<20))
		path, err = cat.Download(ctx, f, sess.Value, cliProgress("Download"))
		fmt.Println()
		if err != nil {
			return err
		}
		fmt.Println("Checksum OK:", path)
		if *dlOnly {
			return nil
		}
	}

	fmt.Printf("Update %s from %s with %s", info.Model, info.Firmware, filepath.Base(path))
	if *factory != axis.FactoryDefaultNone {
		fmt.Printf(" and reset to factory defaults (%s)", *factory)
	}
	fmt.Println(".")
	if !*yes && !confirm("The device restarts and is unavailable for several minutes. Continue? [y/N] ") {
		return errors.New("cancelled")
	}
	newVer, err := c.UpgradeFirmware(ctx, path, *factory, cliProgress("Upload"))
	fmt.Println()
	if err != nil {
		return err
	}
	fmt.Printf("Upload complete, installing %s…\n", newVer)
	if *factory == axis.FactoryDefaultHard {
		fmt.Println("Hard factory reset: the device loses its IP settings and password – not waiting for it.")
		return nil
	}
	if *factory == axis.FactoryDefaultSoft {
		fmt.Println("Factory reset: the device has no password afterwards (axisctl init) – not waiting for it.")
		return nil
	}
	err = c.WaitForUpgrade(ctx, info.Firmware, newVer, func(s axis.UpgradeStage) {
		switch s {
		case axis.StageRestarting:
			fmt.Println("Device is restarting…")
		case axis.StageDone:
			fmt.Println("Device is back online.")
		}
	})
	if err != nil {
		return err
	}
	fmt.Println("Firmware updated to", newVer)
	return nil
}

// cliProgress prints a percentage on one line.
func cliProgress(what string) func(done, total int64) {
	last := -1
	return func(done, total int64) {
		if total <= 0 {
			return
		}
		if p := int(done * 100 / total); p != last {
			last = p
			fmt.Printf("\r%s: %3d %%", what, min(p, 100))
		}
	}
}

func cmdPTZ(ctx context.Context, args []string) error {
	fs, user := authFlags("ptz")
	zoom := fs.String("zoom", "", "zoom one step: in or out")
	step := fs.Int("step", 500, "step size for -zoom with ptz.cgi (1…9999)")
	af := fs.Bool("af", false, "run the autofocus (motorized lens)")
	calib := fs.Bool("calibrate", false, "calibrate the lens (motorized lens; takes up to a minute)")
	fs.Parse(reorder(args))
	ip, err := oneArg(fs)
	if err != nil {
		return err
	}
	c, err := login(ctx, ip, *user)
	if err != nil {
		return err
	}
	fmt.Print(c.PTZDiagnostics(ctx))
	l, err := c.GetLensControls(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("== detected\nPTZ:    %+v\n", l.PTZ)
	if l.Optics != nil {
		fmt.Printf("Optics: %+v\n", *l.Optics)
	}
	if *zoom == "" && !*af && !*calib {
		return nil
	}
	if (*af || *calib) && l.Optics == nil {
		return errors.New("no motorized lens found")
	}
	wait := func(what string) error {
		time.Sleep(time.Second)
		for i := 0; i < 120; i++ {
			st, err := c.GetOptics(ctx, *l.Optics)
			if err != nil {
				return err
			}
			if !st.Moving() {
				fmt.Printf("== after %s: %+v\n", what, st)
				return nil
			}
			time.Sleep(time.Second)
		}
		return errors.New(what + ": lens still moving after 2 minutes")
	}
	if *calib {
		fmt.Println("Calibrating …")
		if err := c.CalibrateOptics(ctx, *l.Optics); err != nil {
			return err
		}
		if err := wait("calibration"); err != nil {
			return err
		}
	}
	if *af {
		fmt.Println("Autofocus …")
		if err := c.Autofocus(ctx, *l.Optics); err != nil {
			return err
		}
		if err := wait("autofocus"); err != nil {
			return err
		}
	}
	if *zoom == "" {
		return nil
	}
	in := *zoom == "in"
	if !in && *zoom != "out" {
		return errors.New("-zoom: in or out")
	}
	if l.Optics != nil && l.Optics.Zoom {
		err = c.StepZoom(ctx, *l.Optics, axis.StepBig, in)
	} else {
		err = c.PTZStepZoom(ctx, l.PTZ, *step, in)
	}
	if err != nil {
		return err
	}
	if l.Optics != nil {
		return wait("zoom " + *zoom)
	}
	time.Sleep(2 * time.Second)
	pos, err := c.PTZGetPosition(ctx)
	fmt.Printf("== after zoom %s: %+v %v\n", *zoom, pos, err)
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
	fmt.Println("Reboot triggered.")
	return nil
}

func cmdReset(ctx context.Context, args []string) error {
	fs, user := authFlags("reset")
	all := fs.Bool("all", false, "reset network settings too (device gets a new IP)")
	yes := fs.Bool("y", false, "don't ask for confirmation")
	fs.Parse(reorder(args))
	ip, err := oneArg(fs)
	if err != nil {
		return err
	}
	c, err := login(ctx, ip, *user)
	if err != nil {
		return err
	}
	what := "All settings, users and passwords (IP settings are kept)"
	if *all {
		what = "All settings, users, passwords AND the IP settings"
	}
	if !*yes && !confirm(fmt.Sprintf("%s on %s will be deleted. Continue? [y/N] ", what, ip)) {
		return errors.New("cancelled")
	}
	if err := c.FactoryReset(ctx, !*all); err != nil {
		return err
	}
	fmt.Println("Loading factory defaults, the device reboots (about 1–2 minutes).")
	return nil
}

func authFlags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	return fs, fs.String("u", "root", "user name")
}

// reorder allows flags after the IP too ("axisctl net 1.2.3.4 -dhcp").
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
	case "dhcp", "sync", "init", "y", "dry-run", "all", "download-only", "af", "calibrate":
		return true
	}
	return false
}

func oneArg(fs *flag.FlagSet) (string, error) {
	if fs.NArg() != 1 {
		return "", errors.New("specify exactly one IP address")
	}
	return fs.Arg(0), nil
}

func login(ctx context.Context, ip, user string) (*axis.Client, error) {
	pw := os.Getenv("AXIS_PASSWORD")
	if pw == "" {
		var err error
		if pw, err = readPassword(fmt.Sprintf("Password for %s@%s: ", user, ip)); err != nil {
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

// readPassword reads from the terminal without echo (no external dependency).
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
		return "off"
	}
	s := strings.Join(t.Servers, ", ")
	if t.FromDHCP {
		s = "via DHCP"
	}
	if len(t.Active) > 0 {
		s += " (active: " + strings.Join(t.Active, ", ") + ")"
	}
	if t.Synced != nil && *t.Synced {
		s += ", synchronized"
	}
	return s
}
