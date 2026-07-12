package defender

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

type Event struct {
	EventID string
	AssetID string
	Status  string
	Details string
	ETag    string
}

type envelope struct {
	ID   string `json:"id"`
	Data struct {
		BlobURI           string `json:"blobUri"`
		ScanResultType    string `json:"scanResultType"`
		ETag              string `json:"eTag"`
		ScanResultDetails struct {
			MalwareNamesFound []string `json:"malwareNamesFound"`
		} `json:"scanResultDetails"`
	} `json:"data"`
}

func DecodeEvents(payload []byte) ([]Event, error) {
	var source []envelope
	if len(payload) > 0 && payload[0] == '[' {
		if err := json.Unmarshal(payload, &source); err != nil {
			return nil, err
		}
	} else {
		var item envelope
		if err := json.Unmarshal(payload, &item); err != nil {
			return nil, err
		}
		source = []envelope{item}
	}
	result := make([]Event, 0, len(source))
	for _, item := range source {
		assetID, err := assetIDFromURI(item.Data.BlobURI)
		if err != nil {
			return nil, err
		}
		status := normalizeStatus(item.Data.ScanResultType)
		if status == "" {
			return nil, fmt.Errorf("unsupported scan result %q", item.Data.ScanResultType)
		}
		result = append(result, Event{EventID: item.ID, AssetID: assetID, Status: status, Details: strings.Join(item.Data.ScanResultDetails.MalwareNamesFound, ", "), ETag: item.Data.ETag})
	}
	return result, nil
}

func assetIDFromURI(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return "", err
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid blob uri")
	}
	for index := len(parts) - 1; index >= 0; index-- {
		if parts[index] == "original" && index > 0 {
			return parts[index-1], nil
		}
	}
	return "", fmt.Errorf("asset id missing from blob uri")
}

func normalizeStatus(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch {
	case strings.Contains(value, "no threats"), value == "clean":
		return "clean"
	case strings.Contains(value, "malicious"), strings.Contains(value, "threat"), value == "infected":
		return "infected"
	case strings.Contains(value, "error"), strings.Contains(value, "failed"):
		return "failed"
	default:
		return ""
	}
}
