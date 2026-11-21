package exporter

import (
	"fmt"
	"slices"

	"github.com/hughobrien/emporiad/internal/emporia"
)

// nameDevices makes every device's labels (device and model) its own, so
// series, /metrics and device facts never share an identity. A device nobody
// else shares a name and model with is left exactly as it is, so stored
// series keep theirs.
//
// Two top-level devices of one model with the same name (or none: two
// unnamed smart plugs) would be one series. The lowest gid keeps the name;
// the next becomes "name (2)", then "name (3)". An empty name becomes "(2)".
// Ordering by gid keeps the result the same on every poll and restart,
// whatever order Emporia lists them in. A nested device that took its
// parent's name takes the parent's new one too, and a nested device with its
// own gid that still shares a name and model gets a suffix as well.
//
// A Vue and its circuit expansion are one monitor with one gid and differ in
// model, so they never collide. The same gid and model listed twice is the
// same device: the second is dropped. The input is not modified.
func nameDevices(ds []emporia.Device) []emporia.Device {
	type id struct {
		gid   int
		model string
	}
	var out []emporia.Device
	seen := map[id]bool{}
	for _, d := range ds {
		if !seen[id{d.Gid, d.Model}] {
			seen[id{d.Gid, d.Model}] = true
			out = append(out, d)
		}
	}
	orig := make([]string, len(out)) // names as Emporia gave them
	for i, d := range out {
		orig[i] = d.Name
	}

	type key struct{ name, model string }
	owner := map[key]int{} // the gid holding a name and model
	claim := func(i int) {
		d := &out[i]
		base := d.Name
		for n := 2; ; n++ {
			k := key{d.Name, d.Model}
			if g, taken := owner[k]; !taken || g == d.Gid {
				owner[k] = d.Gid
				return
			}
			if base == "" {
				d.Name = fmt.Sprintf("(%d)", n)
			} else {
				d.Name = fmt.Sprintf("%s (%d)", base, n)
			}
		}
	}
	byGid := func(idx []int) {
		slices.SortStableFunc(idx, func(a, b int) int { return out[a].Gid - out[b].Gid })
	}

	var top, nested []int
	for i, d := range out {
		if d.Parent == 0 {
			top = append(top, i)
		} else {
			nested = append(nested, i)
		}
	}
	byGid(top)
	for _, i := range top {
		claim(i)
	}
	// A nested device that took its parent's name (Devices copies it)
	// follows the parent's new name. Parents come first in the list.
	for _, i := range nested {
		for j := range i {
			if out[j].Gid == out[i].Parent && orig[j] == orig[i] {
				out[i].Name = out[j].Name
				break
			}
		}
	}
	byGid(nested)
	for _, i := range nested {
		claim(i)
	}
	return out
}
