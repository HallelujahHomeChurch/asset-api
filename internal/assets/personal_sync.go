package assets

import (
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const PersonalNamespace = "presenter.personal"

var ErrPersonalAssetNotReady = errors.New("personal asset not ready")

type PersonalSpace struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}
type PersonalNode struct {
	ID                  string     `json:"id"`
	CollectionID        string     `json:"collectionId"`
	ParentID            string     `json:"parentId,omitempty"`
	Kind                string     `json:"kind"`
	Name                string     `json:"name"`
	AssetID             string     `json:"assetId,omitempty"`
	Revision            int64      `json:"revision"`
	DeletedAt           *time.Time `json:"deletedAt,omitempty"`
	DeletionOperationID string     `json:"-"`
}
type PersonalMutation struct {
	OperationID                string `json:"operationId"`
	Type                       string `json:"type"`
	ItemID                     string `json:"itemId"`
	ParentID                   string `json:"parentId,omitempty"`
	Name                       string `json:"name,omitempty"`
	UploadID                   string `json:"uploadId,omitempty"`
	ExpectedRevision           int64  `json:"expectedRevision,omitempty"`
	ExpectedCollectionRevision int64  `json:"expectedCollectionRevision,omitempty"`
}
type PersonalMutationResult struct {
	ItemID             string `json:"itemId"`
	NodeRevision       int64  `json:"nodeRevision"`
	CollectionRevision int64  `json:"collectionRevision"`
}

func (m PersonalMutation) Validate() error {
	for _, id := range []string{m.OperationID, m.ItemID} {
		if strings.TrimSpace(id) == "" || len(id) > 128 || strings.ContainsFunc(id, unicode.IsControl) {
			return ErrInvalidInput
		}
	}
	if len(m.ParentID) > 128 || len(m.UploadID) > 128 {
		return ErrInvalidInput
	}
	switch m.Type {
	case "create-folder", "create-file", "rename":
		if !utf8.ValidString(m.Name) || strings.TrimSpace(m.Name) == "" || utf8.RuneCountInString(m.Name) > 255 || strings.ContainsAny(m.Name, "/\\") || strings.ContainsFunc(m.Name, unicode.IsControl) {
			return ErrInvalidInput
		}
	case "replace-content", "move", "delete", "restore":
	default:
		return ErrInvalidInput
	}
	if m.Type != "create-folder" && m.Type != "create-file" && m.ExpectedRevision <= 0 {
		return ErrInvalidInput
	}
	if (m.Type == "create-file" || m.Type == "replace-content") && m.UploadID == "" {
		return ErrInvalidInput
	}
	return nil
}

type PersonalChangePage struct {
	Collection PersonalSpace  `json:"collection"`
	Items      []PersonalNode `json:"items"`
	Cursor     string         `json:"nextCursor"`
	HasMore    bool           `json:"hasMore"`
	Reset      bool           `json:"reset"`
}
