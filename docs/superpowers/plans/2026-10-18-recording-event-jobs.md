# 影音 Event Job 分組與成本改善實作計劃

> 執行者：核准實作後使用 superpowers:executing-plans 逐項執行。本文只是計劃，沒有授權正式變更、merge 或部署。

**Goal:** 取消每分鐘啟動 4 CPU／8 GiB 影音 Job 的固定空跑，維持完整媒體驗證、全域媒體併發 2 與直播穩定。
**Architecture:** 沿用 Asset 的 PostgreSQL outbox、Azure Storage Queue、Managed Identity 及 Azure Event Job 模式。資料庫是工作權威，佇列只喚醒；直播、重處理、衍生素材分組，每小時輕量維護。
**Tech Stack:** Go、PostgreSQL、Azure Storage Queue、Container Apps Jobs、Bicep、既有 GitHub Actions。
**Spec:** 本對話確認事項：Event Job 分組、每小時清理補漏、成本量測、分階段切換及回滾；processing 候選 2 CPU／4 GiB、preview 候選 1 CPU／2 GiB，降規未通過回復原 4 CPU／8 GiB；先不實作。
**分析基準:** asset-api 本地 origin/main 69547cfc7492ffe60e47a50c1ca28f83c88ed38b；實作前重新核對最新 main、實際部署與其他進行中變更。

## 不變條件

- 全域重媒體處理仍只有既有兩個 DB processing slots，跨所有 Job 共用；不是每組各兩個。
- 保留 hash／大小、完整解碼、編碼、時間線、跨畫質、短尾與公開邊界驗證。
- OBS K3/B1/C1、D1/S10、inventory、digest、冪等、權限及既有 journal 相容性不變。
- 直播持續處理，不改成每段啟動一次容器。不干擾 DVR、暫停、倍速與公開 cutoff。
- 不手動修改原場、不重送 Stop/seal、不重設金鑰、不刪正式錄影；不要求 YouTube 同時運作。
- 不更動媒體品質或 publication 排程，不藉此加入驗證結果快取、轉碼參數或播放器改版。
- 所有修改經隔離 worktree、PR、required CI、核准的 CI/CD 發布；不直接本機部署。

## Review Focus

1. DB commit 成功但發送失敗、發送成功但 outbox ack 失敗：工作不得遺失或重複產物。
2. 長直播與短期消息可見性期限不相容：必須續租，不能直接照搬 derivative 的短工作 timeout。
3. 三組 Event Job 合計啟動風暴：Azure maxExecutions 不是全域同時 execution 上限。
4. worker 準備退出時剛好有新工作，以及失聯 worker：恢復不能依賴每小時補漏。
5. 直播佔一個 slot 時，封面／預覽／收尾不能失去供給；批次工作亦不得無限飢餓。

## 1. 分組與候選配置

| 組別 | 處理範圍 | 觸發 | 初始規格 |
|---|---|---|---|
| live | 連續直播片段驗證、已驗證清單推進 | Event | 4 CPU／8 GiB，沿用已驗收能力 |
| processing | 封存完整驗證、瀏覽器原始影片轉 HLS | Event | **2 CPU／4 GiB 候選**；驗證與轉檔分別驗收，未通過回復 4 CPU／8 GiB |
| preview | 直播／錄影封面、時間軸圖片 | Event | **1 CPU／2 GiB 候選**；封面與時間軸分別驗收，未通過回復 4 CPU／8 GiB |
| maintenance | 到期清理、狀態補漏、逾期 outbox／工作核對 | 每小時 | 0.5 CPU／1 GiB 候選，無 FFmpeg |

分組共用既有服務、工作紀錄、影像處理與 storage client，不拆新微服務。模式參數限定可領取的工作，不複製驗證演算法。媒體 Job minExecutions=0、parallelism=1；scaler polling 先採既有 10 秒基準。每組 maxExecutions 保守配置，實際上限以全域 DB slot 與 dispatch admission 為準。

新增隔離的 recording queues／poison queues，復用現有 queue transport 模式，不與圖片 derivative 的工作消息混用。不新增 Service Bus。

## 實作裁定（2026-10-19）

