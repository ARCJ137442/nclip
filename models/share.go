package models

// ShareManifest groups logical clipboard objects and their alternate representations.
// Encoding is raw for one representation, or zip-v1 for a server-created archive.
type ShareManifest struct {
	Version  int         `json:"version"`
	Encoding string      `json:"encoding"`
	Items    []ShareItem `json:"items"`
}

// ShareItem is one clipboard object or uploaded file, not one MIME representation.
type ShareItem struct {
	Kind  string      `json:"kind"`
	Name  string      `json:"name"`
	Parts []SharePart `json:"parts"`
}

// SharePart identifies a multipart field and its bytes inside the stored archive.
type SharePart struct {
	Key   string `json:"key"`
	MIME  string `json:"mime"`
	Size  int64  `json:"size"`
	Entry string `json:"entry,omitempty"`
}
