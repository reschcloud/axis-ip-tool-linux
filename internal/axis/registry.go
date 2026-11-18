// SPDX-License-Identifier: AGPL-3.0-or-later

package axis

import (
	"sort"
	"time"
)

// Registry combines the partial discovery results (mDNS, SSDP, enrichment) into
// devices. Not thread-safe – callers must serialize access.
type Registry struct {
	devs map[string]*Device
	byEP map[string]string // IP:port -> key
}

// Change describes what an update changed in the device list.
type Change struct {
	Key     string // key of the updated device ("" = ignored)
	Added   bool   // Key is new in the list
	Removed string // this key was dropped (merged into Key)
}

func NewRegistry() *Registry {
	return &Registry{devs: map[string]*Device{}, byEP: map[string]string{}}
}

// Update applies d and returns the change.
func (r *Registry) Update(d Device) Change {
	key, ep := d.Key(), d.Endpoint()
	if key == "" {
		return Change{}
	}
	var ch Change
	if old, ok := r.byEP[ep]; ok && ep != "" && old != key {
		od := r.devs[old]
		switch {
		case d.Serial == "":
			key = old // message without serial belongs to the known device at this address
		case od != nil && od.Serial == "":
			// device was known only by address (e.g. SSDP) – now we know the serial number
			delete(r.devs, old)
			ch.Removed = old
			if r.devs[key] == nil {
				r.devs[key] = od
				ch.Added = true
			}
		}
	}
	dev := r.devs[key]
	if dev == nil {
		if !d.Online {
			return Change{}
		}
		dev = &Device{}
		r.devs[key] = dev
		ch.Added = true
	}
	dev.Merge(d)
	if e := dev.Endpoint(); e != "" {
		r.byEP[e] = key
	}
	ch.Key = key
	return ch
}

// Get returns the device for key or nil.
func (r *Registry) Get(key string) *Device { return r.devs[key] }

// Len returns the number of devices.
func (r *Registry) Len() int { return len(r.devs) }

// All returns all devices, sorted by IP address.
func (r *Registry) All() []*Device {
	out := make([]*Device, 0, len(r.devs))
	for _, d := range r.devs {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return IPLess(out[i].IP, out[j].IP) })
	return out
}

// Expire marks devices without a sign of life for maxAge as offline and returns their keys.
func (r *Registry) Expire(now time.Time, maxAge time.Duration) []string {
	var keys []string
	for k, d := range r.devs {
		limit := maxAge
		if !d.Pingable {
			limit = max(maxAge, maxAgeNoPing)
		}
		if d.Online && !d.LastSeen.IsZero() && now.Sub(d.LastSeen) > limit {
			d.Online = false
			keys = append(keys, k)
		}
	}
	return keys
}