- 既有持久化工作表就是 outbox，工作與可領取狀態在原交易提交後才會被 dispatcher 看見。三列 lane 記錄只作 admission／generation fence；不另加 producer trigger，避免既有 slot→work 與 work→slot 交易鎖順序反轉。下文「同交易寫入 outbox」以既有工作表實現，不新增逐物件 dirty 列。
- dispatcher 在既有 minReplicas=1 API 每 5 秒迴圈，以每 lane 10 秒 readiness 檢查處理新工作、到期重試與失聯恢復；不是每小時 cron 供給。停用／舊版 API 不累積第二份 outbox，啟用後掃描既有 eligible 工作即可復原。
- 實作事件為 type/version/lane/generation/token，不含任意工作參數。media claim 表保持單件工作權威，lane token 只控制喚醒。
- 發布失敗必須協調恢復 API 與 Job topology；先停觸發、恢復舊 API、等待在途 execution 安全收尾，才恢復舊 image/env/trigger。五分鐘內未排空則停止自動恢復並明列人工接續，不強殺。

## 2. Durable event 與喚醒協定

- API 或 worker 建立可處理工作時，在同一交易寫入工作與 outbox。入口涵蓋 confirm 可驗證批次、seal／完整驗證、source complete、封面／preview 建立、五分鐘封面到期、下一階段工作與重試。
- 不對 sign、每次 status GET 或每個 PUT 建立消息；只對可處理狀態變化發出喚醒。
- 內部事件最小欄位：type、schemaVersion、eventId、lane、workKey、generation。只含內部識別資訊，不含存取憑證、signed URL、標題或可任意指定執行程式的資料。
- 同一 workKey 合併喚醒。新 generation 不能被舊 worker 的 ack 清掉；DB 保留 dirty／due 狀態。
- Dispatcher 沿用現有 Asset API 背景 outbox 模式，跨 replicas 用 DB lease；限制每次讀取量、有界 backoff＋jitter、避免 idle busy-loop。實作前確認 API 至少一個可執行背景工作的 replica；若不成立，不能依賴它提供及時恢復。
- 可發送的有效喚醒受全域 admission 控制，扣除已 dispatch／running 的容量；同一有效直播只保留一個 live owner。容量不足時工作留 DB，不把大量可見消息丟進 Queue 等空轉。
- admission 使用獨立的有期限 dispatch 狀態，不提前佔住媒體 slot 造成 claim 死鎖。最終媒體 slot 仍為權威。
- Queue 消息至少一次投遞；worker 領取後續租可見性，驗證 generation，再用原有 fenced claim 處理。重複或過時事件安全 ack，不重做已完成工作。
- 長工作同時維護 Queue visibility、dispatch owner heartbeat 與媒體 claim lease；任一失效停止新增 claim。既有驗證提交 fence 防止失聯 worker 寫入。
- worker 完成或退出前，交易式核對後續工作／generation，持久化接續需求才 ack。不得先 ack 再以記憶體排下一項工作。
- Queue visibility 到期、claim lease 失效、正常 dispatcher 到期重試負責快速恢復；每小時補漏只是最後保障。ARM／Queue 故障不撤銷既有上傳 receipt。
- 毒消息隔離並告警；不能只丟 poison 就忘掉 DB 工作，也不能對永久錯誤無限自動重送。

## 3. 直播與其他工作的公平性

- live owner 沿用跨分鐘連續處理與原退出邊界；沒有有效直播時退出，不把 cancelled／過期 session 當成永久待機理由。
- 直播正常情況保留處理機會，另一 slot 供 processing／preview；保留有效工作的非搶占 lease，不強殺轉檔。
- 事件供給不可再仰賴「每分鐘另一個 execution」；封面到期、preview 增量、seal、final、lease 回收都有獨立 durable 喚醒來源。
- 使用既有工作優先序，加可觀測的 oldest eligible wait；對 preview 飢餓必須有明確測試。媒體安全檢查不得被 preview 優先權越過。
- 測試兩個長工作已佔滿 slot 時新直播到達的等待；不得承諾在不增加 slot、不搶占下零等待。若現有優先序不足，先呈現 admission 最小調整，不擴容掩蓋。

## 4. 每小時 maintenance

