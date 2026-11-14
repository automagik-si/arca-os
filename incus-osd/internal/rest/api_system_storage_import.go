package rest

import (
	"encoding/json"
	"errors"
	"net/http"

	ocapi "github.com/FuturFusion/operations-center/shared/api"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/providers"
	"github.com/lxc/incus-os/incus-osd/internal/rest/response"
)

// decodePost parses the JSON body of a POST request into target, rendering any error.
func decodePost(w http.ResponseWriter, r *http.Request, target any) bool {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		_ = response.NotImplemented(nil).Render(w)

		return false
	}

	err := json.NewDecoder(r.Body).Decode(target)
	if err != nil {
		_ = response.BadRequest(err).Render(w)

		return false
	}

	return true
}

// swagger:operation GET /1.0/system/storage/import system system_get_storage_import
//
//	Get the drives opened for import
//
//	Returns the drives opened read-only for import, what is on them, and the copy jobs.
//
//	---
//	produces:
//	  - application/json
//	responses:
//	  "200":
//	    description: Import state
//	    schema:
//	      type: object
//	      description: Sync response
//	      properties:
//	        type:
//	          description: Response type
//	          example: sync
//	          type: string
//	        status:
//	          type: string
//	          description: Status description
//	          example: Success
//	        status_code:
//	          type: integer
//	          description: Status code
//	          example: 200
//	        metadata:
//	          $ref: "#/definitions/SystemStorageImport"
func (s *Server) apiSystemStorageImport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodGet {
		_ = response.NotImplemented(nil).Render(w)

		return
	}

	_ = response.SyncResponse(true, s.diskImport.State()).Render(w)
}

// swagger:operation POST /1.0/system/storage/import/:open system system_post_storage_import_open
//
//	Open a drive read-only for import
//
//	Opens the drive with the given serial read-only (LUKS, LVM, btrfs, ext4, xfs or zfs) and lists what is on it.
//	The passphrase is used once and never stored or logged.
//
//	---
//	consumes:
//	  - application/json
//	produces:
//	  - application/json
//	parameters:
//	  - in: body
//	    name: configuration
//	    description: The drive to open
//	    required: true
//	    schema:
//	      $ref: "#/definitions/SystemStorageImportOpen"
//	responses:
//	  "200":
//	    $ref: "#/responses/EmptySyncResponse"
//	  "400":
//	    $ref: "#/responses/BadRequest"
//	  "500":
//	    $ref: "#/responses/InternalServerError"
func (s *Server) apiSystemStorageImportOpen(w http.ResponseWriter, r *http.Request) {
	req := &api.SystemStorageImportOpen{}
	if !decodePost(w, r, req) {
		return
	}

	disk, err := s.diskImport.Open(r.Context(), req.Serial, req.Passphrase)
	req.Passphrase = ""

	if err != nil {
		_ = response.BadRequest(err).Render(w)

		return
	}

	_ = response.SyncResponse(true, disk).Render(w)
}

// swagger:operation POST /1.0/system/storage/import/:close system system_post_storage_import_close
//
//	Close an import drive
//
//	Unmounts and closes every layer opened for import and makes the drive writable again.
//
//	---
//	consumes:
//	  - application/json
//	produces:
//	  - application/json
//	parameters:
//	  - in: body
//	    name: configuration
//	    description: The drive to close
//	    required: true
//	    schema:
//	      $ref: "#/definitions/SystemStorageImportClose"
//	responses:
//	  "200":
//	    $ref: "#/responses/EmptySyncResponse"
//	  "400":
//	    $ref: "#/responses/BadRequest"
//	  "500":
//	    $ref: "#/responses/InternalServerError"
func (s *Server) apiSystemStorageImportClose(w http.ResponseWriter, r *http.Request) {
	req := &api.SystemStorageImportClose{}
	if !decodePost(w, r, req) {
		return
	}

	err := s.diskImport.Close(r.Context(), req.Serial)
	if err != nil {
		_ = response.InternalError(err).Render(w)

		return
	}

	_ = response.EmptySyncResponse.Render(w)
}

// swagger:operation POST /1.0/system/storage/import/:copy system system_post_storage_import_copy
//
//	Copy an item of an import drive into a native pool
//
//	Starts a background copy of one instance, custom volume or folder; its progress is in the import state.
//	A copied instance arrives stopped, with autostart off.
//
//	---
//	consumes:
//	  - application/json
//	produces:
//	  - application/json
//	parameters:
//	  - in: body
//	    name: configuration
//	    description: What to copy and where
//	    required: true
//	    schema:
//	      $ref: "#/definitions/SystemStorageImportCopyRequest"
//	responses:
//	  "200":
//	    $ref: "#/responses/EmptySyncResponse"
//	  "400":
//	    $ref: "#/responses/BadRequest"
//	  "500":
//	    $ref: "#/responses/InternalServerError"
func (s *Server) apiSystemStorageImportCopy(w http.ResponseWriter, r *http.Request) {
	req := &api.SystemStorageImportCopyRequest{}
	if !decodePost(w, r, req) {
		return
	}

	id, err := s.diskImport.Copy(r.Context(), *req)
	if err != nil {
		_ = response.BadRequest(err).Render(w)

		return
	}

	_ = response.SyncResponse(true, map[string]int{"id": id}).Render(w)
}

// swagger:operation POST /1.0/system/storage/import/:convert system system_post_storage_import_convert
//
//	Turn an import drive into a native pool
//
//	Wipes the drive and creates an encrypted single-drive pool that Incus uses. The confirmation must repeat the serial.
//
//	---
//	consumes:
//	  - application/json
//	produces:
//	  - application/json
//	parameters:
//	  - in: body
//	    name: configuration
//	    description: The drive and the new pool's name
//	    required: true
//	    schema:
//	      $ref: "#/definitions/SystemStorageImportConvert"
//	responses:
//	  "200":
//	    $ref: "#/responses/EmptySyncResponse"
//	  "400":
//	    $ref: "#/responses/BadRequest"
//	  "500":
//	    $ref: "#/responses/InternalServerError"
func (s *Server) apiSystemStorageImportConvert(w http.ResponseWriter, r *http.Request) {
	req := &api.SystemStorageImportConvert{}
	if !decodePost(w, r, req) {
		return
	}

	if req.Confirm == "" {
		_ = response.BadRequest(errors.New("the serial must be repeated in confirm")).Render(w)

		return
	}

	err := s.diskImport.Convert(r.Context(), req.Serial, req.Pool, req.Confirm)
	if err != nil {
		_ = response.InternalError(err).Render(w)

		return
	}

	err = providers.Notify(r.Context(), s.state, ocapi.ServerSelfUpdateCauseStorageConfigChanged)
	if err != nil {
		_ = response.InternalError(err).Render(w)

		return
	}

	_ = response.EmptySyncResponse.Render(w)
}
