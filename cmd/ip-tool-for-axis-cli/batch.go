// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"text/tabwriter"

	"ip-tool-for-axis/internal/axis"
)

const batchUsage = `ip-tool-for-axis-cli batch – configure several devices one after another

Usage: ip-tool-for-axis-cli batch [options] [DEVICE …]
  DEVICE = serial number or current IP. None given: all devices from -csv.

Access:
  -u root                 user (password from $AXIS_PASSWORD or prompt)
  -init                   create the first administrator on devices in factory state
  -freq 50                also set the power line frequency (50, 60; 0 = don't change)
  -mount ceiling          also set the mounting position (ceiling, wall, desk, none)
Network (omit = unchanged):
  -dhcp                   IPv4 via DHCP
  -ip START/PREFIX        sequential static addresses, e.g. 10.0.0.20/24
  -gw G  -dns A,B         gateway, DNS servers
  -order serial|ip|name   order of address assignment (default serial)
  -csv PLAN               assignment from CSV: serial;IP;hostname (overrides -ip/-hostname)
Hostname, time:
  -hostname PATTERN       # = sequential number (## → 01, 02 …), @ = last octet of the IP,
                          e.g. camera-## or cam-@
  -nr 1                   start value for #
  -tz Europe/Vienna       time zone
  -ntp dhcp|off|S1,S2     NTP
Run:
  -t 5s                   discovery duration
  -dry-run                only show the plan
  -y                      run without confirmation
  -log FILE.csv           write a log
`

func cmdBatch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("batch", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, batchUsage) }
	user := fs.String("u", "root", "")
	initPw := fs.Bool("init", false, "")
	freq := fs.Int("freq", 50, "")
	mount := fs.String("mount", "ceiling", "")
	dhcp := fs.Bool("dhcp", false, "")
	ipRange := fs.String("ip", "", "")
	gw := fs.String("gw", "", "")
	dns := fs.String("dns", "", "")
	order := fs.String("order", "serial", "")
	csvPath := fs.String("csv", "", "")
	host := fs.String("hostname", "", "")
	nr := fs.Int("nr", 1, "")
	tz := fs.String("tz", "", "")
	ntp := fs.String("ntp", "", "")
	t := fs.String("t", "5s", "")
	dry := fs.Bool("dry-run", false, "")
	yes := fs.Bool("y", false, "")
	logPath := fs.String("log", "", "")
	fs.Parse(reorder(args))

	s := axis.BatchSettings{User: *user, InitPassword: *initPw, Order: *order,
		HostnamePattern: *host, FirstNumber: *nr, TimeZone: *tz}
	if *initPw {
		setup, err := setupFlags(*freq, *mount)
		if err != nil {
			return err
		}
		s.Setup = setup
	}
	switch {
	case *dhcp && *ipRange != "":
		return errors.New("-dhcp and -ip are mutually exclusive")
	case *dhcp:
		s.SetNetwork, s.DHCP = true, true
	case *ipRange != "":
		a, p, ok := strings.Cut(*ipRange, "/")
		if !ok {
			return errors.New("-ip needs a prefix, e.g. 10.0.0.20/24")
		}
		prefix, err := axis.ParsePrefix(p)
		if err != nil {
			return err
		}
		s.SetNetwork, s.StartIP, s.Prefix, s.Gateway = true, a, prefix, *gw
	}
	if *dns != "" {
		s.DNS = strings.Split(*dns, ",")
	}
	if *ntp != "" {
		s.SetNTP = true
		switch strings.ToLower(*ntp) {
		case "off":
			s.NTP = axis.NTPConfig{Enabled: false}
		case "dhcp":
			s.NTP = axis.NTPConfig{Enabled: true, FromDHCP: true}
		default:
			s.NTP = axis.NTPConfig{Enabled: true, Servers: strings.Split(*ntp, ",")}
		}
	}

	var plan []axis.PlanRow
	if *csvPath != "" {
		f, err := os.Open(*csvPath)
		if err != nil {
			return err
		}
		plan, err = axis.ParsePlanCSV(f)
		f.Close()
		if err != nil {
			return err
		}
		if !s.SetNetwork && planHasIP(plan) {
			return errors.New("the CSV plan contains IP addresses – please specify -ip START/PREFIX " +
				"(prefix and range check), e.g. -ip 10.0.0.1/24")
		}
	}
	if !s.SetNetwork && !s.SetNTP && s.TimeZone == "" && s.HostnamePattern == "" && plan == nil && !s.InitPassword {
		return errors.New("nothing to do – see ip-tool-for-axis-cli batch -h")
	}

	fmt.Fprintln(os.Stderr, "Discovering devices …")
	all, err := discover(ctx, *t)
	if err != nil {
		return err
	}
	devs, err := pickDevices(all, fs.Args(), plan)
	if err != nil {
		return err
	}
	allVals := make([]axis.Device, len(all))
	for i, d := range all {
		allVals[i] = *d
	}
	as, err := axis.PlanBatch(devs, s, allVals)
	if err != nil {
		return err
	}
	if plan != nil {
		axis.ApplyPlan(as, plan)
		axis.CheckAssignments(as, s, allVals)
	}
	markUsedAddresses(ctx, as)
	printPlan(as)
	if s.InitPassword && !s.Setup.Empty() {
		fmt.Printf("Devices in factory state additionally: %s\n", s.Setup)
	}

	bad := 0
	for _, a := range as {
		if !a.Ok() {
			bad++
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d device(s) with problems – please correct the plan", bad)
	}
	if *dry {
		return nil
	}
	if !*yes && !confirm(fmt.Sprintf("Configure %d device(s) now? [y/N] ", len(as))) {
		return errors.New("cancelled")
	}

	if s.Password = os.Getenv("AXIS_PASSWORD"); s.Password == "" {
		if s.Password, err = readPassword(fmt.Sprintf("Password for %s: ", s.User)); err != nil {
			return err
		}
		if s.InitPassword {
			p2, err := readPassword("Repeat: ")
			if err != nil {
				return err
			}
			if p2 != s.Password {
				return errors.New("passwords don't match")
			}
		}
	}
	if s.InitPassword && len(s.Password) < 8 {
		return errors.New("password too short (at least 8 characters)")
	}

	res := axis.RunBatch(ctx, as, s, func(e axis.BatchEvent) {
		label := fmt.Sprintf("[%d/%d] %s", e.Index+1, len(as), as[e.Index].Device.Serial)
		switch {
		case !e.Done:
			fmt.Printf("%s: %s …\n", label, e.Step)
		case e.Err != nil:
			fmt.Printf("%s: ERROR – %v\n", label, e.Err)
		default:
			fmt.Printf("%s: done\n", label)
		}
	})
	failed := 0
	for _, r := range res {
		if r.Err != nil {
			failed++
		}
	}
	if *logPath != "" {
		f, err := os.Create(*logPath)
		if err != nil {
			return err
		}
		werr := axis.WriteResultsCSV(f, res)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return werr
		}
		fmt.Println("Log:", *logPath)
	}
	fmt.Printf("%d of %d device(s) configured successfully.\n", len(res)-failed, len(res))
	if failed > 0 {
		return fmt.Errorf("%d device(s) failed", failed)
	}
	return nil
}

