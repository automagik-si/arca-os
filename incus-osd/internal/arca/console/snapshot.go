package console

import (
	"reflect"
	"slices"
	"time"
)

// page is the console page in front of the dashboard.
type page int

const (
	pageNone page = iota
	pageDetails
	pageHelp
	pageUpdates
)

// panel is the part of the dashboard the keys act on.
type panel int

const (
	panelInstances panel = iota
	panelEvents
)

// Snapshot is everything the console shows, as its collectors last read it.
type Snapshot struct {
	Node      string
	OSRelease string
	Apps      []App
	Online    Online

	// IPs are the node's addresses, IPv4 first and then one global IPv6. Nil is
	// never read; empty is read, with no address.
	IPs []string

	// Instances are sorted by name, then project. InstancesErr is set when the
	// last read failed; the rows are then the last ones read, marked stale.
	// InstancesAt is the time of the last read that worked.
	Instances    []Instance
	InstancesErr string
	InstancesAt  time.Time

	// Details is the details panel's allowlisted data, sorted by name and project.
	Details []InstanceDetails

	// Selected is the row the keys act on, Page the page in front and Focus the
	// panel Tab last moved to.
	Selected int
	Page     page
	Focus    panel

	// Now is the time the snapshot was taken, for the clock.
	Now time.Time

	// Updates is the automatic updater's state; UpdateSel the failed version the keys act on.
	Updates   Updates
	UpdateSel int
}

// App is one installed application.
type App struct {
	Name, Version string
}

// Online is the node's internet state. Known is false until the first read.
// RouteOnly means no connectivity check runs on this node, so Up only says
// whether a default route exists.
type Online struct {
	Known, Up, RouteOnly bool
	Reason               string
}

// Instance is one row of the instance table. Type is CT, VM or PHONE, Status is
// Incus's (Running, Stopped, ...) and IPv4 is empty when none is known.
// CPUPercent is -1 until the second sample; CPULimit and MemLimit are 0 when the
// instance has no limit of its own.
type Instance struct {
	Name, Project, Type, Status, IPv4 string
	Stale                             bool
	CPUPercent                        float64
	CPULimit                          float64
	MemUsage, MemLimit                int64
	DiskUsage                         int64
	StartedAt                         time.Time
	Processes                         int
}

// InstanceDetails is the details panel's data for one instance: the allowlist of
// the console's privacy rule, and nothing else.
type InstanceDetails struct {
	Name, Project, Type         string
	Description, Status         string
	Processes                   int
	CPUTime                     time.Duration
	MemUsage, MemPeak, MemLimit int64
	Disks                       []DiskUse
	NICs                        []NICUse
	CreatedAt, StartedAt        time.Time
}

// DiskUse is one disk device's use.
type DiskUse struct {
	Name  string
	Usage int64
}

// NICUse is one network device's addresses and traffic.
type NICUse struct {
	Name      string
	Addresses []string
	Rx, Tx    int64
}

// Equal reports whether s and o draw the same screen. Times are compared to the
// minute, the precision the screen shows them at.
func (s Snapshot) Equal(o Snapshot) bool {
	return s.Node == o.Node &&
		s.OSRelease == o.OSRelease &&
		slices.Equal(s.Apps, o.Apps) &&
		s.Online == o.Online &&
		(s.IPs == nil) == (o.IPs == nil) && slices.Equal(s.IPs, o.IPs) &&
		slices.Equal(s.Instances, o.Instances) &&
		reflect.DeepEqual(s.Details, o.Details) &&
		s.InstancesErr == o.InstancesErr &&
		s.Selected == o.Selected && s.Page == o.Page && s.Focus == o.Focus &&
		reflect.DeepEqual(s.Updates, o.Updates) && s.UpdateSel == o.UpdateSel &&
		s.InstancesAt.Truncate(time.Minute).Equal(o.InstancesAt.Truncate(time.Minute)) &&
		s.Now.Truncate(time.Minute).Equal(o.Now.Truncate(time.Minute))
}

// clone returns a copy of s that shares no slice with it.
func (s Snapshot) clone() Snapshot {
	c := s
	c.Apps = slices.Clone(s.Apps)
	c.IPs = slices.Clone(s.IPs)
	c.Instances = slices.Clone(s.Instances)
	c.Updates = s.Updates.clone()
	c.Details = make([]InstanceDetails, len(s.Details))

	for i, d := range s.Details {
		c.Details[i] = d.clone()
	}

	return c
}

// clone returns a copy of d that shares no slice with it.
func (d InstanceDetails) clone() InstanceDetails {
	c := d
	c.Disks = slices.Clone(d.Disks)
	c.NICs = make([]NICUse, len(d.NICs))

	for i, n := range d.NICs {
		n.Addresses = slices.Clone(n.Addresses)
		c.NICs[i] = n
	}

	return c
}
