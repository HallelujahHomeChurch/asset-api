# Recording Pipeline Optimization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans for the recommended native execution, or superpowers:subagent-driven-development if the user selects delegation. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 縮短 CLI／Browser 錄影從開始處理到可觀看的時間，保留完整驗證，且同輸入完整估算成本不超過基準 105%。

**Architecture:** 在現有 Asset 共享驗證內實作最多兩個片段的 copy／hash／probe／decode pipeline，init 只在同 attempt／rendition 快取。進度由 Asset fenced claim 持久化，CMS 經 owner-bound 投影回傳安全摘要，CLI／Admin 沿用現有輪詢。資源、排程、權限與媒體格式維持不變。

**Tech Stack:** Go／database/sql／PostgreSQL、既有 FFmpeg／ffprobe、R2 S3 storage client、TypeScript／React、既有 pnpm／Vitest／OpenAPI codegen；不新增 runtime dependencies。

**Spec:** `docs/superpowers/specs/2026-10-06-recording-pipeline-optimization-design.md`（此 Asset docs 分支，commit `73d486a`）。使用者已於本輪要求開始優化；此計劃仍需書面審閱與選定執行方式後才修改產品。

## Global Constraints

- 每 Job 4 vCPU／8 GiB，全域最多兩個長處理 slot，每 Job 共用最多兩個 fragment worker。
- scratch 最多 1 GiB，另保留至少 128 MiB free space；不落地完整原始檔或 HLS package。
- 保留三畫質、現有 bitrate、30 秒分段、兩次 ffprobe、完整解碼、IDR／時間軸／跨畫質／SHA 檢查及 atomic ready。
- 原始檔唯讀；operation ID／human／SA／續傳／cover／publish 語意不變，進度不等於 ready。
- 三次 attempt、五分鐘 retry、330 分鐘 deadline、30 秒 heartbeat；進度平常最多每 30 秒持久化，階段／terminal 額外 flush。
- 不改 cron、資源、RBAC、DNS／bucket、Browser 逐畫質 encode、一般 Blob 掃毒、追溯或保留政策 activation。
- 成本目標不增加；基準 105% 是停下重新確認的上限，不是可用免費額度抵銷的理由。
- 各 repo 最新 origin/main 獨立工作樹／分支／PR；required CI 全通過，不直接部署未合併程式。

## Review Focus

1. 亂序完成與低 fps／短音訊尾段：所有片段仍按 index 驗時間軸，不放寬 threshold（Task 3）。
2. 舊 image／缺 progress 與跨 attempt：顯示未知，舊 claim 不能更新新 progress 或 ready（Tasks 2、5、6）。
3. SA 上傳但由其他管理員查看：可見安全摘要，不能簽上傳 URL 或取得 uploader capabilities（Task 4）。
4. 最大物件／兩個長 Job 與取消：scratch 有界，所有 child 退出後才 finish／cleanup（Tasks 1、3）。
5. ready 後自訂封面／發布失敗：resume 不重傳已完成影片，完成狀態與成本不能只量到 ready（Tasks 5、7）。

## Repo 與檔案地圖

此計劃檔案路徑按 owner repo 相對表示；repo 根目錄位於 `/Users/rayselfs/Projects/hhc/website`。基準檢視：Asset `97d8280`、CMS `41cb862`、shared `047108b`、CLI `45b6a7e`、Admin `9132249`、Gateway `df513c5`。執行前重新 fetch，不將較新的 bulletin／account 功能倒退。

| Owner | 檔案責任 |
| --- | --- |
| asset-api | `internal/recordingvalidation/package*.go`：共享 pipeline；`internal/recordingprocessing/worker.go`：Browser callback；`internal/assets/recording_processing_progress.go`：安全型別；`internal/postgres/recording_processing_progress.go`：fenced persistence；`internal/migrations/sql/035_recording_processing_progress.sql`：additive migration；`docs/openapi.yaml`：契約 |
| hhc-web-api | `internal/assetclient/recording_{package,source,retention}.go`、`internal/recordings/{model,lifecycle,source,service}.go`、`internal/httpapi/recording_{handlers,packages,sources}.go`：投影／管理 response；`openapi.yaml`：契約 |
| frontend-platform | `packages/hhc-web-client/{openapi/hhc-web-api.yaml,src/generated.ts,src/index.ts,src/client.test.ts,package.json}`：schema／SDK／版本 |
| hhc-cli | `internal/{api/packages.go,recordings/upload.go,recordings/prepare.go,media/prepare.go,cli/progress.go}`、既有 test 與 bundled skill：進度及操作指引 |
| admin-fe | `src/lib/cms-api.ts`、`src/pages/recordings/{RecordingDetailPage.tsx,editor-labels.ts,RecordingPages.test.tsx}`：既有資訊區與五秒輪詢 |
| api-gateway | 核對 routes／管理權限／service actor 與 contract bundling；若 routing contract 未變不做無關修改 |

