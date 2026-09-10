package assets

import (
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const PersonalNamespace = "presenter.personal"
const DefaultPersonalQuotaBytes int64 = 100 << 30

var (
	ErrPersonalAssetNotReady = errors.New("personal asset not ready")
	ErrPersonalQuotaExceeded = errors.New("personal quota exceeded")
)

type PersonalSpace struct {
	ID         string `json:"id"`
	Revision   int64  `json:"revision"`
	UsedBytes  int64  `json:"usedBytes"`
	QuotaBytes int64  `json:"quotaBytes"`
}
type PersonalUsage struct {
	ActiveBytes    int64  `json:"activeBytes"`
	TrashBytes     int64  `json:"trashBytes"`
	ProtectedBytes int64  `json:"protectedBytes"`
	UsedBytes      int64  `json:"usedBytes"`
	QuotaBytes     int64  `json:"quotaBytes"`
	OverrideBytes  *int64 `json:"overrideBytes"`
}
type PersonalQuotaExceeded struct {
	UsedBytes     int64 `json:"usedBytes"`
	QuotaBytes    int64 `json:"quotaBytes"`
	RequiredBytes int64 `json:"requiredBytes"`
}

func (e *PersonalQuotaExceeded) Error() string { return ErrPersonalQuotaExceeded.Error() }
func (e *PersonalQuotaExceeded) Unwrap() error { return ErrPersonalQuotaExceeded }

type PersonalTrashPurgeInput struct {
	OperationID string   `json:"operationId"`
	ItemIDs     []string `json:"itemIds,omitempty"`
	All         bool     `json:"all,omitempty"`
}
type PersonalTrashPurgeResult struct {
	PurgedItemIDs []string `json:"purgedItemIds"`
}

func (p PersonalTrashPurgeInput) Validate() error {
	if strings.TrimSpace(p.OperationID) == "" || len(p.OperationID) > 128 || strings.ContainsFunc(p.OperationID, unicode.IsControl) || p.All == (len(p.ItemIDs) > 0) || len(p.ItemIDs) > 1000 {
		return ErrInvalidInput
	}
	seen := make(map[string]struct{}, len(p.ItemIDs))
	for _, id := range p.ItemIDs {
		if strings.TrimSpace(id) == "" || len(id) > 128 || strings.ContainsFunc(id, unicode.IsControl) {
			return ErrInvalidInput
		}
		if _, ok := seen[id]; ok {
			return ErrInvalidInput
		}
		seen[id] = struct{}{}
	}
	return nil
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
	Purged              bool       `json:"purged,omitempty"`
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
	case "create-folder", "create-file", "rename", "replace-content", "move", "delete", "restore":
	default:
		return ErrInvalidInput
	}
	if m.Type == "create-folder" || m.Type == "create-file" || m.Type == "rename" || m.Name != "" {
		if !utf8.ValidString(m.Name) || strings.TrimSpace(m.Name) == "" || utf8.RuneCountInString(m.Name) > 255 || strings.ContainsAny(m.Name, "/\\") || strings.ContainsFunc(m.Name, unicode.IsControl) {
			return ErrInvalidInput
		}
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