- UTC 固定每小時一次（候選 17 分），避免與整點大量排程重疊。
- 僅做到期檢查、分批 storage 刪除、失聯／漏送核對及持久化重新派工，不解碼／轉檔。
- 候選每輪最多 5 分鐘，分頁批次、持久化游標或可重入掃描；量測後確認資源與批次上限。積壓與最老等待要告警，不能悄悄永遠清不完。
- 不清理有效 lease、正式引用、VOD 共用 sprite 或尚需復原的素材；引用與刪除以既有 fencing／冪等機制保護。
- 存取到期獨立由 API／媒體授權執行，不等待每小時刪除。物件實體回收可能延後一個排程週期加處理時間。
- 最後補漏發現可能接近一小時；不得把這個延遲用於正常直播恢復或五分鐘封面更新。

## 5. 實作任務與檔案範圍

### Task A — 基準與工作供給盤點
- [ ] 讀取最新 AGENTS／README、實際配置及所涉 claim 路徑。
- [ ] 建立所有工作入口、due time、lease、優先序、下一階段、清理保護矩陣。
- [ ] 收集完整無工作日與代表性工作窗口，保留所有 executions，區分成功、有工作、無工作、失敗重試。
- [ ] 固定內部 event／generation／ownership 規則及配置預覽，不改 OBS wire。

### Task B — 可靠喚醒與 admission
檔案：新增 internal/recordingqueue/{event,dispatcher,job,azure_queue}.go 及測試（命名可依現有 transport 共用能力縮減）；internal/postgres 新 outbox／admission；internal/migrations/sql 下一個 additive migration；cmd/server/main.go；各 recording 工作建立／階段切換的 repository 路徑。
- [ ] 先寫 DB 交易、重複事件、generation 競態、多 replica、發送失敗測試，再實作。
- [ ] Queue transport 若可直接共用就共用；不先抽出跨服務通用框架。
- [ ] 測試 receipt 已接受後消息故障不改既有 API 結果、poison 不遺失工作。

### Task C — 工作模式與恢復
檔案：cmd/recording-worker/main.go、internal/recordingvalidation、internal/recordingprocessing、internal/postgres/recording_live_worker.go 及各 claim；新增 cmd/recording-maintenance/main.go（或既有 binary 獨立 maintenance 模式）。
- [ ] 先寫各模式禁止跨組領取、global slots<=2、長 visibility 續租、失聯與退出競態測試。
- [ ] 分離既有 orchestration，重用處理演算法；維持完整 FFmpeg／hash 驗證。
- [ ] 直播 owner 連續處理，下一階段／retry 持久化後退出。
- [ ] 小規格 maintenance bounded cleanup／引用保護／游標回復測試。

### Task D — Event Job 基礎設施與發布
檔案：infra/main.bicep、infra/main.bicepparam.example、infra/README.md、.github/workflows/release.yml、相關 rollout／smoke scripts、internal/config/config.go。
- [ ] 新 queues、poison、最小 sender／processor／reader 身分權限；不輸出 secret。
- [ ] Event Jobs 分組、mode allow-list、timeout、polling、min=0、保守 scale；hourly maintenance 小規格。
- [ ] 新功能初始關閉；原 cron 過渡保留，禁止未核准同時啟用兩套主動 dispatch。
- [ ] release 同步驗證新的 trigger／資源／開關，不再硬性要求重 Job 每分鐘 Schedule。
- [ ] Bicep what-if 必須只含已審查的 queue、identity role、Job 與設定差異。

### Task E — 測試、成本與切換
- [ ] Go race、DB integration、contract／policy、Bicep、實際 FFmpeg fixtures、required CI 全通過。
- [ ] 兩 API replicas、高速 confirm、雙長工作、毒消息、queue timeout、worker kill、visibility expiry、5 小時 owner 交接（時間控制測試）、輸入消失與復線。
- [ ] 零工作至少 24 小時：重媒體 Job 不應因固定 cron 啟動；零工作事件重送造成的 executions 必須列入並修正。
- [ ] 一場正常 S10 短直播＋上傳驗證／source transcode fixture＋封面更新／預覽＋自然收尾；不重跑完整 Windows 舊套件。
- [ ] 執行 §6.1 降規對照：完整驗證、原始影片轉檔、封面、時間軸各自報告耗時／記憶體／總成本；同組任一必要案例失敗，整組回復原規格，不臨時拆新 Job。
- [ ] 分開驗證觸發模式回滾與單組資源回復；兩者均保留全域 slots=2。
- [ ] 呈現成本及 latency 結果後才核准正式觸發切換。