新增 migration 編號以執行前 latest main 為準：若 035 已被其他變更使用，改用下一個空號，不能覆寫既有 migration。

## Task 1：建立可信基準與量測入口（Asset／CLI）

**Files:** Create `asset-api/internal/recordingvalidation/package_benchmark_test.go`、`asset-api/docs/superpowers/reports/2026-10-06-recording-pipeline-baseline.md`；Modify `asset-api/internal/recordingvalidation/package_test.go` 的 fake storage；Test 既有 package／media tests。

**Interfaces:** benchmark 使用既有 `PackageObjects`，只計數 HEAD／COPY／Open／inventory PUT 與 bytes；正式 API 不暴露 keys。輸出 elapsed／CPU／RSS／scratch／operation count 與 source／preset／tool hash，供 Task 3／7 同條件比較。

- [ ] 寫 `TestPackageObjectsConcurrentFixture`：對 fake store 並行 copy／read；斷言正確 bytes、count 及 `go test -race` 無 race。先用未修 fake 重現失敗，再用 mutex／clone 修 fake，不改 storage 生產語意。
- [ ] 執行 `go test -race ./internal/recordingvalidation -run TestPackageObjectsConcurrentFixture -count=1`，確認 red→green；提交 `test: make recording fixtures concurrency safe`。
- [ ] 在 baseline commit 記 short fixture 與約兩至三小時三畫質 fixture 至少三次配對所需基線；長片以本機產物測試，不上傳／發布正式影片。記本機／NAS與來源 hash，不能用不同來源比較。
- [ ] benchmark 使用與 `Dockerfile.recording` 相同媒體 build、4 CPU／8 GiB 限制；另測雙執行、128 MiB 合法最大物件與 20–50 GB 原始檔路徑的容量情境。正式 image 不增 benchmark runtime dependency；測試 harness 不包進正式 image。
- [ ] 報告分列各 phase、全包 GET 次數／bytes、實際容器 runtime 與 idle cron；舊正式 execution 約 77 分鐘只作歷史訊號，不當成同條件性能基準。無真實長片／計費資料時保留驗收未完成，不捏造數字。

## Task 2：Asset 安全進度型別與 fenced 持久化

**Files:** Create `internal/assets/recording_processing_progress.go`、`internal/postgres/recording_processing_progress.go`、`internal/migrations/sql/035_recording_processing_progress.sql`；Modify `recording_package_upload.go`、`recording_source_upload.go`、`recording_retention.go`、postgres package／source job／scan；Test 新 progress test 與既有 DB integration。

**Interfaces:** `assets.RecordingProcessingProgress` 具有 `Attempt int`、`Phase string`、`Rendition string`、optional `*int64` counts、`AttemptStartedAt/PhaseStartedAt/LastProgressAt/HeartbeatAt time.Time`；wire 名稱完全依 spec。提供 `ValidateRecordingProcessingProgress(value RecordingProcessingProgress) error`。

Store 方法：`UpdatePackageProcessingProgress(ctx context.Context, id, claimID string, value assets.RecordingProcessingProgress) error` 與 `UpdateSourceProcessingProgress(ctx context.Context, id, claimID string, value assets.RecordingProcessingProgress) error`。沿既有 slot→package/source lock 順序，檢查 live claim／lease／state，不建立新的所有權機制。新增 nullable `processing_progress jsonb` 至 package／source 表；claim 初始化／reset，transition 保留摘要但不阻止舊 worker 寫 existing state。claim DTO須提供SQL既有attempt計數與server start timestamp，不能用CLI時間初始化。

