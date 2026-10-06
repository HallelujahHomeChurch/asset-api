# 影音上傳流程效能與成本優化規格

日期：2026-10-06。狀態：使用者已確認設計並要求開始優化；實作計劃已建立，待書面審閱與選定執行方式。尚未修改產品程式或部署。

## 1. 目標與邊界

使用者要求縮短錄影從開始處理到可觀看的整體時間，且不希望費用大幅增加。此規格整合兩位獨立唯讀顧問的效能／安全與成本／發布審查。

- 維持每個錄影 Job 4 vCPU／8 GiB、全域最多兩個長處理 slot。
- 保留所有影音、時間軸、雜湊、授權、不可變副本與 atomic ready 檢查；不以 CLI 本機檢查取代後端驗證。
- 保留三畫質、既有 bitrate、30 秒 HLS 分段、原始檔唯讀與操作 ID／續傳語意。
- 不新增 GPU、容器、佇列、長駐服務或付費媒體平台。
- 成本以每支成功影片的完整用量不增加為目標；同輸入估算成本增加超過 5% 時，停止推進效能版本的正式發布，重新取得使用者確認。
- 不改一般 Blob 上傳／掃毒管線，不啟用尚未批准的追溯或保留政策功能。

替代方案：加大資源容易增加費用；抽樣／跳過完整解碼會削弱既有驗證；全面改為邊轉邊上傳／驗證會擴大狀態與重試複雜度。本輪選擇現有服務內的有限並行與減少重複 I/O。

## 2. 現況證據與未知事項

本輪檢視的 Asset 基準為 `origin/main` 的 `97d82803c2aabe735679ec7491314b64aa86b1e8`；CLI 基準為 v1.1.1。實作前仍須重新確認各 owner repo 最新 main 與契約。

| 現有環節 | 確認到的行為 |
| --- | --- |
| CLI | 單次 FFmpeg 共用來源解碼，同時產出 480p／720p／1080p；完成本機檢查後，最多三個物件並行 PUT |
| 上傳完成 | 全包完成後才呼叫 complete；接受完成只代表進入後端處理，不代表 ready |
| Freeze | 每物件串行 HEAD staging、以 ETag 條件複製至 attempt final、HEAD final、GET 檢查大小與 SHA-256 |
| Media probe | 所有畫質／片段串行；每段再 GET init 與 fragment，執行兩次 ffprobe 與一次完整 FFmpeg 解碼 |
| Browser | 原始檔暫存 Blob；Job 依來源尺寸產生一至三個畫質，目前逐畫質轉檔，輸出 spool 有背壓 |
| 共享流程 | CLI package 與 browser source 都呼叫 `FreezeRecordingPackage`；優化須在此共享邊界完成 |
| 排程 | 正式設定為每分鐘啟動，4 CPU／8 GiB；兩個 DB slot 限制長處理，不限制短執行總數 |

主要程式證據：`internal/recordingvalidation/{package.go,package_probe.go,package_worker.go}`、`internal/recordingprocessing/{worker.go,encode.go,spool.go}`、`internal/postgres/recording_package*.go`；CLI `internal/{media,recordings}`。

目前缺少真實長片的分階段耗時、片段進展、計費秒數、重試分布與雙 slot 資源峰值。曾觀察到長執行約 40 分鐘仍 Running，但不能據此確定瓶頸或證明未卡住。此紀錄不是目前 Job 狀態，也不能精確綁定使用者套件。Azure execution start/end 含排程與啟動等待，不直接視為實際計費容器秒數。

## 3. 預期流程與範圍

```text
CLI：來源指紋 → 三畫質同時轉檔 → 本機驗證 → 有界並行上傳
Browser：來源指紋／分塊直傳 Blob → 原始檔 finalization → 現有逐畫質轉檔
                                                ↓
                     complete durable receipt／既有 Job claim
                                                ↓
                    inventory／控制檔檢查與 attempt final 副本
                                                ↓
                最多兩個片段：複製 → 單次下載與 hash → probe／decode
                                                ↓
                 排序後完整時間軸／跨畫質檢查 → inventory → atomic ready
                                                ↓
                 現有封面／時間軸衍生流程 → 如有請求則發布 → 可觀看
```

