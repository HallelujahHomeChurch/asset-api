package recordingqueue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hhc/asset-api/internal/derivativequeue"
)

type AzureSender struct {
	lanes map[string]*derivativequeue.AzureSender
}

func NewAzureSender(urls map[string]string) (*AzureSender, error) {
	s := &AzureSender{lanes: make(map[string]*derivativequeue.AzureSender)}
	for lane, url := range urls {
		c, err := derivativequeue.NewAzureSender(url)
		if err != nil {
			return nil, err
		}
		s.lanes[lane] = c
	}
	return s, nil
}
func (s *AzureSender) Send(ctx context.Context, e Event) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return s.lanes[e.Lane].SendPayload(ctx, string(raw))
}

type AzureQueue struct{ *derivativequeue.AzureQueue }

func NewAzureQueue(source, poison string) (*AzureQueue, error) {
	q, err := derivativequeue.NewAzureQueue(source, poison)
	if err != nil {
		return nil, err
	}
	return &AzureQueue{q}, nil
}
func (q *AzureQueue) Poison(ctx context.Context, m derivativequeue.Message) error {
	hash := sha256.Sum256([]byte(m.Body))
	raw, err := json.Marshal(struct {
		Type      string `json:"type"`
		MessageID string `json:"messageId"`
		Digest    string `json:"bodySha256"`
	}{"asset.recording.poisoned.v1", m.ID, hex.EncodeToString(hash[:])})
	if err != nil {
		return err
	}
	if err = q.ForwardPayload(ctx, string(raw)); err != nil {
		return err
	}
	return q.Ack(ctx, m)
}
