package rest

import (
	"encoding/json"
	"net/http"

	"github.com/lxc/incus-os/incus-osd/internal/rest/response"
	"github.com/lxc/incus-os/incus-osd/internal/update"
)

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
