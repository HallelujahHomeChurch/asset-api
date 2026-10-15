// Temporary read-only A4 reconciliation command; remove after A5 acceptance.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

const (
	accountURL    = "https://alivestoragebb99ee6e.blob.core.windows.net"
	containerName = "assets"
	include       = "deleted,deletedwithversions,snapshots,uncommittedblobs,versions"
	maxObjects    = 10000
	maxBytes      = 4 << 20
	pageSize      = 500
	maxPages      = 100
	maxDuration   = 2 * time.Minute
)

var prefixes = [2]string{"assets/account.dsr-export/", "assets/account.dsr-supplement/"}

type object struct {
	Name             string     `json:"name"`
	ETag             *string    `json:"etag"`
	VersionID        string     `json:"versionId"`
	Snapshot         string     `json:"snapshot"`
	Deleted          bool       `json:"deleted"`
	IsCurrentVersion *bool      `json:"isCurrentVersion"`
	HasVersionsOnly  *bool      `json:"hasVersionsOnly"`
	ContentLength    *int64     `json:"contentLength"`
	LastModified     *time.Time `json:"lastModified"`
}

type manifest struct {
	ObservationStart time.Time        `json:"observationStart"`
	ObservationEnd   time.Time        `json:"observationEnd"`
	AccountURL       string           `json:"accountUrl"`
	Container        string           `json:"container"`
	Prefixes         [2]string        `json:"prefixes"`
	Include          string           `json:"include"`
	Complete         bool             `json:"complete"`
	Objects          []object         `json:"objects"`
	Database         databaseSnapshot `json:"database"`
}

func main() {
	if err := execute(); err != nil {
		// Provider errors may contain signed requests, tokens or response headers.
		fmt.Fprintln(os.Stderr, "DSR Blob inventory failed; no completed inventory is available")
		os.Exit(1)
	}
}

func validateTarget(account, container string, args []string) error {
	if account != accountURL || container != containerName || len(args) != 0 {
		return errors.New("exact production target and no arguments required")
	}
	return nil
}

func execute() error {
	if err := validateTarget(os.Getenv("ASSET_AZURE_ACCOUNT_URL"), os.Getenv("ASSET_AZURE_CONTAINER"), os.Args[1:]); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), maxDuration)
	defer cancel()
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return errors.New("credential initialization failed")
	}
	client, err := azblob.NewClient(accountURL, credential, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{
		Transport: readOnlyTransport{next: &http.Client{Timeout: maxDuration, CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirect rejected")
		}}},
		Retry: policy.RetryOptions{MaxRetries: -1},
	}})
	if err != nil {
		return errors.New("storage initialization failed")
	}
	db, err := openInventoryDatabase(os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer db.Close()
	snapshot, err := databaseInventory(ctx, db, "asset")
	if err != nil {
		return err
	}
	return inventory(ctx, client, os.Stdout, snapshot)
}

type readOnlyTransport struct{ next policy.Transporter }

func (t readOnlyTransport) Do(r *http.Request) (*http.Response, error) {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	if r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != "alivestoragebb99ee6e.blob.core.windows.net" || r.URL.User != nil || r.URL.Path != "/assets" || r.URL.RawPath != "" || r.URL.Fragment != "" || q.Get("restype") != "container" || q.Get("comp") != "list" || q.Get("include") != include || q.Get("maxresults") != "500" || (prefix != prefixes[0] && prefix != prefixes[1]) {
		return nil, errors.New("storage request outside inventory scope")
	}
	for key, values := range q {
		if len(values) != 1 || (key != "restype" && key != "comp" && key != "include" && key != "maxresults" && key != "prefix" && key != "marker") {
			return nil, errors.New("unexpected storage query")
		}
	}
	response, err := t.next.Do(r)
	if err != nil {
		return nil, errors.New("storage request failed")
	}
	response.Body = http.MaxBytesReader(nil, response.Body, maxBytes)
	return response, nil
}

