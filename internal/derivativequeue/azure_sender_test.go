package derivativequeue

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azqueue"
)

func TestAzureSenderEmitsOnlyTheDerivativeEnvelope(t *testing.T) {
	client := &captureQueueClient{}
	event := Event{Type: "asset.derivative.requested.v1", Version: 1, EventID: "event-1", AssetID: "asset-1", ETag: "etag-1"}
	if err := (&AzureSender{client: client}).Send(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(client.message), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) != 5 || payload["type"] != event.Type || payload["version"] != float64(1) || payload["eventId"] != event.EventID || payload["assetId"] != event.AssetID || payload["etag"] != event.ETag {
		t.Fatalf("payload=%v", payload)
	}
	if client.options == nil || client.options.TimeToLive == nil || *client.options.TimeToLive != -1 {
		t.Fatalf("options=%+v", client.options)
	}
}

type captureQueueClient struct {
	message string
	options *azqueue.EnqueueMessageOptions
}

func (c *captureQueueClient) EnqueueMessage(_ context.Context, message string, options *azqueue.EnqueueMessageOptions) (azqueue.EnqueueMessagesResponse, error) {
	c.message = message
	c.options = options
	return azqueue.EnqueueMessagesResponse{}, nil
}
