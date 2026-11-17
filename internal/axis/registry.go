package axis

import "sort"

// Registry führt die Teilergebnisse der Suche (mDNS, SSDP, Anreicherung) zu
// Geräten zusammen. Nicht threadsicher – vom Aufrufer zu serialisieren.
type Registry struct {
	devs map[string]*Device
	byEP map[string]string // IP:Port -> Schlüssel
}

// Change beschreibt, was ein Update an der Geräteliste geändert hat.
type Change struct {
	Key     string // Schlüssel des aktualisierten Geräts ("" = ignoriert)
	Added   bool   // Key ist neu in der Liste
	Removed string // dieser Schlüssel ist entfallen (in Key aufgegangen)
}

func NewRegistry() *Registry {
	return &Registry{devs: map[string]*Device{}, byEP: map[string]string{}}
}

// Update übernimmt d und liefert die Änderung.
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
			key = old // Nachricht ohne Seriennummer gehört zum bekannten Gerät dieser Adresse
		case od != nil && od.Serial == "":
			// Gerät war nur per Adresse bekannt (z. B. SSDP) – jetzt kennen wir die Seriennummer
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

// Get liefert das Gerät zu key oder nil.
func (r *Registry) Get(key string) *Device { return r.devs[key] }

// Len liefert die Anzahl der Geräte.
func (r *Registry) Len() int { return len(r.devs) }

// All liefert alle Geräte, sortiert nach IP-Adresse.
func (r *Registry) All() []*Device {
	out := make([]*Device, 0, len(r.devs))
	for _, d := range r.devs {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return IPLess(out[i].IP, out[j].IP) })
	return out
}