- [ ] 寫 `TestProcessingProgressValidation`：非法 phase／rendition、負 count、done>total、時間倒置拒絕；缺 counts 合法，queued attempt=0 合法，claimed attempt 1–3；零完成數不因 omitempty 消失。
- [ ] 寫 `TestRecordingProcessingProgressFenced`：expired／舊 claim 不能寫；正常更新不延長 attempt 開始時間；heartbeat 只更新 heartbeatAt，不更新 lastProgressAt；retry reset、舊 worker 無 summary、source claim 都有斷言。

```go
// Assertions inside TestRecordingProcessingProgressFenced, after a new claim:
if err := store.UpdatePackageProcessingProgress(ctx, id, oldClaim, snapshot); !errors.Is(err, assets.ErrConflict) { t.Fatalf("stale progress accepted: %v", err) }
if !after.LastProgressAt.Equal(before.LastProgressAt) { t.Fatal("heartbeat fabricated progress") }
```
- [ ] 先跑 targeted tests red；實作型別／nullable migration／store／scans；進度由協調流程彙整，heartbeat snapshot 加階段／terminal flush，不能逐片段 DB write。worker report callback只更新collector，DB寫入仍受flush gate，不能把兩-worker callback直接寫store。
- [ ] 跑 `go test -race ./internal/assets ./internal/postgres -count=1 -p=1`，使用 CI 同款 disposable PostgreSQL／本機 TEST_DATABASE_URL 與 TEST_POSTGRES_DSN，不使用正式 DB；跑 migration policy scripts 與 `go vet ./...`。
- [ ] 更新 `docs/data-governance.yaml` 對 nullable progress 的欄位分類／既有 row 清理責任及 export tests，無來源 metadata／URL／claim ID；提交 `feat: persist fenced recording processing progress`。

## Task 3：共享單次下載與最多兩個片段 pipeline

**Files:** Modify `internal/recordingvalidation/{package.go,package_probe.go,package_worker.go,package_test.go,package_probe_test.go,package_worker_test.go}`、`internal/recordingprocessing/{worker.go,encode_test.go}`；Create `internal/recordingvalidation/package_pipeline.go` 與 `package_pipeline_test.go`。

**Interfaces:** `PackageMediaProbe` 增加 optional `OnProgress func(assets.RecordingProcessingProgress)`。`FreezeRecordingPackage(ctx context.Context, p assets.RecordingPackage, attempt string, objects PackageObjects, probe PackageMediaProbe) (string,error)` 取代最後一個 whole-package callback；更新所有 grep 到的 caller／test。`PackageMediaProbe.Validate(ctx,inv,prefix) error` 保留既有 final-only callers 的完整驗證能力，與 Freeze 共用 fragment probe／timeline helpers，不能留下另一套較弱 validator。

新增最小 helper：`probePackageFragment(ctx context.Context, path string, r assets.RecordingRendition) (segmentProbe,error)`；`freezePackageObject(ctx context.Context, p assets.RecordingPackage, attempt string, object assets.RecordingPackageObject, objects PackageObjects, destination io.Writer) error` 負責 copy／HEAD／final GET 的 SHA／exact size，只有成功才回報 verified。不新增一個實作的 interface 或通用 pipeline framework。

- [ ] 寫 `TestPackagePipelineSingleReadAndBound`：每 fragment final GET=1、每 rendition init final GET=1、最多兩個 in-flight worker、同 path copy=1；完成順序刻意亂序，timeline 輸出仍依 index。
- [ ] 寫 `TestPackagePipelineRejectsInvalidMedia` table：SHA／size／gap／codec／actual IDR／A/V／跨畫質／尾段偏移拒絕；hash 失敗不 probe、任何失敗不 inventory PUT／ready；保留既有 late staging mutation case。
- [ ] 寫 `TestPackagePipelineCancellationAndBudget`：GET 失敗、lease cancellation、工具 timeout、disk 不足後所有 worker 已 join、reader／file 關閉、scratch 清空；invalid-media root error 不被 cancel 蓋掉；max size／雙 worker reservation 不超 1 GiB＋安全餘量。