最後一段並非新設的發布 gate：ready、衍生素材與 publication 是不同狀態，沿用既有行為。CLI 的 `--cover` 必須成功完成選圖才執行所請求的發布；未要求自訂封面時仍沿用現有自動素材與 fallback，不強迫新增等待。量測必須分別記錄 ready、素材完成、publication 與實際播放可用時間。

本輪包含：共享後端驗證、進度契約、CLI／Admin 顯示、量測與必要測試。CLI 本機檢查與上傳是量測／條件式調整項；browser 轉檔維持現狀，但受益於共享驗證優化。

不包含：跨 attempt 片段驗證 checkpoint、取消原本持久化工作的新 API、player 改版、同時轉 browser 三畫質、合併 ffprobe、邊轉邊上傳、API kick／cron 改頻率、權限或 DNS 變更。

## 4. 有界驗證設計

### 4.1 同一次讀取保留完整性

- 先驗證 inventory、declared digest、物件種類與大小上限；各 attempt 使用唯一 final prefix。
- 控制檔與當前畫質的 init 先從 staging 以現有 ETag 條件複製，HEAD final，GET final 驗證大小與 SHA-256。播放清單檢查不省略。
- 每個 fragment 工作依序完成 conditional copy、HEAD final，再以同一個 final GET 串流計算 fragment SHA-256 並寫入 worker 私有媒體 scratch。
- scratch 若包含 init＋fragment，fragment hash 只覆蓋 fragment bytes；init 使用自己已驗證的 bytes，不把串接檔當成任一物件的 hash。
- 長度及 hash 通過後，才讓 ffprobe／FFmpeg 讀該片段。其他片段已通過不能抵銷任一失敗。
- init 只在同一 attempt／同一 rendition 內重用，採有界磁碟快取；不跨重試或套件使用，也不信任 browser spool 已計算的 hash 代替 final 驗證。
- 同 attempt／object path 只安排一次 copy／write；不允許重複 worker 覆寫 final。retry 維持新 claim prefix。
- 所有物件、playlists、媒體與跨畫質檢查通過，才寫 package inventory 並依原有 fenced transaction commit ready。

### 4.2 單一並行預算

- 每個 Job 最多兩個 fragment worker，下載、hash、probe、decode 共用此限制；不可再開獨立 integrity pool 或每畫質兩個 pool。
- 一次處理一個 rendition；兩片段可亂序完成，主協調流程按 index 組成 timeline，再做 continuity／codecs／duration 及跨 rendition 比對。
- worker 只產出不可變結果；timeline、codec 彙整及累計進度由協調流程持有，避免共享 map／slice race。
- 每個 worker 私有檔案／目錄；已完成片段立即清除，只保留檢查需要的 metadata。不落地完整來源或完整 HLS 套件。
- 依 inventory 最大 init、兩個在途 fragment 與串接成本計算整個 Job 的 scratch reservation；有效媒體 scratch 硬上限 1 GiB，另保留至少 128 MiB filesystem 安全餘量。下載以實際宣告大小＋1 限讀；磁碟不足時不超配或刪除其他工作檔案。
- 靜態預留不能取代 live 容量檢查；browser 同 execution 的 spool、既有 scratch 與其他衍生處理必須計入容量與清理檢查。
- `-threads 2` 不視為 FFmpeg process 總 thread 上限。量測兩 worker 的實際 CPU throttling、thread、RSS 與 scratch 峰值；必要時保持單 worker，不能因此加資源或降低檢查。
- 兩次 ffprobe 保留：metadata／timeline 與首 packet actual IDR 分別驗證。不得以 keyframe flag 代替 actual IDR，或輸出整段 packet hexdump突破既有輸出上限。

