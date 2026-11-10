package main

import (
	apiupdate "github.com/lxc/incus-os/incus-osd/api/images"
)

// nvidiaAsset returns the update component and file type of an NVIDIA driver asset, or empty values.
func nvidiaAsset(name string) (apiupdate.UpdateFileComponent, apiupdate.UpdateFileType) {
	switch name {
	case "nvidia.raw.gz":
		return apiupdate.UpdateFileComponentNVIDIA, apiupdate.UpdateFileTypeApplication
	case "nvidia-lts.raw.gz":
		return apiupdate.UpdateFileComponentNVIDIALTS, apiupdate.UpdateFileTypeApplication
	case "nvidia.manifest.json.gz":
		return apiupdate.UpdateFileComponentNVIDIA, apiupdate.UpdateFileTypeImageManifest
	case "nvidia-lts.manifest.json.gz":
		return apiupdate.UpdateFileComponentNVIDIALTS, apiupdate.UpdateFileTypeImageManifest
	}

	return "", ""
}

// isNVIDIAAsset returns true when the asset belongs to an NVIDIA driver application.
func isNVIDIAAsset(name string) bool {
	c, _ := nvidiaAsset(name)

	return c != ""
}
