// SPDX-License-Identifier: GPL-3.0-or-later

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

	"axis-ip-tool/internal/axis"
)

const batchUsage = `axisctl batch – mehrere Geräte nacheinander konfigurieren

Aufruf: axisctl batch [Optionen] [GERÄT …]
  GERÄT = Seriennummer oder aktuelle IP. Ohne Angabe: alle Geräte aus -csv.

Zugang:
  -u root                 Benutzer (Passwort aus $AXIS_PASSWORD oder Abfrage)
  -init                   auf Geräten im Werkszustand den ersten Administrator anlegen
Netzwerk (weglassen = unverändert):
  -dhcp                   IPv4 per DHCP
  -ip START/PRÄFIX        fortlaufende statische Adressen, z. B. 10.0.0.20/24
  -gw G  -dns A,B         Gateway, DNS-Server
  -order serial|ip|name   Reihenfolge der Adressvergabe (Standard serial)
  -csv PLAN               Zuordnung aus CSV: Seriennummer;IP;Hostname (überschreibt -ip/-hostname)
Hostname, Zeit:
  -hostname MUSTER        z. B. kamera-{nr:02}, cam-{serial}, cam-{ip}
  -nr 1                   Startwert für {nr}
  -tz Europe/Vienna       Zeitzone
  -ntp dhcp|off|S1,S2     NTP
Ablauf:
  -t 5s                   Suchdauer
  -dry-run                nur den Plan anzeigen
  -y                      ohne Rückfrage ausführen
  -log DATEI.csv          Protokoll schreiben
`

func cmdBatch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("batch", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, batchUsage) }
	user := fs.String("u", "root", "")
	initPw := fs.Bool("init", false, "")
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
	switch {
	case *dhcp && *ipRange != "":
		return errors.New("-dhcp und -ip schließen sich aus")
	case *dhcp:
		s.SetNetwork, s.DHCP = true, true
	case *ipRange != "":
		a, p, ok := strings.Cut(*ipRange, "/")
		if !ok {
			return errors.New("-ip braucht ein Präfix, z. B. 10.0.0.20/24")
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
		case "off", "aus":
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
			return errors.New("der CSV-Plan enthält IP-Adressen – bitte -ip START/PRÄFIX angeben " +
				"(Präfix und Bereichsprüfung), z. B. -ip 10.0.0.1/24")
		}
	}
	if !s.SetNetwork && !s.SetNTP && s.TimeZone == "" && s.HostnamePattern == "" && plan == nil && !s.InitPassword {
		return errors.New("nichts zu tun – siehe axisctl batch -h")
	}

	fmt.Fprintln(os.Stderr, "Suche Geräte …")
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

	bad := 0
	for _, a := range as {
		if !a.Ok() {
			bad++
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d Gerät(e) mit Problemen – bitte Plan korrigieren", bad)
	}
	if *dry {
		return nil
	}
	if !*yes && !confirm(fmt.Sprintf("%d Gerät(e) jetzt konfigurieren? [j/N] ", len(as))) {
		return errors.New("abgebrochen")
	}

	if s.Password = os.Getenv("AXIS_PASSWORD"); s.Password == "" {
		if s.Password, err = readPassword(fmt.Sprintf("Passwort für %s: ", s.User)); err != nil {
			return err
		}
		if s.InitPassword {
			p2, err := readPassword("Wiederholen: ")
			if err != nil {
				return err
			}
			if p2 != s.Password {
				return errors.New("Passwörter stimmen nicht überein")
			}
		}
	}
	if s.InitPassword && len(s.Password) < 8 {
		return errors.New("Passwort zu kurz (mindestens 8 Zeichen)")
	}

	res := axis.RunBatch(ctx, as, s, func(e axis.BatchEvent) {
		label := fmt.Sprintf("[%d/%d] %s", e.Index+1, len(as), as[e.Index].Device.Serial)
		switch {
		case !e.Done:
			fmt.Printf("%s: %s …\n", label, e.Step)
		case e.Err != nil:
			fmt.Printf("%s: FEHLER – %v\n", label, e.Err)
		default:
			fmt.Printf("%s: fertig\n", label)
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
		fmt.Println("Protokoll:", *logPath)
	}
	fmt.Printf("%d von %d Gerät(en) erfolgreich konfiguriert.\n", len(res)-failed, len(res))
	if failed > 0 {
		return fmt.Errorf("%d Gerät(e) fehlgeschlagen", failed)
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

// pickDevices wählt die Geräte anhand der Argumente (Seriennummer oder IP) bzw. des Plans.
func pickDevices(all []*axis.Device, args []string, plan []axis.PlanRow) ([]axis.Device, error) {
	if len(args) == 0 {
		for _, r := range plan {
			args = append(args, r.Serial)
		}
	}
	if len(args) == 0 {
		return nil, errors.New("keine Geräte angegeben (Seriennummern/IPs oder -csv)")
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
		return nil, fmt.Errorf("nicht gefunden: %s (Suchdauer mit -t verlängern?)", strings.Join(missing, ", "))
	}
	return out, nil
}

// markUsedAddresses prüft neue IPs, ob dort schon ein Gerät antwortet.
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
				a.Problems = append(a.Problems, "Adresse antwortet bereits (belegt?)")
			}
		}()
	}
	wg.Wait()
}

func printPlan(as []axis.Assignment) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tSERIENNUMMER\tMODELL\tIP ALT\tIP NEU\tHOSTNAME\tHINWEIS")
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
	case "j", "ja", "y", "yes":
		return true
	}
	return false
}