```go
// Assertions inside TestPackagePipelineSingleReadAndBound:
if maxInFlight > 2 { t.Fatalf("unbounded workers: %d", maxInFlight) }
if finalFragmentGETs != segmentCount || finalInitGETs != renditionCount { t.Fatal("duplicate final reads") }
if !slices.IsSorted(completedTimelineIndexes) { t.Fatal("completion order became timeline order") }
```
- [ ] 先跑 `go test -race ./internal/recordingvalidation ./internal/recordingprocessing -count=1` red；以一個固定兩-worker budget、當前 rendition init cache及按 index 彙整實作。init＋fragment 串接時只對 fragment stream hash，不混入 init bytes。
- [ ] shared Freeze 將 copy/hash/probe 放同一 worker順序，控制檔先檢查，最後仍檢所有時間軸／master codecs／inventory。Package／Source callback 初始化 attempt，再沿 Task 2 flush；Browser encode 不變。
- [ ] 全部工具保持 protocol／output／time bound；錯誤 cancel→join→close→cleanup→finish。依 grep 更新所有 Freeze callers，不靠 test-only callback 放寬生產驗證。
- [ ] 跑真實短媒體三畫質／2、24、30 fps及短音訊尾段；跑 Task 1 長片1／2 worker比較與雙 Job峰值；保留兩次 ffprobe。量測不通過就維持原 worker數，不增加資源。
- [ ] 跑 Asset CI 等同完整 race／vet、image／security／OpenAPI／governance；提交 `perf: validate HLS with bounded single-read workers`。

## Task 4：Asset／CMS 管理投影與契約

**Files:** Asset `internal/{httpapi/recording_package.go,httpapi/recording_source.go,httpapi/recording_retention.go,assets/recording_retention.go,postgres/recording_retention.go}`、`docs/openapi.yaml`與對應 tests；CMS `internal/{assetclient/recording_package.go,assetclient/recording_source.go,assetclient/recording_retention.go,recordings/model.go,recordings/lifecycle.go,recordings/source.go,recordings/service.go,httpapi/recording_handlers.go}`、`openapi.yaml`／`openapi_test.go`。

**Interfaces:** package／source status additive `processingProgress?`。為非 uploader 的 source 進度，擴充既有 private `/priv/recordings/lifecycle` request optional `sourceItems: [{recordingId,sourceId}]`、response optional `sourceItems: [{recordingId,sourceId,state,processingProgress?}]`；package `items`既有語意不變。CMS 只能用自身錄影 repository 的綁定發請求，Asset 必須驗 owner service＋recording/source association，回安全摘要而非完整 source／upload capabilities。

CMS `Recording.ProcessingProgress *assetclient.RecordingProcessingProgress` 使用 `json:"-"`，只在既有管理 handlers response 增加 `processingProgress?`；Public／Member projection 不返回進度。CMS Get 可利用本次 lifecycle 回應暫態裝配摘要，不重複呼叫 uploader status，也不為此持久化另一份進度 DB。sourceItems 由 CMS 已有 SourceID 綁定取得；source old producer 缺欄位回退未知。

- [ ] 寫 Asset `TestRecordingLifecycleSourceProgressOwnerBound`：binding 不符／跨 recording／非 CMS caller 拒絕；合法 CMS 不同 uploader 可讀摘要，結果沒有 actor/key/URL/confirmedBlocks；既有 package items request response不變。
- [ ] 寫 CMS `TestAdminProcessingProgressAcrossUploader`：human／SA／其他管理員具read能看摘要，但 sign／complete 不鬆綁；舊 producer缺欄位仍 Get成功；public/member無summary；ready gate／publication完全沿既有結果。
- [ ] targeted tests red→型別／handler／snapshot validation／暫態裝配green；對 phase／counts／timestamp 做輸入契約 validation，非法summary不偽造ready。
- [ ] canonical OpenAPI 同 PR 增 schema及optional fields，確認 response size limits；跑 Asset contract tests與CMS `go test -race ./... -count=1 -p=1`／`go vet ./...`／Redocly lint／migration／release policy tests。
- [ ] Gateway唯讀核對既有 GET與human/service actor權限；若 bundling要求更新 artifact，只做精確contract同步並跑 existing recording routing tests，不新增public path或弱化鑑權。提交各 owner的獨立contract PR。

## Task 5：SDK 與 CLI 可觀測進度

**Files:** shared client schema／generated／index／client tests及package版本；CLI `internal/api/packages.go`、`internal/recordings/{upload.go,prepare.go,upload_test.go,transfer_test.go}`、`internal/media/prepare.go`、`internal/cli/{progress.go,progress_test.go}`、`skills/hhc/SKILL.md`及`internal/cli/skill_test.go`。

