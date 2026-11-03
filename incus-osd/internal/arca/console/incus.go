package console

import (
	"net/http"
	"slices"
	"strings"
	"time"

	incusclient "github.com/lxc/incus/v7/client"
	"github.com/lxc/incus/v7/shared/api"
)

// phoneKey is the instance configuration key that marks an instance as a phone.
const phoneKey = "user.droid.image"

// incusTimeout bounds each request to Incus, so a daemon that hangs leaves the
// table stale instead of stopping its updates.
const incusTimeout = 10 * time.Second

// incusReader reads the instance list from Incus's unix socket, as the REST API
// serves it. It keeps its connection between reads and reconnects after a failure.
type incusReader struct {
	sock string // "" is Incus's own socket
	c    incusclient.InstanceServer
}

// read returns every instance of every project, sorted by name, then project.
func (r *incusReader) read() ([]Instance, error) {
	if r.c == nil {
		c, err := incusclient.ConnectIncusUnix(r.sock, &incusclient.ConnectionArgs{HTTPClient: &http.Client{Timeout: incusTimeout}})
		if err != nil {
			return nil, err
		}

		r.c = c
	}

	full, err := r.c.GetInstancesFullAllProjects(api.InstanceTypeAny)
	if err != nil {
		r.c.Disconnect()
		r.c = nil

		return nil, err
	}

	rows := make([]Instance, 0, len(full))
	for _, inst := range full {
		rows = append(rows, toInstance(inst))
	}

	slices.SortFunc(rows, func(a, b Instance) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}

		return strings.Compare(a.Project, b.Project)
	})

	return rows, nil
}

// toInstance maps one Incus instance to a row: PHONE when the phone key is set,
// else CT or VM, and the first global IPv4 address it reports.
func toInstance(inst api.InstanceFull) Instance {
	kind := "CT"
	if inst.Type == string(api.InstanceTypeVM) {
		kind = "VM"
	}

	if _, ok := inst.ExpandedConfig[phoneKey]; ok {
		kind = "PHONE"
	}

	return Instance{Name: inst.Name, Project: inst.Project, Type: kind, Status: inst.Status, IPv4: firstIPv4(inst.State)}
}

// firstIPv4 is the first global IPv4 address over the interfaces in name order, or
// "" when there is none, as for a stopped instance or a VM without its agent.
func firstIPv4(st *api.InstanceState) string {
	if st == nil {
		return ""
	}

	names := make([]string, 0, len(st.Network))
	for name := range st.Network {
		names = append(names, name)
	}

	slices.Sort(names)

	for _, name := range names {
		if name == "lo" {
			continue
		}

		for _, a := range st.Network[name].Addresses {
			if a.Family == "inet" && a.Scope == "global" {
				return a.Address
			}
		}
	}

	return ""
}

// instanceTable applies reads of the instance list to a snapshot.
type instanceTable struct {
	downSince time.Time
}

// apply records one read made at now. After a failure the last rows stay, marked
// stale, and the error names the time of the first failure in a row.
func (it *instanceTable) apply(s *Snapshot, rows []Instance, err error, now time.Time) {
	if err == nil {
		s.Instances, s.InstancesErr, s.InstancesAt = rows, "", now
		it.downSince = time.Time{}

		return
	}

	if it.downSince.IsZero() {
		it.downSince = now
	}

	for i := range s.Instances {
		s.Instances[i].Stale = true
	}

	s.InstancesErr = "Incus is not running since " + it.downSince.Format("15:04")
}