## 6. 成本量測與驗收

現況每分鐘排程每天有 1,440 次觸發機會（不是假設每次都成功執行，也不是 24 小時持續滿載）。新 hourly maintenance 每天 24 次觸發機會。兩者次數差不能直接當帳單省費比例。

分別比較：
1. 零工作基線：所有 Job startup、無 claim、maintenance、dispatch、Queue、監控成本。
2. 相同素材及相近負載的每場／每片成本：包括所有 rendition、attempt、下載/hash、probe/decode、轉檔、封面／preview、storage 操作。
3. 直播連續成本與延遲：不是只比較完成時間，也量測 verified backlog、claim等待、會員實際時間碼延遲。

收集 CPU／記憶體配置、峰值、完整 execution 生存時間、processing/non-processing、空執行、重試、queue requests、DB負載。CPU×execution seconds 僅為資源時間代理；正式結論以 Azure Cost Management／usage meter 對帳，免費額度前後分列，不把 execution 時間或 CPU 使用率當帳單。

預期可減少：每分鐘高規格空跑與輕工作被迫使用大容器的費用。不能保證消除真正處理成本，也不能將使用者所報 NT$2,653 全列為可省。

資源降規只作候選，不視為已驗收。live 保持 4 CPU／8 GiB；processing 測 2 CPU／4 GiB，preview 測 1 CPU／2 GiB。任何組未通過以下門檻，回復原 4 CPU／8 GiB；不得以降低完整驗證、圖片品質或增加全域併發補償。暫不承諾節省百分比。

### 6.1 降規對照與接受門檻

**比較方法：** 用同一 immutable image、同一 Event 觸發方式、同一素材／分段／驗證參數，分別跑原 4 CPU／8 GiB 與候選規格；固定快取條件並分開報冷／暖結果。事件化節省與降規節省分開計算，不能拿舊 cron 的空跑費掩蓋降規後單件成本上升。每項代表性案例至少三次，報中位數與最大值；小樣本不宣稱穩定 p95。

**四類分別驗收：** 完整驗證、原始影片轉檔、單張封面、時間軸。涵蓋短素材、代表性 2.5 小時素材、接近已支援上限的素材，以及直播佔用另一 slot 時的資源競爭。既有素材／隔離 fixtures 優先，不要求 Windows 重開長直播。

| 面向 | 必須提供的證據與門檻 |
|---|---|
| 正確性 | 完整驗證與輸出規格一致；不新增漏驗、時間線／公開範圍錯誤，不降低封面／預覽品質或取樣密度 |
| 耗時 | 分開量測排隊、啟動、下載/hash、probe/decode、轉檔／取圖與總完成時間；所有案例在既有最短有效 timeout 內完成，計劃要求至少保留 20% 時間餘裕；不延長 timeout 來讓降規通過 |
| 工作供給 | 代表性到達速率下佇列基線不持續增加；完整驗證／轉檔分別報告變慢幅度；不得造成直播 verified backlog 持續增加、破壞既定穩定約 2–3 分鐘播放目標，或讓封面／時間軸工作持續飢餓 |
| 記憶體 | 量測整個容器及 FFmpeg 子程序的記憶體峰值，包含 cgroup 計費／限制相關的快取與暫存使用；無 OOM／因資源不足新增重試，計劃要求至少 20% 記憶體餘裕；preview 2 GiB 不能只看 Go heap |
| 成本 | 比較每件成功產物的所有 attempts、啟動／等待、CPU與記憶體用量、Queue、儲存操作；候選總成本須下降且超過觀測波動。僅 CPU 數變少、估算近似持平或證據不足，不算通過 |
| 復原 | 降規失敗仍保留原工作與冪等，資源回復後可安全接續；不重建 capture 或手動發布 |

20% 為本計劃的安全餘裕門檻，不是已量測結果。若既有基準本身不達門檻，先列出容量限制，不能歸咎候選或偷偷放寬支援範圍。正式計費資料尚未入帳時，只能標記技術／資源代理結果，不結案為帳單節費通過。processing 同組任一類型失敗回復整組；preview 同理，是否再細拆留待獨立決策。

## 7. 切換步驟

