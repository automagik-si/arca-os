package diskimport

import (
	"encoding/json"
)

// idEntry is one range of an Incus id map, as stored in volatile.last_state.idmap.
type idEntry struct {
	IsUID    bool  `json:"Isuid"`    //nolint:tagliatelle
	IsGID    bool  `json:"Isgid"`    //nolint:tagliatelle
	HostID   int64 `json:"Hostid"`   //nolint:tagliatelle
	NSID     int64 `json:"Nsid"`     //nolint:tagliatelle
	MapRange int64 `json:"Maprange"` //nolint:tagliatelle
}

// idMap maps the old host's ids back to the container's own.
type idMap []idEntry

func parseIDMap(data string) (idMap, error) {
	var m idMap

	err := json.Unmarshal([]byte(data), &m)
	if err != nil {
		return nil, err
	}

	if len(m) == 0 {
		return nil, nil
	}

	return m, nil
}

// unshift returns the container ids of host ids, -1 when an id is outside every range; -1 in stays -1.
func (m idMap) unshift(uid int64, gid int64) (int64, int64) {
	u, g := int64(-1), int64(-1)

	for _, e := range m {
		if e.IsUID && u == -1 && uid >= e.HostID && uid < e.HostID+e.MapRange {
			u = uid - e.HostID + e.NSID
		}

		if e.IsGID && g == -1 && gid >= e.HostID && gid < e.HostID+e.MapRange {
			g = gid - e.HostID + e.NSID
		}
	}

	return u, g
}
