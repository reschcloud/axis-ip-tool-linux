// SPDX-License-Identifier: GPL-3.0-or-later

package axis

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func dev(serial, ip string) Device { return Device{Serial: serial, IP: ip, Online: true} }

func TestPlanBatchIPs(t *testing.T) {
	devs := []Device{dev("ACCC8E000003", "169.254.1.3"), dev("ACCC8E000001", "169.254.1.1"), dev("ACCC8E000002", "169.254.1.2")}
	other := dev("B8A44F000009", "10.0.0.11") // fremdes Gerät mitten im Bereich
	s := BatchSettings{SetNetwork: true, StartIP: "10.0.0.10", Prefix: 24, Gateway: "10.0.0.12",
		HostnamePattern: "Kamera_{nr:02}-{ip}", FirstNumber: 1}
	as, err := PlanBatch(devs, s, append(devs, other))
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ serial, ip, host string }{
		{"ACCC8E000001", "10.0.0.10", "kamera-01-10"},
		{"ACCC8E000002", "10.0.0.13", "kamera-02-13"}, // .11 belegt, .12 Gateway
		{"ACCC8E000003", "10.0.0.14", "kamera-03-14"},
	}
	for i, w := range want {
		a := as[i]
		if a.Device.Serial != w.serial || a.IP != w.ip || a.Hostname != w.host || !a.Ok() {
			t.Errorf("%d: %+v, Probleme %v", i, a, a.Problems)
		}
	}

	// Bereich zu klein: /30 hat nur zwei nutzbare Adressen
	s.StartIP, s.Prefix, s.Gateway = "10.0.0.1", 30, ""
	if _, err := PlanBatch(devs, s, devs); err == nil || !strings.Contains(err.Error(), "reicht nur für 2 von 3") {
		t.Fatalf("Überlauf: %v", err)
	}
}

func TestCheckAssignments(t *testing.T) {
	yes := true
	a1, a2 := dev("ACCC8E000001", "10.0.0.50"), dev("ACCC8E000002", "10.0.0.51")
	a2.NeedsSetup = &yes
	other := dev("B8A44F000009", "10.0.0.60")
	other.Name = "Hofkamera"
	s := BatchSettings{SetNetwork: true, StartIP: "10.0.0.10", Prefix: 24}
	as := []Assignment{
		{Device: a1, IP: "10.0.0.60", Hostname: "cam"},
		{Device: a2, IP: "10.0.1.5", Hostname: "Cam_1"},
	}
	CheckAssignments(as, s, []Device{a1, a2, other})
	if p := strings.Join(as[0].Problems, "|"); !strings.Contains(p, "Hofkamera") {
		t.Errorf("Konflikt mit fremdem Gerät nicht erkannt: %v", as[0].Problems)
	}
	p := strings.Join(as[1].Problems, "|")
	for _, w := range []string{"außerhalb", "ungültiger Hostname", "Werkszustand"} {
		if !strings.Contains(p, w) {
			t.Errorf("%q fehlt in %v", w, as[1].Problems)
		}
	}
	// Die eigene aktuelle IP behalten ist kein Konflikt
	as = []Assignment{{Device: a1, IP: "10.0.0.50"}}
	CheckAssignments(as, s, []Device{a1})
	if !as[0].Ok() {
		t.Errorf("eigene IP: %v", as[0].Problems)
	}
}