### 4.3 錯誤、租約與清理

任一不可恢復錯誤、context 取消、工具 timeout 或 lease loss：取消所有工作、等待全部 goroutine／子程序退出、關閉 reader／檔案並清理 owned scratch，再返回與更新持久化狀態。保留原始非取消錯誤與 `errors.Is` 分類，不讓 sibling 的 `context.Canceled` 覆蓋 invalid-media 或 provider 失敗。

沿用最多三次 attempt、5 分鐘 retry、330 分鐘單 claim deadline、30 秒 heartbeat 與 DB slot fencing；不增加額外失敗重試。progress 不是續驗憑證。failed final／late writes 仍走既有 grace 與重掃；中止工作不發布，來源原始檔與使用者指定輸出不刪除。

## 5. 進度契約與顯示

Asset 是工作／進度權威，CMS 是錄影權限與 publication 權威。既有狀態碼不改；新增 optional `processingProgress` 摘要，不改 immutable inventory／digest 或 journal schema 的完成定義。

摘要欄位語意：

- `attempt`：此 processing owner 的 attempt 次數；source／package 以其既有狀態區分，不暴露 claim ID。
- `phase`：`queued`、`source_finalization`、`encoding`、`package_validation`、`package_finalization`。
- `rendition`：只允許既有畫質名稱；不含來源檔名、路徑、object key 或 URL。
- `objectsVerified`／`objectsTotal`、`segmentsVerified`／`segmentsTotal`、`bytesVerified`／`bytesTotal`：僅已完成檢查的數量；未能確定 total 的欄位省略，不造假零值。
- `attemptStartedAt`、`phaseStartedAt`、`lastProgressAt`、`heartbeatAt`：server UTC 時間；最後實際進展只在完成工作單位或階段時更新，不由 heartbeat 推進。

ready／failed／expired 仍由既有 state 表示；100% 片段檢查不等於 ready。source encoding 階段可先只顯示階段及耗時，不憑工具存活捏造百分比。

持久化採 additive nullable migration，與目前 claim fence 同交易／條件更新。累計數由協調流程更新記憶體，沿既有 heartbeat 最多每 30 秒寫一次，階段與 terminal transition 額外 flush；不逐 frame／packet／片段寫 DB。遲到的舊 claim 不能寫新 attempt 的進度，且觀測性不得鬆綁既有 lease 或 ready 判定。

Asset package/source status 與 owner-bound lifecycle 投影提供摘要；CMS 對 read 授權的錄影 metadata 返回安全摘要。非 uploader 管理員包含 SA 上傳情境，仍不得透過進度取得 sign／complete／confirmed upload inventory 能力。canonical OpenAPI、CMS client、shared client 與契約測試同步更新。

CLI 沿用既有等待／退避與單行 progress bar；新增本機檢查階段，顯示後端 phase、畫質、數量、attempt 耗時與最後進展時間。非互動／JSON 模式的摘要及事件須沿既有輸出規則，不混入彩色或機密，且不改命令成功語意。Admin 沿用現有狀態區與五秒輪詢，不改其他 layout，不新增 SSE／WebSocket。

超過五分鐘未見實際進展時顯示「暫未收到新進度，工作可能仍在處理」及 last progress／heartbeat；不是失敗或自動重跑指令。舊 worker／舊 API 缺摘要時顯示「進度未知」與已知 state，不當作 0%、stalled 或改動有效租約。Ctrl+C 仍只取消 CLI 等待，未提供新的 server cancel 行為。

## 6. CLI／Browser 的條件式優化

CLI 基線保留三 PUT；在同機器、同來源／網路下分別量測三與六並行。只有整體 upload 更快、重試／資源／成本門檻通過，才在後續實作中採用較高固定並行；不為未證實收益新增自動調參框架或使用者參數。續傳仍只上傳缺少物件，SHA／size／stable-file 檢查不省略。

