package console

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"

	osapi "github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/state"
)

// testInstances is the fake Incus's instance list: a container with addresses, a
// VM without its agent, a phone in a second project and a stopped container.
func testInstances() []api.InstanceFull {
	running := func(name, project, kind string, cfg map[string]string, st *api.InstanceState) api.InstanceFull {
		return api.InstanceFull{
			Instance: api.Instance{
				Name: name, Project: project, Type: kind, Status: "Running", StatusCode: api.Running,
				ExpandedConfig: cfg,
			},
			State: st,
		}
	}

	nic := func(addrs ...api.InstanceStateNetworkAddress) map[string]api.InstanceStateNetwork {
		return map[string]api.InstanceStateNetwork{
			"lo":   {Addresses: []api.InstanceStateNetworkAddress{{Family: "inet", Address: "127.0.0.1", Netmask: "8", Scope: "local"}}},
			"eth0": {Addresses: addrs},
		}
	}

	return []api.InstanceFull{
		running("web", "default", "container", map[string]string{"image.description": "Example"}, &api.InstanceState{
			Status: "Running", StatusCode: api.Running,
			Network: nic(
				api.InstanceStateNetworkAddress{Family: "inet6", Address: "fe80::2", Netmask: "64", Scope: "link"},
				api.InstanceStateNetworkAddress{Family: "inet6", Address: "fd42::2", Netmask: "64", Scope: "global"},
				api.InstanceStateNetworkAddress{Family: "inet", Address: "10.42.0.2", Netmask: "24", Scope: "global"},
			),
		}),
		running("vm-1", "default", "virtual-machine", map[string]string{}, &api.InstanceState{Status: "Running", StatusCode: api.Running}),
		running("phone-1", "other", "container", map[string]string{phoneKey: "example"}, &api.InstanceState{
			Status: "Running", StatusCode: api.Running,
			Network: nic(api.InstanceStateNetworkAddress{Family: "inet", Address: "10.42.0.3", Netmask: "24", Scope: "global"}),
		}),
		{
			Instance: api.Instance{Name: "probe-1", Project: "default", Type: "container", Status: "Stopped", StatusCode: api.Stopped},
			State:    &api.InstanceState{Status: "Stopped", StatusCode: api.Stopped},
		},
	}
}

// testRows is testInstances as the table shows them.
func testRows() []Instance {
	rows := []Instance{}
	for _, inst := range testInstances() {
		rows = append(rows, toInstance(inst))
	}

	slices.SortFunc(rows, func(a, b Instance) int { return strings.Compare(a.Name, b.Name) })

	return rows
}

// fakeIncus serves /1.0 and the all-projects instance list on a unix socket and
// returns the socket's path and a function that stops it and removes the socket.
func fakeIncus(t *testing.T, instances []api.InstanceFull) (string, func()) {
	t.Helper()

	sock := filepath.Join(t.TempDir(), "incus.sock")

	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}

	reply := func(w http.ResponseWriter, metadata any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.ResponseRaw{Type: api.SyncResponse, Status: "Success", StatusCode: http.StatusOK, Metadata: metadata})
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /1.0", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, api.Server{ServerUntrusted: api.ServerUntrusted{
			APIExtensions: []string{"container_full", "instance_all_projects"},
			APIStatus:     "stable", APIVersion: "1.0", Auth: "trusted",
		}})
	})
	mux.HandleFunc("GET /1.0/instances", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("recursion") != "2" || q.Get("all-projects") != "true" {
			t.Errorf("the instance list was asked for with %q, not recursion=2 and all-projects=true", r.URL.RawQuery)
		}

		reply(w, instances)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() { _ = srv.Serve(l) }()

	stop := func() {
		_ = srv.Close()
		_ = os.Remove(sock)
	}
	t.Cleanup(stop)

	return sock, stop
}

func TestIncusCollect(t *testing.T) {
	sock, _ := fakeIncus(t, testInstances())

	rows, err := (&incusReader{sock: sock}).read()
	if err != nil {
		t.Fatal(err)
	}

	want := []Instance{
		{Name: "phone-1", Project: "other", Type: "PHONE", Status: "Running", IPv4: "10.42.0.3"},
		{Name: "probe-1", Project: "default", Type: "CT", Status: "Stopped"},
		{Name: "vm-1", Project: "default", Type: "VM", Status: "Running"},
		{Name: "web", Project: "default", Type: "CT", Status: "Running", IPv4: "10.42.0.2"},
	}

	if !slices.Equal(rows, want) {
		t.Fatalf("rows\n got %+v\nwant %+v", rows, want)
	}

	// The table shows "—" for an instance without an address.
	snap := tracerSnapshot()
	snap.Instances = rows
	screen := draw(t, snap, 80, 25)

	for _, name := range []string{"probe-1", "vm-1"} {
		line := screenLine(screen, rowOf(screen, name))
		if !strings.HasSuffix(line, "—") {
			t.Errorf("the %s row does not end in —: %q", name, line)
		}
	}

	if line := screenLine(screen, rowOf(screen, "probe-1")); !strings.Contains(line, "CT") || !strings.Contains(line, "STOPPED") {
		t.Errorf("the probe-1 row is %q", line)
	}
}

