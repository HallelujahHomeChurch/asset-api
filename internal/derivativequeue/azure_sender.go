package derivativequeue

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azqueue"
)

type queueClient interface {
	EnqueueMessage(context.Context, string, *azqueue.EnqueueMessageOptions) (azqueue.EnqueueMessagesResponse, error)
}

type AzureSender struct {
	client queueClient
}

func NewAzureSender(queueURL string) (*AzureSender, error) {
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create queue credential: %w", err)
	}
	client, err := azqueue.NewQueueClient(queueURL, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("create queue client: %w", err)
	}
	return &AzureSender{client: client}, nil
}

func (s *AzureSender) Send(ctx context.Context, event Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode derivative request: %w", err)
	}
	ttl := int32(-1)
	if _, err := s.client.EnqueueMessage(ctx, string(payload), &azqueue.EnqueueMessageOptions{TimeToLive: &ttl}); err != nil {
		return fmt.Errorf("enqueue derivative request: %w", err)
	}
	return nil
}
