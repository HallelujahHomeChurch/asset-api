package derivativequeue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azqueue"
)

type Message struct {
	ID           string
	PopReceipt   string
	Body         string
	DequeueCount int64
}

type poisonRecord struct {
	Type         string `json:"type"`
	Version      int    `json:"version"`
	PoisonID     string `json:"poisonId"`
	Reason       string `json:"reason"`
	BodySHA256   string `json:"bodySha256"`
	DequeueCount int64  `json:"dequeueCount"`
	PoisonedAt   string `json:"poisonedAt"`
	Event        *Event `json:"event,omitempty"`
}

type receiverClient interface {
	DequeueMessage(context.Context, *azqueue.DequeueMessageOptions) (azqueue.DequeueMessagesResponse, error)
	DeleteMessage(context.Context, string, string, *azqueue.DeleteMessageOptions) (azqueue.DeleteMessageResponse, error)
	UpdateMessage(context.Context, string, string, string, *azqueue.UpdateMessageOptions) (azqueue.UpdateMessageResponse, error)
	EnqueueMessage(context.Context, string, *azqueue.EnqueueMessageOptions) (azqueue.EnqueueMessagesResponse, error)
}

type AzureQueue struct {
	source receiverClient
	poison receiverClient
}

func NewAzureQueue(sourceURL, poisonURL string) (*AzureQueue, error) {
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create queue credential: %w", err)
	}
	source, err := azqueue.NewQueueClient(sourceURL, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("create source queue: %w", err)
	}
	poison, err := azqueue.NewQueueClient(poisonURL, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("create poison queue: %w", err)
	}
	return &AzureQueue{source: source, poison: poison}, nil
}

func (q *AzureQueue) Receive(ctx context.Context, visibility time.Duration) (Message, bool, error) {
	seconds := int32(visibility / time.Second)
	response, err := q.source.DequeueMessage(ctx, &azqueue.DequeueMessageOptions{VisibilityTimeout: &seconds})
	if err != nil {
		return Message{}, false, fmt.Errorf("dequeue derivative request: %w", err)
	}
	if len(response.Messages) == 0 {
		return Message{}, false, nil
	}
	value := response.Messages[0]
	if value == nil || value.MessageID == nil || value.PopReceipt == nil || value.MessageText == nil || value.DequeueCount == nil {
		return Message{}, false, errors.New("malformed Azure Queue response")
	}
	return Message{ID: *value.MessageID, PopReceipt: *value.PopReceipt, Body: *value.MessageText, DequeueCount: *value.DequeueCount}, true, nil
}

func (q *AzureQueue) Ack(ctx context.Context, message Message) error {
	_, err := q.source.DeleteMessage(ctx, message.ID, message.PopReceipt, nil)
	return err
}

func (q *AzureQueue) Retry(ctx context.Context, message Message, delay time.Duration) error {
	seconds := int32(delay / time.Second)
	_, err := q.source.UpdateMessage(ctx, message.ID, message.PopReceipt, message.Body, &azqueue.UpdateMessageOptions{VisibilityTimeout: &seconds})
	return err
}

func (q *AzureQueue) ForwardPoison(ctx context.Context, message Message, event *Event, reason string, now time.Time) error {
	digest := sha256.Sum256([]byte(message.Body))
	record := poisonRecord{Type: "asset.derivative.poisoned.v1", Version: 1, PoisonID: message.ID + ":" + reason, Reason: reason, BodySHA256: hex.EncodeToString(digest[:]), DequeueCount: message.DequeueCount, PoisonedAt: now.UTC().Format(time.RFC3339Nano), Event: event}
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	ttl := int32(-1)
	_, err = q.poison.EnqueueMessage(ctx, string(payload), &azqueue.EnqueueMessageOptions{TimeToLive: &ttl})
	return err
}
