package r2_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"hhc/asset-api/internal/assets"
	"hhc/asset-api/internal/migrations"
	"hhc/asset-api/internal/postgres"
	"hhc/asset-api/internal/storage/r2"
)

func TestAudioPointerRecoveryAcrossCommitAndResponseLoss(t *testing.T) {
	for _, mode := range []string{"commit", "response"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			dsn := os.Getenv("TEST_DATABASE_URL")
			if dsn == "" {
				t.Skip("requires disposable PostgreSQL")
			}
			cfg, err := pgx.ParseConfig(dsn)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Host != "localhost" && cfg.Host != "127.0.0.1" {
				t.Fatal("requires loopback PostgreSQL")
			}
			admin := stdlib.OpenDB(*cfg)
			defer admin.Close()
			schema := fmt.Sprintf("audio_recovery_%d", time.Now().UnixNano())
			if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
				t.Fatal(err)
			}
			defer admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`)
			cfg.RuntimeParams["search_path"] = schema
			db := stdlib.OpenDB(*cfg)
			defer db.Close()
			if err := migrations.Run(ctx, db); err != nil {
				t.Fatal(err)
			}
			inv := assets.RecordingPackageInventory{SchemaVersion: 1, PresetVersion: "hls-v1", Renditions: []assets.RecordingRendition{{Name: "720p", Width: 1280, Height: 720, FrameRate: 30, VideoBitrate: 1500000, AudioBitrate: 128000, DurationSeconds: 5, SegmentCount: 1}}}
			for _, path := range []string{"master.m3u8", "720p/index.m3u8", "720p/init.mp4", "720p/seg-000000.m4s"} {
				inv.Objects = append(inv.Objects, assets.RecordingPackageObject{Path: path, SizeBytes: 100, SHA256: strings.Repeat("a", 64)})
			}
			inv.InventoryDigest, _ = assets.RecordingInventoryDigest(inv)
			size, err := assets.ValidateRecordingInventory(inv)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			p := assets.RecordingPackage{ID: "audio-recover", SessionID: "audio-recover", OwnerService: "hhc-web-api", ActorID: "actor", RecordingID: "recording", IdempotencyKey: "audio-recover", State: "uploading", SizeBytes: size, CreatedAt: now, ExpiresAt: now.Add(time.Hour), Inventory: inv}
			repo := postgres.NewRecordingPackageStore(db)
			if err := repo.Create(ctx, p); err != nil {
				t.Fatal(err)
			}
			if err := repo.Freeze(ctx, p.ID, now); err != nil {
				t.Fatal(err)
			}
			validation, err := repo.ClaimPackageValidation(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.FinishPackageValidation(ctx, p.ID, validation.ClaimID, true, ""); err != nil {
				t.Fatal(err)
			}
			first, err := repo.ClaimPackageAudio(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			objects := map[string][]byte{}
			conflicts := 0
			lost := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				key := strings.TrimPrefix(r.URL.Path, "/test/")
				if r.Method != "PUT" || r.Header.Get("If-None-Match") != "*" {
					t.Error("unsafe write")
					w.WriteHeader(400)
					return
				}
				if _, exists := objects[key]; exists {
					conflicts++
					w.Header().Set("Content-Type", "application/xml")
					w.WriteHeader(412)
					fmt.Fprint(w, `<Error><Code>PreconditionFailed</Code></Error>`)
					return
				}
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				objects[key] = data
				if mode == "response" && !lost && strings.HasSuffix(key, "current.json") {
					lost = true
					connection, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					connection.Close()
				}
			}))
			defer server.Close()
			storage := r2.NewAudioRecoveryTestStore(server)
			base := first.Package.FinalPrefix + "audio/aac-hls-v1/"
			putAttempt := func(claim string) {
				t.Helper()
				if err := storage.PutPackageAudio(ctx, base+claim+"/seg-000000.ts", []byte("complete AAC transport")); err != nil {
					t.Fatal(err)
				}
				if err := storage.PutPackageAudio(ctx, base+claim+"/index.m3u8", []byte("#EXTM3U\n#EXT-X-ENDLIST\n")); err != nil {
					t.Fatal(err)
				}
			}
			publish := func(claim string) func(context.Context) error {
				return func(ctx context.Context) error {
					data, _ := json.Marshal(map[string]string{"attempt": claim})
					return storage.PutPackageAudio(ctx, base+"current.json", data)
				}
			}
			putAttempt(first.ClaimID)
			if mode == "commit" {
				if _, err := db.Exec(`CREATE FUNCTION reject_audio_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.state='ready' THEN RAISE EXCEPTION 'injected audio commit failure'; END IF; RETURN NEW; END $$; CREATE CONSTRAINT TRIGGER reject_audio_commit AFTER UPDATE ON recording_audio DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_audio_commit()`); err != nil {
					t.Fatal(err)
				}
			}
			if err := repo.FinishPackageAudio(ctx, p.ID, first.ClaimID, true, publish(first.ClaimID)); err == nil {
				t.Fatal("lost response/commit acknowledged")
			}
			if mode == "commit" {
				if _, err := db.Exec(`DROP TRIGGER reject_audio_commit ON recording_audio; DROP FUNCTION reject_audio_commit()`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(`UPDATE recording_audio SET claimed_until=now()-interval '1 second'; UPDATE recording_processing_slots SET leased_until=now()-interval '1 second' WHERE claim_id IS NOT NULL`); err != nil {
				t.Fatal(err)
			}
			second, err := repo.ClaimPackageAudio(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if second.ClaimID == first.ClaimID {
				t.Fatal("reused attempt")
			}
			putAttempt(second.ClaimID)
			if err := repo.FinishPackageAudio(ctx, p.ID, second.ClaimID, true, publish(second.ClaimID)); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			var pointer map[string]string
			err = json.Unmarshal(objects[base+"current.json"], &pointer)
			gotConflicts := conflicts
			mu.Unlock()
			if err != nil || pointer["attempt"] != first.ClaimID || gotConflicts != 1 {
				t.Fatalf("winner replaced or 412 not recovered: %v conflicts=%d %v", pointer, gotConflicts, err)
			}
			ready, err := repo.Get(ctx, p.ID)
			if err != nil || ready.Audio == nil || ready.Audio.State != "ready" {
				t.Fatalf("not converged %+v %v", ready.Audio, err)
			}
			var deleted []string
			remove := func(_ context.Context, keys []string) error {
				deleted = append(deleted, keys...)
				mu.Lock()
				defer mu.Unlock()
				for _, key := range keys {
					delete(objects, key)
				}
				return nil
			}
			if err := repo.CleanupPackage(ctx, p.ID, remove); err != nil {
				t.Fatal(err)
			}
			for _, key := range deleted {
				if strings.Contains(key, "/audio/") {
					t.Fatal("winner deleted before grant grace")
				}
			}
			if _, err := db.Exec(`UPDATE recording_packages SET media_expires_at=now()-interval '2 hours',cleanup_after=now() WHERE id=$1`, p.ID); err != nil {
				t.Fatal(err)
			}
			if err := repo.CleanupPackage(ctx, p.ID, remove); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			remaining := len(objects)
			mu.Unlock()
			if remaining != 0 {
				t.Fatalf("failed/winning audio retained after grace: %d", remaining)
			}
		})
	}
}
