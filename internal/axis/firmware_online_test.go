// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"context"
	"os"
	"testing"
)

// Against the real axis.com: AXIS_ONLINE_TEST=1 go test -run Online ./internal/axis
func TestFirmwareCatalogOnline(t *testing.T) {
	if os.Getenv("AXIS_ONLINE_TEST") == "" {
		t.Skip("set AXIS_ONLINE_TEST=1")
	}
	ctx := context.Background()
	cat := NewFirmwareCatalog()
	for _, m := range [][]string{{"P3225-VE Mk II", "AXIS P3225-VE Mk II"}, {"Q2101-TE", "AXIS Q2101-TE Thermal Camera"}, {"", "AXIS Q2101-TE Thermal Camera"}} {
		p, ok, err := cat.FindProduct(ctx, m...)
		if err != nil || !ok {
			t.Fatalf("%v: %v %v", m, ok, err)
		}
		vs, err := cat.Versions(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		cur := map[string]string{"P3225-VE_Mk_II": "9.80.49", "Q2101-TE": "11.11.73"}[p.Folder]
		cv, _ := ParseFWVersion(cur)
		ch := FirmwareChoices(cv, vs)
		t.Logf("%v → %+v, %d versions, choices %v", m, p, len(vs), ch)
		f, err := cat.File(ctx, p, ch[0].Version)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("file %+v", f)
	}
	if err := cat.CheckMyAxisSession(ctx, "invalid"); err != ErrMyAxisLogin {
		t.Fatalf("session check: %v", err)
	}
}
