package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"hhc/asset-api/internal/meetingclient"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azqueue"
)

const (
	pulseDepth = 20
	pulseTTL   = 120 * time.Second
)

func main() {
	if err := execute(context.Background()); err != nil {
		slog.Error("scan warmer failed", "error", err)
		os.Exit(1)
	}
}

func execute(ctx context.Context) error {
	baseURL := strings.TrimSpace(os.Getenv("MEETING_API_BASE_URL"))
	audience := strings.TrimRight(strings.TrimSpace(os.Getenv("MEETING_API_AUDIENCE")), "/")
	queueURL := strings.TrimSpace(os.Getenv("ASSET_SCAN_WARM_QUEUE_URL"))
	if baseURL == "" || audience == "" || queueURL == "" {
		return fmt.Errorf("MEETING_API_BASE_URL, MEETING_API_AUDIENCE, and ASSET_SCAN_WARM_QUEUE_URL are required")
	}
	lead, err := duration("ASSET_SCAN_WARM_LEAD", 5*time.Minute)
	if err != nil {
		return err
	}
	tail, err := duration("ASSET_SCAN_WARM_TAIL", 10*time.Minute)
	if err != nil {
		return err
	}
	timeout, err := duration("ASSET_SCAN_WARM_HTTP_TIMEOUT", 10*time.Second)
	if err != nil {
		return err
	}
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return err
	}
	queue, err := azqueue.NewQueueClient(queueURL, credential, nil)
	if err != nil {
		return err
	}
	client := meetingclient.New(baseURL, &http.Client{Timeout: timeout}, func(ctx context.Context) (string, error) {
		accessToken, err := credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{audience + "/.default"}})
		return accessToken.Token, err
	})
	return run(ctx, time.Now().UTC(), lead, tail, client.ListSyncWindows, func(ctx context.Context) error {
		ttl := int32(pulseTTL / time.Second)
		_, err := queue.EnqueueMessage(ctx, "warm", &azqueue.EnqueueMessageOptions{TimeToLive: &ttl})
		return err
	})
}

func run(ctx context.Context, now time.Time, lead, tail time.Duration, list func(context.Context, time.Time, time.Time) ([]meetingclient.Window, error), pulse func(context.Context) error) error {
	windows, err := list(ctx, now.Add(-tail), now.Add(lead))
	if err != nil {
		return err
	}
	for _, window := range windows {
		if !now.Before(window.StartsAt.Add(-lead)) && !now.After(window.EndsAt.Add(tail)) {
			for range pulseDepth {
				if err := pulse(ctx); err != nil {
					return err
				}
			}
			return nil
		}
	}
	return nil
}

func duration(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 || parsed > 24*time.Hour {
		return 0, fmt.Errorf("invalid %s %s", name, strconv.Quote(value))
	}
	return parsed, nil
}
