module ip-tool-for-axis

go 1.24.0

require (
	github.com/diamondburned/gotk4-adwaita/pkg v0.0.0-20250703085740-f81761ef0e0d
	github.com/diamondburned/gotk4/pkg v0.3.2-0.20250703063411-16654385f59a
	github.com/godbus/dbus/v5 v5.2.4
)

require (
	github.com/KarpelesLab/weak v0.1.1 // indirect
	go4.org/unsafe/assume-no-moving-gc v0.0.0-20231121144256-b99613f794b6 // indirect
	golang.org/x/sync v0.0.0-20210220032951-036812b2e83c // indirect
	golang.org/x/sys v0.27.0 // indirect
)

// Fetch golang.org/x/* via the official GitHub mirrors
// (also works without access to proxy.golang.org).
replace (
	go4.org/unsafe/assume-no-moving-gc => github.com/go4org/unsafe-assume-no-moving-gc v0.0.0-20231121144256-b99613f794b6
	golang.org/x/sync => github.com/golang/sync v0.0.0-20210220032951-036812b2e83c
	golang.org/x/sys => github.com/golang/sys v0.27.0
)
