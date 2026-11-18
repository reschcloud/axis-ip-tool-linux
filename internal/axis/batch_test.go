// SPDX-License-Identifier: AGPL-3.0-or-later

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
	other := dev("B8A44F000009", "10.0.0.11") // foreign device in the middle of the range
	s := BatchSettings{SetNetwork: true, StartIP: "10.0.0.10", Prefix: 24, Gateway: "10.0.0.12",
		HostnamePattern: "Kamera_##-@", FirstNumber: 1}
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

	// range too small: /30 has only two usable addresses
	s.StartIP, s.Prefix, s.Gateway = "10.0.0.1", 30, ""
	if _, err := PlanBatch(devs, s, devs); err == nil || !strings.Contains(err.Error(), "only enough for 2 of 3") {
		t.Fatalf("overflow: %v", err)
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
		t.Errorf("conflict with foreign device not detected: %v", as[0].Problems)
	}
	p := strings.Join(as[1].Problems, "|")
	for _, w := range []string{"outside", "invalid hostname", "factory state"} {
		if !strings.Contains(p, w) {
			t.Errorf("%q missing in %v", w, as[1].Problems)
		}
	}
	// keeping the own current IP is no conflict
	as = []Assignment{{Device: a1, IP: "10.0.0.50"}}
	CheckAssignments(as, s, []Device{a1})
	if !as[0].Ok() {
		t.Errorf("own IP: %v", as[0].Problems)
	}
}

func TestParsePlanCSV(t *testing.T) {
	for name, in := range map[string]string{
		"Excel with BOM and German header": "\ufeffHostname;Seriennummer;IP-Adresse\r\nCam-Hof;ACCC8E000001;10.0.0.20/24\r\n;ac:cc:8e:00:00:02;10.0.0.21\r\n",
		"no header, comma":                 "ACCC8E000001,10.0.0.20,cam-hof\n# comment\nACCC8E000002,10.0.0.21,\n",
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
		t.Error("CSV without serial number accepted")
	}
	as := []Assignment{{Device: dev("ACCC8E000001", "")}}
	missing := ApplyPlan(as, []PlanRow{{"ACCC8E000001", "10.0.0.9", "a"}, {"ACCC8E0000FF", "10.0.0.8", ""}})
	if as[0].IP != "10.0.0.9" || as[0].Hostname != "a" || len(missing) != 1 {
		t.Errorf("ApplyPlan: %+v, fehlend %v", as, missing)
	}
}

func TestRunBatch(t *testing.T) {
	ctx := context.Background()
	f1, h1, p1 := startFake(t) // factory state
	f2, h2, p2 := startFake(t)
	f2.users["root"] = "other" // wrong password → error, the rest continues
	no := false
	d1 := Device{Serial: "ACCC8E000001", IP: h1, Port: p1, Online: true}
	d2 := Device{Serial: "ACCC8E000002", IP: h2, Port: p2, Online: true, NeedsSetup: &no}

	s := BatchSettings{User: "root", Password: "geheim123", InitPassword: true,
		SetNetwork: true, StartIP: "10.0.0.20", Prefix: 24, Gateway: "10.0.0.1", DNS: []string{"10.0.0.1"},
		HostnamePattern: "cam-#", FirstNumber: 1, SetNTP: true, NTP: NTPConfig{Enabled: true, Servers: []string{"pool.ntp.org"}},
		Setup: InitialSetup{PowerFrequency: 50, Mount: MountCeiling}}
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
		t.Fatalf("device 1: %v (steps %v)", res[0].Err, steps)
	}
	if res[1].Err == nil || !strings.Contains(res[1].Err.Error(), "login") {
		t.Fatalf("device 2 should have failed at login: %v", res[1].Err)
	}
	if got := strings.Join(steps, " "); got != "01:Initial password 01:Login 01:Power frequency/mounting 01:NTP 01:Network 02:Login" {
		t.Errorf("steps: %s", got)
	}
	if f1.params["ImageSource.I0.CaptureFrequency"] != "50Hz" || f2.params["ImageSource.I0.CaptureFrequency"] != "60Hz" {
		t.Errorf("power line frequency: fake 1 %v, fake 2 %v (only when setting the initial password)", f1.params, f2.params)
	}
	if f1.users["root"] != "geheim123" || f1.ntp["serversSource"] != "static" {
		t.Errorf("Fake 1: users=%v ntp=%v", f1.users, f1.ntp)
	}
	last := f1.jsonBody[len(f1.jsonBody)-1]
	addr := last["params"].(map[string]any)["staticAddressConfigurations"].([]any)[0].(map[string]any)
	if last["method"] != "setIPv4AddressConfiguration" || addr["address"] != "10.0.0.20" {
		t.Errorf("IP not set: %v", last)
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

// Logged-in devices use their own credentials, without a shared password.
func TestRunBatchOwnCredentials(t *testing.T) {
	ctx := context.Background()
	f, host, port := startFake(t)
	f.users["admin"] = "eigenes123"
	d := Device{Serial: "ACCC8E112233", IP: host, Port: port, Online: true}
	s := BatchSettings{HostnamePattern: "cam-#", FirstNumber: 1}
	as, err := PlanBatch([]Device{d}, s, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res := RunBatch(ctx, as, s, func(BatchEvent) {}); res[0].Err == nil {
		t.Fatal("successful without credentials?")
	}
	as[0].User, as[0].Password = "admin", "eigenes123"
	if res := RunBatch(ctx, as, s, func(BatchEvent) {}); res[0].Err != nil {
		t.Fatalf("with own credentials: %v", res[0].Err)
	}
}
