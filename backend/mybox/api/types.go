// Licensed under Overwatch License Revision 3 with an upstream rclone PR exception.
// See MYBOX-LICENSE at the repository root. Pre-existing third-party rights are preserved.

// Package api contains the MYBOX Open API wire types.
package api

import "time"

// RenameRequest changes a resource name without changing its ID.
type RenameRequest struct {
	Name string `json:"name"`
}

// Resource describes a file or folder.
type Resource struct {
	ID         string    `json:"resourceId"`
	Name       string    `json:"name"`
	Type       string    `json:"type"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

// Listing is a page of directory entries.
type Listing struct {
	Resources []Resource `json:"resources"`
	Metadata  struct {
		NextCursor string `json:"nextCursor"`
	} `json:"responseMetaData"`
}

// CreateFolder is a folder creation request. An absent parent means the root.
type CreateFolder struct {
	Name     string `json:"folderName"`
	ParentID string `json:"parentId,omitempty"`
}

// UploadRequest asks for a single-use upload URL.
type UploadRequest struct {
	Name         string `json:"fileName"`
	Size         int64  `json:"fileSize"`
	ParentID     string `json:"parentId,omitempty"`
	Overwrite    bool   `json:"isOverwrite"`
	ModifiedTime string `json:"modifiedTime"`
	Resume       bool   `json:"resume"`
}

// Upload contains a single-use upload URL and resume offset.
type Upload struct {
	URL    string `json:"uploadUrl"`
	Offset int64  `json:"offset"`
}

// Download contains a single-use download URL.
type Download struct {
	URL string `json:"downloadUrl"`
}

// Storage describes the account quota, including allocated shared storage.
type Storage struct {
	Total int64 `json:"quotaBytes"`
	Used  int64 `json:"usedBytes"`
}