本機檢查先計時，不能為追求速度刪除指紋、媒體檢查或改寫來源。若本機段驗證占主要耗時，再在同一規格邊界提出有界並行的具體實作計劃；未有 benchmark 前不擴大 producer 行為。

Browser 轉檔在 4 CPU 且無 GPU 情境下，不預設同步三畫質更快／便宜；本輪不改 source reader、range pinning、spool 背壓、Blob 原始檔暫存與其清理。來源大小、package 大小與保留期限沿現有實際契約，不在這次變更上限。

## 7. 成本與性能量測

分別量測本機準備、上傳、排隊、每次 freeze／validation、ready、封面、時間軸預覽、publication 與播放可用時間。從準備到可觀看的牆鐘時間不把平行工作重複相加；requested draft 不列入 publication 延遲。記 retry 次數與 queue wait，避免把 GPU／NAS／網路差異誤歸因到後端。

每成功錄影成本包含所有 attempts、failed copy、相關清理／衍生工作、R2 操作及傳輸量與 Blob 原始檔暫存；另列不能精確歸屬的 idle／no-claim 執行基線。排程基線以每週相同上傳數／工作量的用量比較，不能忽略或任意攤成零。

- Azure Consumption Job：記實際容器 active 用量、配置 vCPU-seconds／GiB-seconds與資源規格；不以 CPU 利用率或 execution elapsed 代替正式計費 meter。
- R2：記 HEAD／GET／COPY／PUT 次數與 bytes，並保持成功後穩態套件／封面／預覽大小相同；減少讀取不等於免除操作費。
- 費率使用量測當時、同區域／幣別的官方價格，並另外列免費額度前與後的估算，避免共享免費額度掩蓋回歸。
- 使用 Azure Cost Management／provider usage 對帳；費率估算、provider 帳單與本機 benchmark 分列證據，不宣稱未取得的帳單已驗證。