func inventory(ctx context.Context, client *azblob.Client, output io.Writer, database databaseSnapshot) error {
	if !database.Complete {
		return errors.New("database inventory incomplete")
	}
	ctx, cancel := context.WithTimeout(ctx, maxDuration)
	defer cancel()
	m := manifest{ObservationStart: time.Now().UTC(), AccountURL: accountURL, Container: containerName, Prefixes: prefixes, Include: include, Objects: []object{}, Database: database}
	if !database.ObservationStart.IsZero() {
		m.ObservationStart = database.ObservationStart
	}
	databaseJSON, err := json.Marshal(database)
	if err != nil {
		return errors.New("database inventory encoding failed")
	}
	usedBytes := len(databaseJSON)
	databaseRows := len(database.Assets) + len(database.Grants) + len(database.Uploads) + len(database.ForeignKeys) + len(database.Dependencies)
	for _, prefix := range prefixes {
		pager := client.NewListBlobsFlatPager(containerName, &azblob.ListBlobsFlatOptions{
			Prefix: &prefix, MaxResults: to.Ptr(int32(pageSize)),
			Include: azblob.ListBlobsInclude{Deleted: true, DeletedWithVersions: true, Snapshots: true, UncommittedBlobs: true, Versions: true},
		})
		markers := map[string]bool{}
		marker := ""
		pages := 0
		for pager.More() {
			pages++
			if pages > maxPages || ctx.Err() != nil {
				return errors.New("inventory page or time limit exceeded")
			}
			page, err := pager.NextPage(ctx)
			if err != nil {
				return errors.New("inventory page unavailable")
			}
			if page.Segment == nil || page.NextMarker == nil || page.ServiceEndpoint == nil || *page.ServiceEndpoint != accountURL+"/" || page.ContainerName == nil || *page.ContainerName != containerName || page.Prefix == nil || *page.Prefix != prefix || stringValue(page.Marker) != marker || len(page.Segment.BlobItems) > pageSize {
				return errors.New("incomplete or unexpected inventory page")
			}
			for _, item := range page.Segment.BlobItems {
				if item == nil || item.Name == nil || !strings.HasPrefix(*item.Name, prefix) || len(*item.Name) == len(prefix) || item.Properties == nil {
					return errors.New("incomplete or out of scope inventory object")
				}
				properties := item.Properties
				entry := object{Name: *item.Name, VersionID: stringValue(item.VersionID), Snapshot: stringValue(item.Snapshot), Deleted: (item.Deleted != nil && *item.Deleted), IsCurrentVersion: item.IsCurrentVersion, HasVersionsOnly: item.HasVersionsOnly, ContentLength: properties.ContentLength, LastModified: properties.LastModified}
				if properties.ETag != nil {
					entry.ETag = to.Ptr(string(*properties.ETag))
				}
				encoded, err := json.Marshal(entry)
				if err != nil {
					return errors.New("inventory object encoding failed")
				}
				usedBytes += len(encoded) + 1
				if len(m.Objects)+databaseRows >= maxObjects || usedBytes > maxBytes {
					return errors.New("inventory count or size limit exceeded")
				}
				m.Objects = append(m.Objects, entry)
			}
			marker = *page.NextMarker
			if marker != "" && markers[marker] {
				return errors.New("inventory continuation repeated")
			}
			markers[marker] = true
		}
		if marker != "" {
			return errors.New("inventory pagination incomplete")
		}
	}
	slices.SortFunc(m.Objects, func(a, b object) int {
		for _, pair := range [][2]string{{a.Name, b.Name}, {a.VersionID, b.VersionID}, {a.Snapshot, b.Snapshot}} {
			if order := strings.Compare(pair[0], pair[1]); order != 0 {
				return order
			}
		}
		return 0
	})
	for i := 1; i < len(m.Objects); i++ {
		a, b := m.Objects[i-1], m.Objects[i]
		if a.Name == b.Name && a.VersionID == b.VersionID && a.Snapshot == b.Snapshot {
			return errors.New("duplicate inventory object")
		}
	}
	if ctx.Err() != nil {
		return errors.New("inventory time limit exceeded")
	}
	m.ObservationEnd = time.Now().UTC()
	m.Complete = true
	payload, err := json.Marshal(m)
	if err != nil || len(payload) > maxBytes {
		return errors.New("inventory manifest encoding or size limit exceeded")
	}
	frame := fmt.Sprintf("HHC_DSR_BLOB_INVENTORY_V1\n%s\nHHC_DSR_BLOB_INVENTORY_COMPLETE sha256=%x bytes=%d\n", payload, sha256.Sum256(payload), len(payload))
	if _, err := io.WriteString(output, frame); err != nil {
		return errors.New("inventory output failed")
	}
	return nil
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