**Interfaces:** TypeScript從canonical OpenAPI產生 `RecordingProcessingProgress`，不手寫另一套 schema。CLI `api.ProcessingProgress` 與 Task 2 wire一致；`recordings.TransferProgress`增加 optional summary，`progressDisplay.Transfer(value recordings.TransferProgress)`保持既有入口；本機檢查沿media/Journal既有callback擴充phase，不改 encoding 100%語意。

- [ ] SDK `TestAdminRecordingProcessingProgressContract` assertions：缺 optional合法、0 completed保留、新 enum正確、member response無進度；codegen後 `corepack pnpm --filter @hallelujahhomechurch/hhc-web-client check:generated`乾淨。
- [ ] CLI `TestValidationProgressDisplay`：窄 terminal單行、attempt／elapsed／lastprogress、五分鐘提醒、heartbeat新但lastprogress舊仍提醒；未知無0%、100%仍等待atomicready；JSON／redirect不含ANSI或能力URL，stdout machine結果不混人類文字。
- [ ] CLI `TestResumeReadyWithCoverFailure`：Ctrl+C僅停止等待；ready後cover／publish失敗再resume不重轉／上傳，journal成功與publication仍分開。非法或舊summary不能破壞原status處理。

```go
// Assertions inside TestResumeReadyWithCoverFailure after resume:
if encodeCalls != 0 || uploadCalls != 0 { t.Fatal("ready media was prepared or transferred again") }
if result.RequestedActionSatisfied { t.Fatal("failed requested cover was reported successful") }
```
- [ ] tests red→最小 typed parsing／render／callback green；poll backoff仍沿既有2→4→8→16→30秒，不新增daemon／SSE。機器進度沿既有事件輸出通道，最終schemaVersion=1結果只additive擴充。
- [ ] shared分別跑 `corepack pnpm test`、`corepack pnpm lint`、`corepack pnpm build`、`corepack pnpm check:packages`、`corepack pnpm pack:packages`、`corepack pnpm test:consumers`；CLI跑 `go test -race ./... -count=1`、`go vet ./...`；以 `task_build_dir=$(mktemp -d)` 建暫存目錄，再跑 `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o "$task_build_dir/hhc.exe" ./cmd/hhc` 及 `GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -o "$task_build_dir/hhc" ./cmd/hhc`，之後由actual bundled media/native CI驗證cgo credential與工具。
- [ ] 先讀 writing-skills 技能，再更新bundled skill的wait／resume／phase提醒並跑`internal/cli/skill_test.go`；不提供server cancel命令，journalSchema與簽章manifest不變。各repo獨立提交／PR。

## Task 6：Admin 同版型進度

**Files:** `src/lib/cms-api.ts`／`mock-cms-api.ts`與tests、`src/pages/recordings/{RecordingDetailPage.tsx,editor-labels.ts,RecordingPages.test.tsx}`；Create `processing-progress.ts`與test供format／elapsed判斷；package.json／pnpm-lock.yaml只更新已發布SDK精確版本。

**Interfaces:** `formatRecordingProcessingProgress(progress: RecordingProcessingProgress | undefined, state: string, now: number, locale: keyof typeof editorLabels): {label:string;detail:string;waiting:boolean}` 使用Task5 schema；沿現有locales labels與UI元件，不能硬編只中文的新畫面。

- [ ] 寫 `RecordingPages` tests：其他管理員看SA上傳進度不呼叫不可讀 uploader status；old worker未知、stage/attempt變化、五分鐘提醒、ready完成；五秒輪詢不重疊、離頁取消，network error保留已知進度且不判job失敗。
- [ ] tests red→在既有影片資訊／檔案狀態區顯示phase、數量、耗時與最後進展；使用existing refresh/get response，不增新route；可用標題／儲存／刪除／下架／封面與publish行為不改。
- [ ] 跑 `corepack pnpm test:run`、`corepack pnpm lint`、`corepack pnpm build`與release policy scripts；local mock檢桌面／窄螢幕／未知和失敗狀態，browser demo依browser skill，不以模擬取代實機。
- [ ] 提交 `feat: show recording processing progress in admin`，保持原layout與共用元件。

## Task 7：條件式上傳優化與完整成本報告

**Files:** CLI `internal/recordings/upload.go`／`transfer_test.go`（只有測量通過才改）、Task1 baseline報告、新 `docs/superpowers/reports/2026-10-06-recording-pipeline-acceptance.md`與`docs/member-video-operations.md`。