官方依據（查核日期 2026-10-06）：[Azure Container Apps billing](https://learn.microsoft.com/en-us/azure/container-apps/billing)、[R2 pricing](https://developers.cloudflare.com/r2/pricing/)。Jobs 為 active-rate 計費；R2 不收 egress，但有儲存與操作費。本規格不承諾固定金額或固定倍數加速。

## 8. 排程與基礎設施：本輪維持不變

每分鐘排程還負責清理、重試與衍生素材供給。本輪保留原 cron、資源、全域 slot、identity／RBAC、DNS 與 bucket 設定。

若量測確認 no-claim 用量占比高，另行提出「完成 durable commit 後觸發既有 Job＋低頻 cron 補漏」的 reviewed diff。該方案必須定義 cleanup backlog／retry／衍生工作的供給 SLO、重複／漏觸發與 slot 忙碌處理；ARM 403／timeout 不得撤銷已接受的上傳，也不把 kick 成功當成 ready。身份只允許 exact Job scope 的 start 能力，不授予 Contributor 或任意 template override。此次寫規格不授權此變更。

## 9. 測試與驗收門檻

### 正確性必須全數通過

- 測試 storage fake 與並行 callback 先具 thread safety；race 測試覆蓋亂序完成與取消。
- hash／size 錯誤、缺物件、壞 playlist、IDR 不符、codec 變更、片段缺口、影音／跨畫質偏移仍拒絕；該物件 hash 失敗時不進 probe，整包不能 ready。
- late staging overwrite 不影響 final；同 path 不重複排程；init 不跨 attempt 重用。
- GET 中斷、工具 timeout、disk 不足、lease loss、任一 worker 失敗時，所有子工作退出且 scratch 清除，錯誤分類及重試語意不變。
- 兩個長 Job 同時忙碌、最大合法物件、低 fps／尾段與三畫質真實媒體測試；原始檔 hash 不變。
- stale progress 寫入受 fence 阻擋；old worker／missing field、attempt reset、非 uploader read、human／SA、CLI Ctrl+C／resume、Admin 輪詢錯誤均有測試。
- ready、cover 選取、publication receipt／通知冪等與播放授權不變；封面失敗不摧毀 ready HLS 或強制重傳。

### 性能／成本驗收

基準與候選使用同來源 hash、同畫質／preset、同 FFmpeg build、同機器／資源規格、相近網路，先以單長工作至少三組配對測試，再測兩個長工作並行。長片包含約兩至三小時三畫質，短片／20–50 GB 原始檔情境分別測試；不只用現有單畫質 35 秒 fixture 宣稱完成。PR 前以本機／CI 限制至等同 Job 資源的環境比較演算法與峰值；正式 Azure／R2 的計費與延遲另在 merge 後依既有 CI/CD 發布及已批准的影片驗收記錄，不建立或部署未 merge 的候選環境。

- 必須提供前後各階段與端到端耗時、median／範圍；資料量足夠才報 p95，不由三次樣本宣稱穩定 p95。
- 候選 median validation 時間與端到端可用時間均須比基準短；若只有局部更快、總時間不降，不能宣稱整體優化完成。
- 每支成功影片完整估算成本不得超過 baseline 的 105%；目標不增加，超出需重新確認，不靠共享免費額度掩蓋。
- 正確性測試全通過；不得出現額外 OOM、失敗率或 cleanup backlog 回歸。實測峰值必須在既有資源與 scratch 預算內。
- no-claim baseline 與清理吞吐單獨呈現；不足以取得帳單時，明確保留成本實測未完成，不以估算代替 provider 驗收。

本機、CI、雙平台 compilation、正式部署 smoke、Windows NVIDIA 實機／真實影片與實際播放驗收分開記錄。任何正式測試影片、發布或額外基礎設施變更需按既有批准範圍執行；不建立永久測試環境。

## 10. Repo 所有權、發布與回滾

| Owner | 變更責任 |
| --- | --- |
| asset-api | 共用驗證、進度持久化／私有契約、migration、worker／lifecycle、測試與操作文件 |
| hhc-web-api | 授權 metadata／lifecycle 投影、公開管理契約、契約測試 |
| frontend-platform | canonical 管理 client／型別與相容測試 |
| hhc-cli | 階段與進度顯示、條件式上傳優化、Windows amd64／macOS arm64 發行驗證 |
| admin-fe | 既有資訊區的進度與錯誤呈現，不改頁面佈局 |
| api-gateway | 僅核對既有 route／權限與 contract artifact；無新增路由時不做無關變更 |

所有實作使用最新 origin/main 的獨立分支／工作樹、各 repo PR／CI。發布採 producer-first：Asset additive schema／API／worker → CMS → shared client → CLI／Admin；在實作計劃補足 repo 實際 release 指令與必要 contract artifact 更新，不能預先宣稱不需 Gateway 同步。

新舊 worker 可並存最長既有 claim deadline，migration 不刪舊欄位、不要求舊 worker 產生 progress，不改 existing state。不得為部署停止健康長 Job；新執行才採新 image。API runtime rollback 不等於 Job rollback，操作文件須記錄兩者 last healthy digest 與相容 schema；rollback 不清除有效 leases／ready 資產／使用者 journal。

實作前取得基準，先完成正確性測試、本機性能及用量估算門檻再發效能 PR；不可繞過 CI 或部署未 merge 的 image。正式發布後補 provider 量測及成本對帳；若超出 5% 或出現安全／可靠性回歸，依相容回滾路徑恢復後續新執行的版本並重新確認，不中斷健康在途任務。正式 release／route smoke 與真正影片驗收仍是不同交付狀態。

## 11. 下一步與審閱邊界

本文件記錄兩位顧問的缺口與已選設計，不表示產品已修改、性能已證明或費用已對帳。使用者審閱並確認後，才建立包含 owner repo、migration／OpenAPI、測試、基準量測、發布順序與回滾的實作計劃；實作計劃另需審閱與選定執行方式。
