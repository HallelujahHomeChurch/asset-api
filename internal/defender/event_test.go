package defender

import "testing"

func TestDecodeScanEvent(t *testing.T) {
	payload := []byte(`[{"id":"event-1","eventType":"Microsoft.Security.MalwareScanningResult","data":{"blobUri":"https://store.blob.core.windows.net/assets/prod/cms.weekly.pdf/2026/07/asset-1/original","scanResultType":"Malicious","scanResultDetails":{"malwareNamesFound":["EICAR-Test-File"]},"eTag":"0x123"}}]`)
	events, err := DecodeEvents(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	if events[0].EventID != "event-1" || events[0].AssetID != "asset-1" || events[0].Status != "infected" {
		t.Fatalf("unexpected event: %+v", events[0])
	}
}

func TestDecodeCleanScanEvent(t *testing.T) {
	payload := []byte(`{"id":"event-2","data":{"blobUri":"https://store.blob.core.windows.net/assets/prod/cms.page.image/2026/07/asset-2/original","scanResultType":"No threats found","eTag":"0x456"}}`)
	events, err := DecodeEvents(payload)
	if err != nil {
		t.Fatal(err)
	}
	if events[0].Status != "clean" {
		t.Fatalf("status = %s", events[0].Status)
	}
}
