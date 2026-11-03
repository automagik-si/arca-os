package console

import (
	"slices"
	"time"
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

	// Now is the time the snapshot was taken, for the clock.
	Now time.Time
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
type Instance struct {
	Name, Project, Type, Status, IPv4 string
	Stale                             bool
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
		s.InstancesErr == o.InstancesErr &&
		s.InstancesAt.Truncate(time.Minute).Equal(o.InstancesAt.Truncate(time.Minute)) &&
		s.Now.Truncate(time.Minute).Equal(o.Now.Truncate(time.Minute))
}

// clone returns a copy of s that shares no slice with it.
func (s Snapshot) clone() Snapshot {
	c := s
	c.Apps = slices.Clone(s.Apps)
	c.IPs = slices.Clone(s.IPs)
	c.Instances = slices.Clone(s.Instances)

	return c
}
