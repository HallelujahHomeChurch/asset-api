package defender

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"hhc/asset-api/internal/assets"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/messaging/azservicebus"
)

type Worker struct {
	client   *azservicebus.Client
	receiver *azservicebus.Receiver
	service  *assets.Service
}

func NewWorker(namespace, queue string, service *assets.Service) (*Worker, error) {
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(namespace, ".") {
		namespace += ".servicebus.windows.net"
	}
	client, err := azservicebus.NewClient(namespace, credential, nil)
	if err != nil {
		return nil, err
	}
	receiver, err := client.NewReceiverForQueue(queue, nil)
	if err != nil {
		_ = client.Close(context.Background())
		return nil, err
	}
	return &Worker{client: client, receiver: receiver, service: service}, nil
}

func (w *Worker) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		receiveCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		messages, err := w.receiver.ReceiveMessages(receiveCtx, 8, nil)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Error("receive defender scan results", "error", err)
			continue
		}
		for _, message := range messages {
			events, err := DecodeEvents(message.Body)
			if err == nil {
				for _, event := range events {
					err = w.service.ApplyScanResult(ctx, assets.ScanResult{EventID: event.EventID, AssetID: event.AssetID, Status: assets.ScanStatus(event.Status), Details: event.Details, ETag: event.ETag})
					if err != nil {
						break
					}
				}
			}
			if err != nil {
				slog.Error("process defender scan result", "message_id", message.MessageID, "error", err)
				if message.DeliveryCount >= 5 {
					_ = w.receiver.DeadLetterMessage(ctx, message, &azservicebus.DeadLetterOptions{Reason: stringPtr("asset scan processing failed"), ErrorDescription: stringPtr(err.Error())})
				} else {
					_ = w.receiver.AbandonMessage(ctx, message, nil)
				}
				continue
			}
			if err := w.receiver.CompleteMessage(ctx, message, nil); err != nil {
				slog.Error("complete defender scan message", "error", err)
			}
		}
	}
	return fmt.Errorf("scan worker stopped: %w", ctx.Err())
}

func (w *Worker) Close(ctx context.Context) error {
	if err := w.receiver.Close(ctx); err != nil {
		return err
	}
	return w.client.Close(ctx)
}
func stringPtr(value string) *string { return &value }