func TestIncusDown(t *testing.T) {
	sock, stop := fakeIncus(t, testInstances())

	r := &incusReader{sock: sock}
	table := instanceTable{}
	snap := tracerSnapshot()
	snap.Instances, snap.InstancesAt = nil, time.Time{}

	rows, err := r.read()
	table.apply(&snap, rows, err, testNow.Add(-20*time.Minute))

	if err != nil || len(snap.Instances) != 4 || snap.InstancesErr != "" {
		t.Fatalf("the first read: %v, %d rows, %q", err, len(snap.Instances), snap.InstancesErr)
	}

	// Incus goes away: the socket is gone.
	stop()

	first := time.Date(2026, 11, 3, 14, 2, 30, 0, time.UTC)

	for _, at := range []time.Time{first, testNow} {
		rows, err = r.read()
		if err == nil {
			t.Fatal("a read without the socket worked")
		}

		table.apply(&snap, rows, err, at)
	}

	if snap.InstancesErr != "Incus is not running since 14:02" {
		t.Errorf("InstancesErr is %q", snap.InstancesErr)
	}

	if len(snap.Instances) != 4 {
		t.Fatalf("the last rows were not kept: %+v", snap.Instances)
	}

	for _, inst := range snap.Instances {
		if !inst.Stale {
			t.Errorf("%s is not marked stale", inst.Name)
		}
	}

	if !snap.InstancesAt.Equal(testNow.Add(-20 * time.Minute)) {
		t.Errorf("InstancesAt moved to %v", snap.InstancesAt)
	}

	golden(t, "incus-down-80x25.txt", draw(t, snap, 80, 25))
}

// TestHeaderSources reads the header's addresses and online state from fixtures.
func TestHeaderSources(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"sys/class/net/enp5s0", "sys/class/net/wlan0/phy80211", "proc/net"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	oldSys, oldProc, oldAddrs := sysRoot, procRoot, ipAddresses
	t.Cleanup(func() { sysRoot, procRoot, ipAddresses = oldSys, oldProc, oldAddrs })

	sysRoot, procRoot = filepath.Join(root, "sys"), filepath.Join(root, "proc")
	ipAddresses = func(_ context.Context, name string) ([]string, error) {
		return map[string][]string{
			"enp5s0": {"fd42::10", "10.42.0.10", "fd42::11"},
			"wlan0":  {"10.42.1.10", "10.42.0.10"},
		}[name], nil
	}

	s := &state.State{}
	s.System.Network.Config = &osapi.SystemNetworkConfig{
		Interfaces: []osapi.SystemNetworkInterface{{Name: "enp5s0", Addresses: []string{"dhcp4"}}, {Name: "missing0", Addresses: []string{"dhcp4"}}},
	}

	if got, want := readIPs(context.Background(), s), []string{"10.42.0.10", "10.42.1.10", "fd42::10"}; !slices.Equal(got, want) {
		t.Errorf("readIPs = %v, want %v", got, want)
	}

	if got := readIPs(context.Background(), &state.State{}); got == nil {
		t.Error("readIPs returned nil with Wi-Fi present")
	}

	// No connectivity check: only the default route counts.
	if got := readOnline(s); got != (Online{Known: true, RouteOnly: true, Reason: "no default route"}) {
		t.Errorf("without a route: %+v", got)
	}

	route := "Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\nenp5s0\t00000000\t01002A0A\t0003\t0\t0\t0\t00000000\n"
	if err := os.WriteFile(filepath.Join(root, "proc/net/route"), []byte(route), 0o644); err != nil { //nolint:gosec
		t.Fatal(err)
	}

	if got := readOnline(s); got != (Online{Known: true, Up: true, RouteOnly: true}) {
		t.Errorf("with a route: %+v", got)
	}

	s.System.Network.State.Connectivity = &osapi.SystemNetworkConnectivity{Online: false, Reason: "the update server did not answer"}
	if got := readOnline(s); got != (Online{Known: true, Reason: "the update server did not answer"}) {
		t.Errorf("with a connectivity check: %+v", got)
	}
}