func planHasIP(plan []axis.PlanRow) bool {
	for _, r := range plan {
		if r.IP != "" {
			return true
		}
	}
	return false
}

// pickDevices selects the devices by the arguments (serial number or IP) or the plan.
func pickDevices(all []*axis.Device, args []string, plan []axis.PlanRow) ([]axis.Device, error) {
	if len(args) == 0 {
		for _, r := range plan {
			args = append(args, r.Serial)
		}
	}
	if len(args) == 0 {
		return nil, errors.New("no devices given (serial numbers/IPs or -csv)")
	}
	var out []axis.Device
	var missing []string
	seen := map[string]bool{}
	for _, a := range args {
		serial := axis.NormMAC(a)
		var found *axis.Device
		for _, d := range all {
			if (serial != "" && d.Serial == serial) || d.IP == a {
				found = d
				break
			}
		}
		switch {
		case found == nil:
			missing = append(missing, a)
		case !seen[found.Key()]:
			seen[found.Key()] = true
			out = append(out, *found)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("not found: %s (extend the discovery duration with -t?)", strings.Join(missing, ", "))
	}
	return out, nil
}

// markUsedAddresses checks new IPs for whether a device already answers there.
func markUsedAddresses(ctx context.Context, as []axis.Assignment) {
	var wg sync.WaitGroup
	for i := range as {
		a := &as[i]
		if a.IP == "" || a.IP == a.Device.IP || !a.Ok() {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if axis.AddressInUse(ctx, a.IP) {
				a.Problems = append(a.Problems, "address already answers (in use?)")
			}
		}()
	}
	wg.Wait()
}

func printPlan(as []axis.Assignment) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tSERIAL\tMODEL\tOLD IP\tNEW IP\tHOSTNAME\tNOTE")
	dash := func(s string) string {
		if s == "" {
			return "–"
		}
		return s
	}
	for i, a := range as {
		note := "ok"
		if !a.Ok() {
			note = "PROBLEM: " + strings.Join(a.Problems, "; ")
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", i+1, a.Device.Serial, a.Device.Model,
			dash(a.Device.IP), dash(a.IP), dash(a.Hostname), note)
	}
	tw.Flush()
}

func confirm(prompt string) bool {
	fmt.Print(prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}
