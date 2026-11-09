package rest

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/rest/response"
	"github.com/lxc/incus-os/incus-osd/internal/state"
	"github.com/lxc/incus-os/incus-osd/internal/update"
)

// withUpdateHealth returns the system update state with the automatic updater's record.
func withUpdateHealth(s *state.State) api.SystemUpdate {
	ret := s.System.Update
	ret.State.Health = update.Health(s, time.Now())

	return ret
}

// decodeUpdateConfig decodes a PUT body. The automatic updater's settings a client leaves out keep
// their current values, so an older client never turns automatic updates back on by accident.
func decodeUpdateConfig(body io.Reader, out *api.SystemUpdate, current api.SystemUpdateConfig) error {
	raw, err := io.ReadAll(body)
	if err != nil {
		return err
	}

	err = json.Unmarshal(raw, out)
	if err != nil {
		return err
	}

	sent := struct {
		Config map[string]json.RawMessage `json:"config"`
	}{}

	err = json.Unmarshal(raw, &sent)
	if err != nil {
		return err
	}

	_, ok := sent.Config["auto_updates_off"]
	if !ok {
		out.Config.AutoUpdatesOff = current.AutoUpdatesOff
	}

	_, ok = sent.Config["restart_window"]
	if !ok {
		out.Config.RestartWindow = current.RestartWindow
	}

	_, ok = sent.Config["bake_time"]
	if !ok {
		out.Config.BakeTime = current.BakeTime
	}

	return nil
}

// swagger:operation POST /1.0/system/update/:retry system system_post_update_retry
//
//	Try a failed version again
//
//	Releases an OS version, or an application version given as "<application>:<version>", that
//	failed its health check, so it can be applied again, then triggers an update check.
//
//	---
//	consumes:
//	  - application/json
//	produces:
//	  - application/json
//	parameters:
//	  - in: body
//	    name: version
//	    description: The failed version to try again
//	    required: true
//	    schema:
//	      type: object
//	      example: {"version": "202611091832"}
//	responses:
//	  "200":
//	    $ref: "#/responses/EmptySyncResponse"
//	  "400":
//	    $ref: "#/responses/BadRequest"
func (s *Server) apiSystemUpdateRetry(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		_ = response.NotImplemented(nil).Render(w)

		return
	}

	type retryPost struct {
		Version string `json:"version"`
	}

	retry := &retryPost{}

	err := json.NewDecoder(r.Body).Decode(retry)
	if err != nil {
		_ = response.BadRequest(err).Render(w)

		return
	}

	err = update.RetryVersion(r.Context(), s.state, retry.Version)
	if err != nil {
		_ = response.BadRequest(err).Render(w)

		return
	}

	_ = response.EmptySyncResponse.Render(w)
}