1. 發布 additive DB／程式與關閉的 queues/jobs；保留舊 trigger，outbox 可 shadow 記錄但不投遞，不啟動重複處理。
2. 隔離環境驗證事件可靠性、成本、資源規格與完整媒體結果；盤點準備切換的待處理工作。
3. 確認無正式直播／錄影進行、既有長 execution 自然完成。展示新鮮 what-if、identity scope、開關與回滾配置，取得正式變更核准。
4. CI/CD 受控切換：停止舊 Schedule 的後續觸發（保留可回復 Manual 入口），啟用 Event 模式與 hourly maintenance。任何短暫空窗由 durable DB 工作承接。
5. 有界補建既有 eligible 工作的喚醒，避免全歷史資料重放。已 running／completed／terminal 的工作不可重派。
6. 讀回各 Job image、trigger、scale、資源、身分與開關；確認 processing slots 未增加。
7. Windows 一次最小短場驗證首批冷啟動、跨分鐘連續播放、三畫質進度、封面／preview供給與自然收尾；不改外掛。Mac 另驗 source upload、清理與故障補漏。
8. 24 小時空閒觀測及下一次正常工作量／計費回報後，關閉過渡追蹤項；保留必要回滾配置。

## 8. 回滾

**先區分資源回復與事件機制回滾：**
- 只有降規不達標：透過版本化 CI/CD 將失敗組回復原 4 CPU／8 GiB，Event 觸發保持不變；其他通過組不變。先停止該組新 admission、讓在途工作完成或安全釋放 lease，再切規格；不硬殺後盲目重派。回讀 Job 配置與 image，以原工作確認接續，並重新量測。
- admission／Queue／事件恢復機制失敗：才使用以下完整觸發模式回滾。不能因 preview 降規失敗就自動恢復全站每分鐘 4 CPU cron。
- 維護小規格不達標時不准直接啟用；採已驗證資源設定，另行核對成本。不能以漏清理、無限批次或新增高頻排程掩蓋。

完整模式回滾觸發：工作無法恢復、verified backlog持續增加、併發超限、反覆 OOM／啟動風暴、清理安全問題，或同負載成本惡化。

- 透過版本化 CI/CD 設定停止新事件投遞與 Event scale，保留 Queue／outbox／工作資料。
- 停止新 claim admission，讓有效工作安全完成；不得強殺後立即重派。必要時等 lease 到期並確認 fence。
- 恢復相容的已知健康 worker image 與原每分鐘 Schedule；這會恢復原成本，但先保護處理可用性。
- 回滾前後都維持全域 slots=2；不讓舊 cron 與 Event consumer 同時自由領取。
- additive migration 不 down、不刪事件資料；消息可能重投，但既有冪等保證不重複產物／發布。
- 固定版本、影響時間窗、未完成工作與成本異常紀錄；不更動正式場內容。

## 9. 完成定義與交付

- 同一資料庫兩個媒體 slot、完整驗證、wire／授權不變；無工作時高規格 Job 歸零。
- 正常直播沒有重新出現週期性交接空檔；達到既定穩定約 2–3 分鐘會員延遲方向，不持續累積。
- 事件故障能在正常恢復路徑接續；hourly maintenance不是常態工作供給。
- 每組延遲、成本代理與正式計費限制分開報告；不宣稱未量測節省。
- 提供 PR/CI、commit、各實際 image／Job config、切換與回滾證據、Windows最小驗收 prompt。
- 歷史弱網／seek與壓力追趕判定保留；不要求重跑已通過固定金鑰、16分鐘續傳與2.5小時長場。

## 參考

- 現有模式：asset-api internal/derivativequeue/*、cmd/derivative-worker/main.go、infra/main.bicep。
- Azure Event Job scaling：https://learn.microsoft.com/en-us/azure/container-apps/tutorial-event-driven-jobs
- Azure Job 計費：https://learn.microsoft.com/en-us/azure/container-apps/billing

狀態：僅完成實作計劃。未修改產品程式、infra、正式資料、排程或工作資源。

## 執行期修訂：資源配對

使用者於本 session 確認完整驗證／轉檔候選為 **2 CPU／4 GiB**（原 2 CPU／8 GiB 為口誤）。候選 memory 與 CPU 綁定：processing 2／4 或回復 4／8；preview 1／2 或回復 4／8。官方 Consumption 配對限制已核對，不採不支援的 2／8 組合。