**Interfaces:** 比較同fixture／網路／tool的3與6 PUT；不公開新參數／自動調参framework。算單成功錄影所有attempt、failed copies、清理、素材、原始Blob與idle baseline的用量，成本以同區域當時官方價列免費額度前／後。

- [ ] 用既有upload fixture測3／6並行的實際throughput／maxinflight／retry；補 `TestUploadConcurrencyLimitAndResume`，assert只有缺物件被重傳、hashcheck未減、canceljoin完成。只有總上傳更快且成本不超105%才採6；否則保留3並記實測理由。
- [ ] 本機來源指紋／檢查計時必須已Task5顯示；若需再並行本機validator，先提出同規格內的明確補充步驟，不能默默改producer算法或省略hash。
- [ ] 重跑Task1配對benchmark：median後端驗證與端到端時間下降；雙Job無OOM／額外retry、scratch合規；不由三個樣本宣稱可靠p95，提供raw安全metrics與範圍。
- [ ] 整理ready→customcover→publish、預覽與播放可用延遲，不能以只到ready的數字宣稱整體完成；測錯誤cover不破壞已ready影片。
- [ ] 記container runtime與allocated-resource seconds，Execution start/end不能直接当bill；R2操作數／bytes需counter或provider evidence；正式帳單未知獨立列未驗收。cron成本量測但不改排程／身份。
- [ ] report self-review：超105%、端到端未改善或可靠性回歸時停止效能發布；候選throughput不等於productionSLA。提交 `docs: record recording pipeline performance and cost evidence`。

## Task 8：PR／CI／相容發布／正式驗收

**Files:** 各repo既有`.github/workflows/{ci,release}.yml`、Asset`docs/member-video-operations.md`與Task7報告；只在必要時修改精確contract artifact／manifest，不改部署資源。

- [ ] Native執行建議：此session主代理依task sequential實作，每個owner完成先完整CI，所有diff最後請獨立顧問唯讀review；不自行使用更高model或再spawn實作agent，除非使用者選定／授權。
- [ ] 用repo精確檔案清单commit／push task branch，`gh pr create`後逐一attach artifact；required checks green，不bypass security掃描或已有新CVE。先owner contract/progress PR，再consumer PR，Benchmark report列本機／雲端未知範圍。
- [ ] 取得本輪merge／release授權後，Asset→CMS→shared→CLI／Admin順序merge，deployable owner用既有merged-main CI/CD；source storage provision workflow不執行。
- [ ] Asset `Production Release`：merge程式會觸發；manual只可 `gh workflow run release.yml --ref main -f confirmation=deploy-asset-api-production`且已批准，其他approval／failureinputs保留false。CMS confirmation `deploy-hhc-web-api-production`；Admin `deploy-admin-fe-production`，同樣只能merged main。
- [ ] shared先核對root/package版本與已發行tags，再只在已merged commit打符合root版本的v tag，執行既有publish。CLI核對最新version／release後選下一個patch（目前1.1.1，不預先保證tag），merged commit打穩定v tag，既有signed Release出Windows/macOS native bundles；不覆寫release asset。
- [ ] live smoke確認latestReady revision／immutable API與recording image digest、4CPU8GiB／cron／slots未變、health／ready、管理schema與未授權媒體仍拒絕。authorized真實影片由使用者測CLI／播放，不自行publishproductiontest。
- [ ] 不停止健康在途Job；additive migration與oldworker／new API並存測試通過。記API與Job各自healthy digest，roll back只走repo既有路徑；schema不down，leases／journal不清除。
- [ ] 正式用量／provider帳單後補對帳；超105%或可靠性回歸就停止推進、恢復後續execution healthy image並重新確認，保留健康在途任務。
- [ ] 本機tests、PR／CI、merge、deploy／smoke、Windows NVIDIA／真人影片、provider成本驗收分列證據；未取得真實acceptance不稱全部完成。release成功且無後續工作才移除本任務cleanworktree；不清其他人的worktree或dirtymain。

## 執行決策與本輪停止點

建議 Native，由主代理在本session依此計劃實作；共享驗證／claim／契約依賴緊密， sequential小diff較易控制，不需要每項task另起agent。最後獨立review需另行明確授權。使用者若選subagent-driven，依對應skill執行並保留相同gate。

本輪只建立與自檢此計劃，不寫產品程式、不跑正式測試上傳或部署。使用者審閱本計劃並選定執行方式後，才進入Task1。
