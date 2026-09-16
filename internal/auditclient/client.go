package auditclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

//go:embed asset-producer-fixture.json
var fixtureFiles embed.FS

var ErrInvalidEvent = errors.New("invalid audit event")

var (
	accountID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[47][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	assetID   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	uuidV4    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	requestID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	traceID   = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

type Metadata map[string]any
type Provenance struct{ ActorType, ActorID, RequestID string }
type provenanceKey struct{}

func WithProvenance(ctx context.Context, actorType, actorID, requestID string) context.Context {
	return context.WithValue(ctx, provenanceKey{}, Provenance{actorType, actorID, requestID})
}
func ProvenanceFromContext(ctx context.Context) (Provenance, bool) {
	value, ok := ctx.Value(provenanceKey{}).(Provenance)
	return value, ok
}
func ValidUserRequest(actor, request string) bool {
	return accountID.MatchString(actor) && validRequest(request)
}
func ValidServiceRequest(request string) bool    { return validRequest(request) }
func ValidUploadMetadata(metadata Metadata) bool { return validMetadata("upload", metadata) }

type Action struct {
	Action                 string   `json:"action"`
	Category               string   `json:"category"`
	ResourceOwnerService   string   `json:"resourceOwnerService"`
	ResourceType           string   `json:"resourceType"`
	ResourceIDGrammar      string   `json:"resourceIdGrammar"`
	EventIDGrammar         string   `json:"eventIdGrammar"`
	IdentityStrategy       string   `json:"identityStrategy"`
	Outcome                string   `json:"outcome"`
	Severity               string   `json:"severity"`
	MetadataClassification string   `json:"metadataClassification"`
	RetentionClass         string   `json:"retentionClass"`
	MetadataSchema         string   `json:"metadataSchema"`
	ActorTypes             []string `json:"actorTypes"`
	ServicePrincipals      []string `json:"servicePrincipals"`
}
type Fixture struct {
	SchemaVersion       int             `json:"schemaVersion"`
	SourceService       string          `json:"sourceService"`
	Actions             json.RawMessage `json:"actions"`
	MetadataSchemas     json.RawMessage `json:"metadataSchemas"`
	IDGrammars          json.RawMessage `json:"idGrammars"`
	DenialBindings      json.RawMessage `json:"denialBindings"`
	CredentialFixtures  json.RawMessage `json:"credentialFixtures"`
	CredentialPredicate string          `json:"credentialPredicate"`
	GrammarFixtures     json.RawMessage `json:"grammarFixtures"`
	IdentityFixtures    json.RawMessage `json:"identityFixtures"`
	Checksum            string          `json:"checksum"`
}
type Event struct {
	EventID                string    `json:"eventId"`
	SchemaVersion          int       `json:"schemaVersion"`
	OccurredAt             time.Time `json:"occurredAt"`
	RequestID              string    `json:"requestId"`
	TraceID                string    `json:"traceId,omitempty"`
	SourceService          string    `json:"sourceService"`
	ActorType              string    `json:"actorType"`
	ActorID                string    `json:"actorId"`
	Action                 string    `json:"action"`
	Category               string    `json:"category"`
	ResourceOwnerService   string    `json:"resourceOwnerService"`
	ResourceType           string    `json:"resourceType"`
	ResourceID             string    `json:"resourceId"`
	Outcome                string    `json:"outcome"`
	Severity               string    `json:"severity"`
	MetadataClassification string    `json:"metadataClassification"`
	RetentionClass         string    `json:"retentionClass"`
	Metadata               Metadata  `json:"metadata"`
}

func LoadFixture() (Fixture, error) {
	data, _ := fixtureFiles.ReadFile("asset-producer-fixture.json")
	var fixture Fixture
	if json.Unmarshal(data, &fixture) != nil {
		return Fixture{}, ErrInvalidEvent
	}
	want := fixture.Checksum
	fixture.Checksum = ""
	canonical, _ := json.Marshal(fixture)
	sum := sha256.Sum256(canonical)
	if fixture.SourceService != "asset-api" || want == "" || "sha256:"+hex.EncodeToString(sum[:]) != want {
		return Fixture{}, errors.New("audit fixture checksum mismatch")
	}
	var verified Fixture
	err := json.Unmarshal(data, &verified)
	return verified, err
}
func (f Fixture) Contracts() []Action {
	var values []Action
	_ = json.Unmarshal(f.Actions, &values)
	return values
}
func (f Fixture) contract(action string) (Action, bool) {
	for _, value := range f.Contracts() {
		if value.Action == action {
			return value, true
		}
	}
	return Action{}, false
}

func NewEvent(action, resource, actorType, actor, request string, at time.Time, metadata Metadata) (Event, error) {
	fixture, err := LoadFixture()
	contract, ok := fixture.contract(action)
	if err != nil || !ok {
		return Event{}, ErrInvalidEvent
	}
	event := Event{SchemaVersion: 1, EventID: uuid.NewString(), OccurredAt: at.UTC(), RequestID: request, SourceService: fixture.SourceService, ActorType: actorType, ActorID: actor, Action: action, Category: contract.Category, ResourceOwnerService: contract.ResourceOwnerService, ResourceType: contract.ResourceType, ResourceID: resource, Outcome: contract.Outcome, Severity: contract.Severity, MetadataClassification: contract.MetadataClassification, RetentionClass: contract.RetentionClass, Metadata: metadata}
	if contract.IdentityStrategy == "admin_access_sha256" {
		event.EventID = identityHash("admin-access", action, actor, request, resource)
	}
	_, _, err = event.Canonical()
	return event, err
}

func (e Event) Canonical() ([]byte, string, error) {
	fixture, err := LoadFixture()
	contract, ok := fixture.contract(e.Action)
	if err != nil || !ok || e.SchemaVersion != 1 || e.OccurredAt.IsZero() || !validRequest(e.RequestID) || e.TraceID != "" && !traceID.MatchString(e.TraceID) || e.SourceService != fixture.SourceService || !validActor(contract, e.ActorType, e.ActorID) || e.Category != contract.Category || e.ResourceOwnerService != contract.ResourceOwnerService || e.ResourceType != contract.ResourceType || e.Outcome != contract.Outcome || e.Severity != contract.Severity || e.MetadataClassification != contract.MetadataClassification || e.RetentionClass != contract.RetentionClass || !validResource(contract.ResourceIDGrammar, e.ResourceID) || !validMetadata(contract.MetadataSchema, e.Metadata) || !validIdentity(contract, e) {
		return nil, "", ErrInvalidEvent
	}
	e.OccurredAt = e.OccurredAt.UTC()
	if e.Metadata == nil {
		e.Metadata = Metadata{}
	}
	payload, err := json.Marshal(e)
	if err != nil || len(payload) > 32<<10 {
		return nil, "", ErrInvalidEvent
	}
	sum := sha256.Sum256(payload)
	return payload, hex.EncodeToString(sum[:]), nil
}
func Decode(payload []byte) (Event, error) {
	var event Event
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if len(payload) > 32<<10 || decoder.Decode(&event) != nil || decoder.Decode(new(any)) != io.EOF {
		return event, ErrInvalidEvent
	}
	var envelope struct {
		Metadata json.RawMessage `json:"metadata"`
	}
	if json.Unmarshal(payload, &envelope) != nil || len(bytes.TrimSpace(envelope.Metadata)) == 0 || bytes.TrimSpace(envelope.Metadata)[0] != '{' {
		return event, ErrInvalidEvent
	}
	_, _, err := event.Canonical()
	return event, err
}
func EquivalentForEnqueue(stored, incoming Event) bool {
	storedPayload, storedHash, storedErr := stored.Canonical()
	_, incomingHash, incomingErr := incoming.Canonical()
	if storedErr != nil || incomingErr != nil {
		return false
	}
	if storedHash == incomingHash {
		return true
	}
	fixture, _ := LoadFixture()
	contract, ok := fixture.contract(incoming.Action)
	if !ok || stored.EventID != incoming.EventID || contract.IdentityStrategy != "admin_access_sha256" {
		return false
	}
	incoming.OccurredAt = stored.OccurredAt
	incomingPayload, _, err := incoming.Canonical()
	return err == nil && bytes.Equal(storedPayload, incomingPayload)
}
func validActor(contract Action, actorType, actor string) bool {
	if !slices.Contains(contract.ActorTypes, actorType) {
		return false
	}
	if actorType == "user" {
		return accountID.MatchString(actor)
	}
	return actorType == "service" && slices.Contains(contract.ServicePrincipals, actor)
}
func validIdentity(contract Action, event Event) bool {
	switch contract.IdentityStrategy {
	case "mutation_uuid_v4":
		return contract.EventIDGrammar == "uuid" && uuidV4.MatchString(event.EventID)
	case "admin_access_sha256":
		return contract.EventIDGrammar == "hash" && event.EventID == identityHash("admin-access", event.Action, event.ActorID, event.RequestID, event.ResourceID)
	default:
		return false
	}
}
func validResource(grammar, value string) bool {
	switch grammar {
	case "asset_hex":
		return assetID.MatchString(value)
	case "empty":
		return value == ""
	default:
		return false
	}
}
func validMetadata(schema string, metadata Metadata) bool {
	if metadata == nil {
		metadata = Metadata{}
	}
	switch schema {
	case "empty", "filters":
		return len(metadata) == 0
	case "collection_acl":
		return exactStringFields(metadata, map[string]func(string) bool{"collectionId": assetID.MatchString, "subjectType": func(v string) bool { return v == "user" || v == "role" }})
	case "collection_parent":
		return exactStringFields(metadata, map[string]func(string) bool{"collectionId": assetID.MatchString})
	case "ticket_count":
		return exactCounts(metadata, map[string][2]int64{"normalizedUniqueItemCount": {1, 100}})
	case "items_deleted":
		return exactCounts(metadata, map[string][2]int64{"normalizedUniqueItemCount": {0, 2147483647}, "deletedCount": {0, 2147483647}, "alreadyAbsentCount": {0, 2147483647}})
	case "items_updated":
		return exactCounts(metadata, map[string][2]int64{"normalizedUniqueItemCount": {0, 2147483647}, "updatedCount": {0, 2147483647}, "alreadyAbsentCount": {0, 2147483647}})
	case "upload":
		if !exactStringFields(metadata, map[string]func(string) bool{"namespace": func(string) bool { return true }, "ownerType": func(string) bool { return true }, "purpose": func(string) bool { return true }}) {
			return false
		}
		tuple := metadata["namespace"].(string) + "\x00" + metadata["ownerType"].(string) + "\x00" + metadata["purpose"].(string)
		return slices.Contains([]string{"cms.weekly.pdf\x00bulletin_issue\x00weekly_bulletin", "cms.home.banner\x00page\x00home_banner", "cms.news.cover\x00news\x00", "cms.news.cover\x00news\x00news_cover", "cms.news.cover\x00news\x00news_detail_cover", "cms.news.cover\x00news\x00news_home_cover"}, tuple)
	default:
		return false
	}
}
func exactStringFields(metadata Metadata, fields map[string]func(string) bool) bool {
	if len(metadata) != len(fields) {
		return false
	}
	for key, valid := range fields {
		value, ok := metadata[key].(string)
		if !ok || !valid(value) {
			return false
		}
	}
	return true
}
func exactCounts(metadata Metadata, fields map[string][2]int64) bool {
	if len(metadata) != len(fields) {
		return false
	}
	for key, bounds := range fields {
		var value int64
		switch raw := metadata[key].(type) {
		case int:
			value = int64(raw)
		case int64:
			value = raw
		case float64:
			value = int64(raw)
			if raw != float64(value) {
				return false
			}
		case json.Number:
			var err error
			value, err = raw.Int64()
			if err != nil {
				return false
			}
		default:
			return false
		}
		if value < bounds[0] || value > bounds[1] {
			return false
		}
	}
	return true
}
func validRequest(value string) bool {
	return requestID.MatchString(value) && net.ParseIP(value) == nil && !credentialShaped(value)
}
func credentialShaped(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 3 && len(parts) != 5 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		if _, err := base64.RawURLEncoding.DecodeString(part); err != nil {
			return false
		}
	}
	return true
}
func identityHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

type AppendResult struct {
	Result     string
	HTTPStatus int
	DurationMs int64
}
type Client struct {
	http            *http.Client
	endpoint, token string
}

func New(port int, appID, token string) (*Client, error) {
	if port < 1 || port > 65535 || appID != "audit-log" && appID != "audit-log-test" || strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("invalid audit client configuration")
	}
	return &Client{http: &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, endpoint: fmt.Sprintf("http://127.0.0.1:%d/v1.0/invoke/%s/method/priv/audit/events", port, appID), token: token}, nil
}
func (c *Client) Append(ctx context.Context, payload []byte) (result AppendResult) {
	started := time.Now()
	defer func() { result.DurationMs = time.Since(started).Milliseconds() }()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return AppendResult{Result: "dead_letter"}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Audit-Token", c.token)
	response, err := c.http.Do(request)
	if err != nil {
		return AppendResult{Result: "retry"}
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	result.HTTPStatus = response.StatusCode
	switch {
	case response.StatusCode == 201:
		result.Result = "accepted"
	case response.StatusCode == 200:
		result.Result = "duplicate"
	case response.StatusCode == 409:
		result.Result = "conflict"
	case response.StatusCode == 429 || response.StatusCode >= 500 && response.StatusCode <= 599:
		result.Result = "retry"
	default:
		result.Result = "dead_letter"
	}
	return result
}

type PipelineStats struct {
	PendingCount         int64
	OldestPendingSeconds float64
	DeadLetterCount      int64
}