func TestParsePlanCSV(t *testing.T) {
	for name, in := range map[string]string{
		"Excel mit BOM und Kopf": "\ufeffHostname;Seriennummer;IP-Adresse\r\nCam-Hof;ACCC8E000001;10.0.0.20/24\r\n;ac:cc:8e:00:00:02;10.0.0.21\r\n",
		"ohne Kopf, Komma":       "ACCC8E000001,10.0.0.20,cam-hof\n# Kommentar\nACCC8E000002,10.0.0.21,\n",
	} {
		rows, err := ParsePlanCSV(strings.NewReader(in))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(rows) != 2 || rows[0] != (PlanRow{"ACCC8E000001", "10.0.0.20", "cam-hof"}) ||
			rows[1].Serial != "ACCC8E000002" || rows[1].IP != "10.0.0.21" {
			t.Errorf("%s: %+v", name, rows)
		}
	}
	if _, err := ParsePlanCSV(strings.NewReader("IP;Hostname\n10.0.0.1;x\n")); err == nil {
		t.Error("CSV ohne Seriennummer akzeptiert")
	}
	as := []Assignment{{Device: dev("ACCC8E000001", "")}}
	missing := ApplyPlan(as, []PlanRow{{"ACCC8E000001", "10.0.0.9", "a"}, {"ACCC8E0000FF", "10.0.0.8", ""}})
	if as[0].IP != "10.0.0.9" || as[0].Hostname != "a" || len(missing) != 1 {
		t.Errorf("ApplyPlan: %+v, fehlend %v", as, missing)
	}
}

func TestRunBatch(t *testing.T) {
	ctx := context.Background()
	f1, h1, p1 := startFake(t) // Werkszustand
	f2, h2, p2 := startFake(t)
	f2.users["root"] = "anderes" // falsches Passwort → Fehler, Rest läuft weiter
	no := false
	d1 := Device{Serial: "ACCC8E000001", IP: h1, Port: p1, Online: true}
	d2 := Device{Serial: "ACCC8E000002", IP: h2, Port: p2, Online: true, NeedsSetup: &no}

	s := BatchSettings{User: "root", Password: "geheim123", InitPassword: true,
		SetNetwork: true, StartIP: "10.0.0.20", Prefix: 24, Gateway: "10.0.0.1", DNS: []string{"10.0.0.1"},
		HostnamePattern: "cam-{nr}", FirstNumber: 1, SetNTP: true, NTP: NTPConfig{Enabled: true, Servers: []string{"pool.ntp.org"}}}
	as, err := PlanBatch([]Device{d2, d1}, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	res := RunBatch(ctx, as, s, func(e BatchEvent) {
		if !e.Done {
			steps = append(steps, as[e.Index].Device.Serial[10:]+":"+e.Step)
		}
	})
	if res[0].Err != nil {
		t.Fatalf("Gerät 1: %v (Schritte %v)", res[0].Err, steps)
	}
	if res[1].Err == nil || !strings.Contains(res[1].Err.Error(), "Anmeldung") {
		t.Fatalf("Gerät 2 hätte an der Anmeldung scheitern müssen: %v", res[1].Err)
	}
	if got := strings.Join(steps, " "); got != "01:Erstpasswort 01:Anmeldung 01:NTP 01:Netzwerk 02:Anmeldung" {
		t.Errorf("Schritte: %s", got)
	}
	if f1.users["root"] != "geheim123" || f1.ntp["serversSource"] != "static" {
		t.Errorf("Fake 1: users=%v ntp=%v", f1.users, f1.ntp)
	}
	last := f1.jsonBody[len(f1.jsonBody)-1]
	addr := last["params"].(map[string]any)["staticAddressConfigurations"].([]any)[0].(map[string]any)
	if last["method"] != "setIPv4AddressConfiguration" || addr["address"] != "10.0.0.20" {
		t.Errorf("IP nicht gesetzt: %v", last)
	}
	var host map[string]any
	for _, b := range f1.jsonBody {
		if b["method"] == "setHostnameConfiguration" {
			host = b["params"].(map[string]any)
		}
	}
	if host["staticHostname"] != "cam-1" {
		t.Errorf("Hostname: %v", host)
	}

	var buf bytes.Buffer
	WriteResultsCSV(&buf, res)
	if !strings.Contains(buf.String(), "ACCC8E000001;;"+h1+";10.0.0.20;cam-1;OK") {
		t.Errorf("Protokoll:\n%s", buf.String())
	}
}
