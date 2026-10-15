package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func testClient(t *testing.T, transport policy.Transporter) *azblob.Client {
	t.Helper()
	c, err := azblob.NewClientWithNoCredential(accountURL, &azblob.ClientOptions{ClientOptions: azcore.ClientOptions{
		Transport: readOnlyTransport{next: transport}, Retry: policy.RetryOptions{MaxRetries: -1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func page(r *http.Request, items, next string) *http.Response {
	body := fmt.Sprintf(`<EnumerationResults ServiceEndpoint="%s/" ContainerName="assets"><Prefix>%s</Prefix><Marker>%s</Marker><Blobs>%s</Blobs><NextMarker>%s</NextMarker></EnumerationResults>`, accountURL, r.URL.Query().Get("prefix"), r.URL.Query().Get("marker"), items, next)
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/xml"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func item(name, extra string) string {
	return `<Blob><Name>` + name + `</Name><Deleted>false</Deleted><Snapshot></Snapshot>` + extra + `<Properties><Etag>etag</Etag><Last-Modified>Thu, 15 Oct 2026 00:00:00 GMT</Last-Modified><Content-Length>42</Content-Length></Properties></Blob>`
}

func decodeFrame(t *testing.T, output []byte) manifest {
	t.Helper()
	lines := strings.Split(string(output), "\n")
	if len(lines) != 4 || lines[0] != "HHC_DSR_BLOB_INVENTORY_V1" || lines[3] != "" {
		t.Fatalf("invalid frame: %q", output)
	}
	digest := sha256.Sum256([]byte(lines[1]))
	if lines[2] != fmt.Sprintf("HHC_DSR_BLOB_INVENTORY_COMPLETE sha256=%x bytes=%d", digest, len(lines[1])) {
		t.Fatal("bad completion digest")
	}
	var result manifest
	if err := json.Unmarshal([]byte(lines[1]), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Complete || result.ObservationEnd.Before(result.ObservationStart) {
		t.Fatal("incomplete observation")
	}
	return result
}

func TestInventoryBothPrefixesMultiPageAndVersions(t *testing.T) {
	var requests []string
	c := testClient(t, transportFunc(func(r *http.Request) (*http.Response, error) {
		q := r.URL.Query()
		if r.Method != http.MethodGet || r.URL.Host != "alivestoragebb99ee6e.blob.core.windows.net" || r.URL.Path != "/assets" || q.Get("comp") != "list" || q.Get("restype") != "container" || q.Get("include") != "deleted,deletedwithversions,snapshots,uncommittedblobs,versions" || q.Get("maxresults") != "500" {
			t.Fatalf("unbounded request %s %s", r.Method, r.URL)
		}
		prefix := q.Get("prefix")
		requests = append(requests, prefix+q.Get("marker"))
		switch q.Get("marker") {
		case "":
			return page(r, item(prefix+"z", `<VersionId>v2</VersionId><IsCurrentVersion>true</IsCurrentVersion>`), "next"), nil
		case "next":
			return page(r, item(prefix+"a", `<Snapshot>2026-10-15T00:00:00Z</Snapshot><Deleted>true</Deleted><HasVersionsOnly>true</HasVersionsOnly>`), ""), nil
		default:
			t.Fatal("unexpected continuation")
			return nil, nil
		}
	}))
	var output bytes.Buffer
	if err := inventory(context.Background(), c, &output, databaseSnapshot{Complete: true}); err != nil {
		t.Fatal(err)
	}
	m := decodeFrame(t, output.Bytes())
	if len(requests) != 4 || len(m.Objects) != 4 || m.AccountURL != accountURL || m.Container != containerName || m.Prefixes != prefixes {
		t.Fatalf("wrong scope/result: %+v", m)
	}
	for i, object := range m.Objects {
		if object.ETag == nil || *object.ETag != "etag" || object.ContentLength == nil || *object.ContentLength != 42 || object.LastModified == nil {
			t.Fatal("missing metadata")
		}
		if i > 0 && m.Objects[i-1].Name > object.Name {
			t.Fatal("unsorted objects")
		}
	}
	if m.Objects[0].Snapshot == "" || !m.Objects[0].Deleted || m.Objects[0].HasVersionsOnly == nil || !*m.Objects[0].HasVersionsOnly || m.Objects[1].VersionID != "v2" || m.Objects[1].IsCurrentVersion == nil || !*m.Objects[1].IsCurrentVersion {
		t.Fatal("version/snapshot metadata lost")
	}
}

func TestInventoryEmptyAndDeterministic(t *testing.T) {
	for _, names := range [][]string{nil, {"z", "a"}, {"a", "z"}} {
		c := testClient(t, transportFunc(func(r *http.Request) (*http.Response, error) {
			var items string
			for _, name := range names {
				items += item(r.URL.Query().Get("prefix")+name, "")
			}
			return page(r, items, ""), nil
		}))
		var output bytes.Buffer
		if err := inventory(context.Background(), c, &output, databaseSnapshot{Complete: true}); err != nil {
			t.Fatal(err)
		}
		m := decodeFrame(t, output.Bytes())
		if m.Objects == nil || len(m.Objects) != 2*len(names) {
			t.Fatal("wrong object count")
		}
		if len(names) > 0 && !strings.HasSuffix(m.Objects[0].Name, "/a") {
			t.Fatal("nondeterministic sort")
		}
	}
}

func TestInventoryFailuresEmitNothing(t *testing.T) {
	for _, scenario := range []string{"auth", "second-page", "malformed", "missing-segment", "missing-marker", "out-of-scope", "missing-name", "count", "size", "repeated-marker", "deadline"} {
		t.Run(scenario, func(t *testing.T) {
			calls := 0
			c := testClient(t, transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				prefix := r.URL.Query().Get("prefix")
				switch scenario {
				case "auth":
					return &http.Response{StatusCode: 403, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("secret-provider-payload")), Request: r}, nil
				case "second-page":
					if calls == 1 {
						return page(r, item(prefix+"a", ""), "next"), nil
					}
					return nil, fmt.Errorf("secret-signed-url")
				case "malformed":
					result := page(r, "", "")
					result.Body = io.NopCloser(strings.NewReader("<broken>"))
					return result, nil
				case "missing-segment":
					result := page(r, "", "")
					raw, _ := io.ReadAll(result.Body)
					result.Body = io.NopCloser(strings.NewReader(strings.ReplaceAll(string(raw), "<Blobs></Blobs>", "")))
					return result, nil
				case "missing-marker":
					result := page(r, "", "")
					raw, _ := io.ReadAll(result.Body)
					result.Body = io.NopCloser(strings.NewReader(strings.ReplaceAll(string(raw), "<NextMarker></NextMarker>", "")))
					return result, nil
				case "out-of-scope":
					return page(r, item("assets/account.avatar/keep", ""), ""), nil
				case "missing-name":
					return page(r, "<Blob><Properties/></Blob>", ""), nil
				case "count":
					return page(r, strings.Repeat(item(prefix+"a", ""), 501), ""), nil
				case "size":
					return page(r, item(prefix+strings.Repeat("a", maxBytes), ""), ""), nil
				case "repeated-marker":
					return page(r, "", "same"), nil
				case "deadline":
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				return nil, fmt.Errorf("unexpected")
			}))
			ctx := context.Background()
			if scenario == "deadline" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Millisecond)
				defer cancel()
			}
			var output bytes.Buffer
			err := inventory(ctx, c, &output, databaseSnapshot{Complete: true})
			if err == nil || output.Len() != 0 {
				t.Fatalf("failure produced manifest: %v, %d bytes", err, output.Len())
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("provider error leaked")
			}
		})
	}
}

func TestExactTargetAndNoArguments(t *testing.T) {
	if err := validateTarget(accountURL, containerName, nil); err != nil {
		t.Fatal(err)
	}
	for _, target := range [][2]string{{accountURL + "/", containerName}, {accountURL + "?sig=secret", containerName}, {"https://other.blob.core.windows.net", containerName}, {accountURL, "other"}, {"", ""}} {
		if err := validateTarget(target[0], target[1], nil); err == nil {
			t.Fatal("accepted different target")
		}
	}
	if err := validateTarget(accountURL, containerName, []string{"--prefix=assets/"}); err == nil {
		t.Fatal("accepted arbitrary input")
	}
}

func TestReadOnlyTransportRejectsChanges(t *testing.T) {
	for _, raw := range []string{accountURL + "/assets?comp=list&restype=container&prefix=assets/account.avatar/", accountURL + "/assets/blob", "https://other.example/assets", accountURL + "/assets?sig=secret"} {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPost, http.MethodHead} {
			r, _ := http.NewRequest(method, raw, nil)
			transport := readOnlyTransport{next: transportFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("invalid request reached network")
				return nil, nil
			})}
			if _, err := transport.Do(r); err == nil {
				t.Fatal("accepted invalid request")
			}
		}
	}
}
