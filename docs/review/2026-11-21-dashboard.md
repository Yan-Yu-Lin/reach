# Dashboard 修正與重新設計（2026-11-21）

- 分支：`fix/dashboard-2026-11`，從 `8c74086` 開出。只改了 `dashboard/` 和這份報告。
- 依據：`~/30-39-Research/34-tech-research/2026-11-20-reach-dashboard-frontend.md` 的 A 表和 B 部分、兩份安全報告裡跟前端有關的項目（Opus P2-4、P2-7、P3-5；daybreak P2-1、P3-1），以及後端的 `docs/EVENTS.md`（`fix/hardening-2026-11` 的 41fbbff）。
- 新截圖：`~/30-39-Research/34-tech-research/2026-11-21-reach-dashboard-v2-shots/`
- 量測原始資料：同一個資料夾下的 `measurements/`（`before-all.json`、`after-all.json`、`after-oldsrv-all.json`、`summary.md`）
- 驗證（第 2 輪後）：
  - `npm run build`、`npm run generate` 都通過。
  - `nuxt typecheck` 0 個錯誤。
  - `vitest` 62/62 通過。
  - 瀏覽器回歸測試 `test-harness/regress.mjs` 19/19 通過。
  - 對真實 reachd 的 revoke e2e 通過。
  - 審查意見的處理見「astra round 1／daybreak」一節。

---

## 結論

- **手機版從「不能用」變成「主要介面」。** 底部三個分頁：機器、請求、設定。所有可點的東西都至少 44px，畫面沒有橫向捲動，這些都由 `shots.mjs` 自動量測確認。
- **平常幾乎不會再發 API 請求。** heartbeat 改成直接更新本地資料：真實節奏 15 秒內的請求從 20 個降到 0；每秒 50 個心跳持續 10 秒的風暴，從 465～472 個請求降到 0。
- **首頁一打開就看得到「Needs you」**，裡面依序列出待核准、剛掉線（7 天內）、退役卡住。沒事的時候顯示綠色的「All good」。二十幾台死很久的機器被收進預設摺疊的 Dormant 群組，群組裡有「Disable all N」可以一次全部停用。
- **危險操作：**
  - Remove 要輸入機器名稱才能確認，而且放在頁面最底部的 Danger zone，跟 Disable 分開，各自附一段白話說明。
  - 假的「Approve as...」已經拿掉。
  - 鍵盤 `a` 只會打開確認框，不會直接核准。這點有實測：按下後 approve API 被呼叫 0 次。
- **核准畫面會標示資料可不可信：**
  - 「伺服器看到的 IP」標成 seen by server。
  - 對方自己填的欄位（metadata）標成 self-reported。
  - 兩個 IP 不一致時，用紅色標出 does not match。

---

## 前後對照數字

量測方法：同一個 harness（`dashboard/test-harness/`）、同一台 Mac、Chromium headless shell 1248。

- 「4G」是指模擬網路：延遲 150ms、下行 1.6Mbps。
- 手機在冷載入（第一次打開頁面）時，另外把 CPU 降速 4 倍，模擬中階手機。
- 三組設定的意思：
  - **before**：8c74086 的 bundle，搭配舊版伺服器的事件格式。
  - **after**：新 bundle，搭配新版伺服器（heartbeat 不帶 id、payload 有 `observed_state`/`status`、支援 `?status=pending`、request 有 `observed_ip`）。
  - **after-oldsrv**：新 bundle，搭配舊版伺服器。這組驗證前端比後端先部署時也能正常運作。

### 首次載入（40 台機器、2 筆待審）

| 項目 | before | after | after-oldsrv |
|---|---|---|---|
| 4G 手機：機器列表出現 | 2693 ms | 2689 ms | 2704 ms |
| 4G 手機＋gzip：機器列表出現 | 1629 ms | 1589 ms | 1576 ms |
| 4G 桌面：機器列表出現 | 2746 ms | 2665 ms | 2658 ms |
| 4G 桌面＋gzip：機器列表出現 | 1523 ms | 1541 ms | 1536 ms |
| 載入時的 API 請求數 | 5（machines ×2、requests ×2、events） | 4（machines ×1、pending ×2、events） | 4 |
| 傳輸量（沒開 gzip） | 348 KB | 315 KB | 340 KB |
| 200 台機器的傳輸量 | 715 KB | 499 KB | 524 KB |
| long task（手機，CPU 降速 4 倍） | 1 個（102 ms） | 0 | 0 |
| long task（桌面） | 3 個（最長 69 ms） | 0 | 0 |

- 載入時間基本上持平。瓶頸是 JS 下載，所以 **nginx 開 gzip 還是最有效的一步**：大約從 2.7 秒降到 1.6 秒。這要動 jason，需要 Arthur 同意，見最後一節。
- `machines` 的重複抓取已經拿掉了。pending 還是會抓兩次：第一次是 mount 時的快照，第二次是 SSE `hello` 之後的補抓。後者是刻意保留的，用來補上「快照之後、訂閱之前」這段空窗裡新進來的請求，而且 v2 伺服器上這個回應幾乎是空的。
- `requests` 只抓 pending，所以載入時 200 台的情境少了約 216 KB（原本每次都抓 60 筆歷史紀錄）。
- 新 bundle 比較大：
  - 頁面 chunk 從 37 KB 變成 67 KB（gzip 後 12→22 KB）。
  - CSS 從 16 KB 變成 23 KB。
  - 主檔（框架 runtime）幾乎沒變，173→175 KB。
  - 在 4G 下看不出差別，就是上表裡的數字。
- DOM 節點變多：40 台時從 541 變成 1034。原因是每一列多了狀態圖示（SVG）、第二行說明文字和獨立的 copy 按鈕。200 台時是 3439 個節點，仍然沒有 long task。

### 事件風暴（桌面 40 台，標註手機的是 393×852 並把 CPU 降速 4 倍）

| 情境 | before 請求數 | after 請求數 | before long task | after long task |
|---|---|---|---|---|
| 真實節奏心跳（15 秒內 20 次） | 20 | **0** | 0 | 0 |
| 心跳風暴：每秒 50 次 × 10 秒（桌面） | 465 | **0** | 0 | 0 |
| 心跳風暴：每秒 50 次 × 10 秒（手機） | 472 | **0** | 0 | 0 |
| 狀態跳動風暴：每秒 20 台 × 10 秒（桌面） | 170（全量 refresh 85 次） | 193（全部是單台 GET） | 0 | 0 |
| 同上，API 回應 400ms | 44 | 78 | 0 | 0 |
| 同上，手機 | 174 | 195 | 0 | 0 |
| 跳動風暴中點機器到詳細頁出現（手機） | 218 ms | **129 ms** | | |

**跳動風暴的請求數比較多，這是刻意的取捨，不是退步。**

- 以前每個 `ssh_config.changed` 都會觸發一次完整 refresh，每次抓兩支 API（40 台機器的完整清單，加上 60 筆請求歷史）。
- 現在只抓受影響的那一台（A7）。請求「個數」多了 13%，但每個請求只回傳一台機器。
- 風暴期間的傳輸量估算（mock 裡單台約 0.85 KB，完整清單約 34 KB，requests 約 9 KB）：
  - before：85 × 43 KB ≈ 3.6 MB
  - after：193 × 0.85 KB ≈ 0.16 MB
- 同一時間最多只有 3～4 個請求在跑，跟 refreshCoordinator 原本的上限一樣，請求不會越積越多。
- 狀態跳動的事件已經直接帶了 `observed_state`/`status`，所以畫面在 GET 回來之前就會更新。單台 GET 只是為了 `ssh_config.changed` 需要的 tunnel 細節。

主執行緒時間在跳動風暴下比較高（桌面 861→1312 ms／10 秒，都分散在小任務裡，沒有 long task）。原因是每次只更新一台，DOM 會被小幅改動很多次（841→1386 次 mutation）。對 Arthur 來說這不是實際問題，因為他真實的機群只有 2～5 台在線。

### 長時間執行與重連

| 項目 | before | after |
|---|---|---|
| 2 分鐘、每秒 20 次心跳：heap | 2.8 → 3.4 MB | 3.2 → 4.0 MB（第 30 秒後持平） |
| DOM 節點 | 645 → 645 | 1085 → 1213（第 10 秒後持平） |
| listener 數 | 120 → 120 | 113 → 126（第 10 秒後持平） |
| SSE 被切斷 10 次後殘留的連線 | 0 | 0 |
| 這 10 次重連打了多少 API | **2,327 個單台 GET**，加上 6 次 machines 和 6 次 requests | 5 次 machines、6 次 pending（新伺服器）；1 次 machines、2 次 pending（舊伺服器） |

- 表裡 after 的 1085 → 1213，是第一個心跳把這台機器的 Connection check 從 unknown 變成 ok 時多畫出來的節點，之後就沒有再長。所以沒有記憶體洩漏。
- 舊版重連時，伺服器會重播最多 500 筆 heartbeat，前端每筆都打一次 GET，所以才有 2,327 個請求。
- 新版：
  - 用 `Last-Event-ID` 續接（after-oldsrv 這組）時，不會再抓快照，重播的 heartbeat 只在本地更新。
  - 新伺服器的 heartbeat 本來就不會重播，所以前端只抓快照，不會一筆一筆補抓。

---

## A 表逐項對照

| # | 發現 | 怎麼修 | 證明 |
|---|---|---|---|
| 1 | 手機版 grid 整個壞掉（從 `d566bf1` 就壞了）；待審卡片 sticky 會疊在一起 | CSS 改成 mobile-first：手機是 flex 欄加上 fixed 底部分頁，≥900px 才變成 grid；`100dvh`；考慮 safe-area；拿掉 sticky。另外只渲染一份機器列表（`isDesktop`），不再藏一份在背景 | 截圖 01～14；`facts.json`：手機四個畫面 `under44: []`、`horizontalOverflow: false` |
| 2 | nginx 沒開 gzip 和 cache | **沒做**：要改 jason 上的 nginx，不在前端範圍 | 量測顯示開 gzip 可以從 2.7 秒降到 1.6 秒（見上表） |
| 3 | repair/uninstall 用 `sudo sh` | `utils/commands.ts`：一律用 `bash -s`；user-mode 不加 sudo；Windows 改用 PowerShell 的 `-Repair`/`-Uninstall` | `tests/commandsAndRequests.test.ts`：「pipes setup.sh into bash, never sh」、「matches the README forms」 |
| 4 | 假的 Approve as；鍵盤 `a` 不經確認就核准 | 拿掉 Approve as（P3-5）；`a` 改成打開和按鈕一樣的確認框 | `facts.json`：`keyboardAOpensDialog: true`、`keyboardAApproveCalls: 0`；截圖 26 |
| 5 | 每個 heartbeat 都重抓 | `fleetEvents.ts` 把 heartbeat 規劃成 `patch`；`fleetStore.applyPatch` 以 copy-on-write 更新，亂序到達的舊 heartbeat 會被忽略 | `fleetEvents.test.ts`、`fleetStore.test.ts`；風暴量測 465→0 |
| 6 | 首次載入抓兩次 | 先抓快照、再開 SSE；第一個 `hello` 在 3 秒內就不再抓全量，只補抓 pending；續接的連線完全不抓 | 載入量測：machines 2→1 |
| 7 | `ssh_config.changed` 觸發全量 refresh | 有 `machine_id` 就只刷那一台；`request.*` 只抓 pending；`machine.<state>` 這類重複事件直接忽略 | `fleetEvents.test.ts`；跳動風暴的請求全部變成單台 GET |
| 8 | Remove 只用 `confirm()`，還跟 Disable 並排 | `ConfirmDialog` 加上輸入名稱確認；Danger zone 放在頁面最底；Disable、Remove、Force retire 各自有說明；焦點預設在 Cancel（或輸入框） | `facts.json`：名稱打錯時按鈕 disabled、打對才 enabled；截圖 09、11、22 |
| 9 | Needs attention 被死機器淹沒；retiring 卡住看不出來 | `utils/status.ts`：offline 超過 7 天算 dormant；retiring 超過 24 小時，或 `cleanup_state=timed_out`，算 stuck；新增 `cleanup_failed`/`disable_failed` 狀態；Dormant 群組加「Disable all」 | `status.test.ts`：「splits offline…」、「marks retiring…stuck」、「keeps long-dead machines out of Needs attention」；截圖 01、03、14 |
| 10a | 狀態顏色不一致 | 所有地方都只用 `uiStatus()` 判斷狀態，`--st-*` 每個狀態只有一個顏色，再搭配不同的 SVG 形狀和文字（見下表） | 截圖 01、20（淺色）、21 |
| 10b | 桌面版看不到 Setup | 頂列加了「Settings & setup」 | 截圖 24 |
| 10c | 一打開就要求通知權限 | 只有在 Settings 按「Turn on notifications」才會請求；收到 `request.created` 時只在已經 granted 的情況下才跳通知 | `grep requestPermission`：只剩按鈕的 handler 裡有 |
| 10d | 登入帳號欄預填 admin | 清空，並加上 `autocapitalize=off` | `facts.json`：`loginUsernamePrefill: ""`；截圖 30 |
| 10e | 「Setup (no sudo)」標籤錯了 | 改成「Linux / macOS on a network that blocks SSH」，並說明這是走 WebSocket；另外加上 Windows 指令 | 截圖 13 |

### 安全報告裡跟前端有關的項目

| 項目 | 怎麼處理 |
|---|---|
| Opus P2-4／daybreak P2-1：source_ip 可以偽造 | 核准卡片上：<br>- `observed_ip`（v2 伺服器才有）標成 **seen by server**。<br>- 舊伺服器沒有這個欄位，就顯示 metadata 裡的值，並標成 **requester's own claim, not verified**，同時跳出警告。<br>- 兩個 IP 不一致時，用紅字標出 **does not match what the server saw**。<br>- slug 和 target_user 優先使用伺服器綁定的頂層欄位。沒有綁定時，標成「self-reported, can change at setup」。 |
| Opus P1-2：核准畫面看不到最後的 slug | 卡片第一行就寫「Will appear as `ssh <slug>`」，確認框裡也會再寫一次。 |
| Opus P3-5：假的 Approve as | 已拿掉。 |
| Opus P2-7／daybreak P3-1：JWT 存在 localStorage | 部分處理：<br>- 登入頁加了「Keep me signed in」，不勾時 token 只存在 sessionStorage，分頁關掉就沒了。<br>- Settings 加了「Sign out everywhere」（`POST /admin/sessions/revoke-all`）。舊伺服器回 404 時，會顯示「需要先更新 reachd」。<br>- **沒有改成 HttpOnly cookie**：那要後端加 cookie、CSRF 和 SameSite，再加上 nginx CSP 拿掉 `unsafe-inline`，都不是前端單方面能做的。<br>- 程式裡仍然沒有 `v-html`／`innerHTML`。 |
| daybreak P1-2／EVENTS.md：cleanup 失敗 | `cleanup_failed`／`disable_failed` 會顯示成「Cleanup failed」，加三角形圖示和紅框，並列在 Needs attention。<br>- `disable_failed` 提供「Retry disable」。<br>- 其他狀況提供「Force retire」（`retire-now`）。<br>- 後端回 409 時，會用 toast 顯示錯誤訊息，並重抓那一台機器，不會當成成功。 |

---

## B 部分：資訊架構

- **手機**（截圖 01～14）：
  - 頂列：Reach、Live 狀態、主題切換。
  - 首頁依序是：Needs you 或 All good 橫幅、搜尋框、各群組。群組依序是 Needs attention、Online、Dormant（摺疊）、Disabled & retiring（摺疊）。retired 的機器不顯示，這也配合伺服器預設不回傳。
  - 底部分頁是 Machines、Requests、Settings，分頁上有數字徽章。
  - 每一列機器：狀態圖示、等寬字的名稱、相對時間、狀態文字、傳輸方式和系統，右邊有一顆 52px 寬的 copy 按鈕。
- **詳細頁**（截圖 07、08、10、11、21～23），由上往下：
  1. 名稱、大狀態標籤、一句白話（例如「No heartbeat for 28h. The tunnel is down; ssh will not work.」）。
  2. `ssh <name>` 和 Copy 按鈕。
  3. Connection check，三格：Hub probe、Tunnel、Target sshd。每格各自顯示 OK、Failed 或 Unknown 加上原因。agent 的回報超過 10 分鐘就算 Unknown，不再假裝 OK。沒有 probe 資料也算 Unknown，修掉以前「永遠綠色」的問題。原本寫死的 `:443` 已經拿掉。
  4. Diagnostics。
  5. Advanced，預設收合：desired、observed、cleanup 的白話解釋，update policy、process title、ID、repair 和 uninstall 指令。
  6. Danger zone。
- **桌面**（截圖 20～27）：左邊固定是機器列表，右邊是內容區，頂列有三個分頁。首頁另外多一排統計卡和待審卡片。詳細頁是兩欄。
- **網址狀態**：`?m=<id>`、`?tab=requests|settings`。手機的返回手勢會回到上一頁，不會直接離開 dashboard。

### 狀態對照

| 狀態 | 顏色變數 | 形狀 | 什麼時候 |
|---|---|---|---|
| Online | `--st-online` 綠 | 實心圓 | observed=online |
| Degraded | `--st-degraded` 琥珀 | 半實心圓 | observed=degraded |
| Offline | `--st-offline` 紅 | 粗的空心圓 | offline/gone，且 7 天內還看過 |
| Dormant | `--st-dormant` 灰 | 虛線空心圓 | 超過 7 天沒看到，或從來沒上線 |
| Pending | `--st-pending` 藍 | 菱形，會脈動 | status=pending |
| Disabled | `--st-disabled` 灰藍 | 暫停符號 ⏸ | desired=disabled |
| Retiring | `--st-retiring` 灰藍 | 循環箭頭 | retiring 還不到 24 小時 |
| Retire stuck | `--st-stuck` 琥珀，標籤有紅框 | 循環箭頭 | retiring 超過 24 小時，或 `cleanup_state=timed_out` |
| Cleanup failed | `--st-failed` 紅，標籤有紅框 | 實心三角形加驚嘆號 | status 或 cleanup_state 是 `cleanup_failed`／`disable_failed` |

深色和淺色主題各有一組顏色，兩組都經過肉眼確認（截圖 01／01-light、14-dark／14-light、20-dark／20-light）。系統設定了 `prefers-reduced-motion` 時，所有動畫都會關掉。

### 資料層

- `useFleet`：
  - 機器存在 `shallowReactive(Map<id, MachineWithTunnels>)` 裡。
  - 全量快照用 `applySnapshot`，沒變的機器保留原本的物件，所以那一列不會重新渲染。
  - 單台更新用 `upsertMachine`；SSE 修補用 `applyPatch`。
- `refreshCoordinator` 新增 `requests` 這一條路線，可以跟單台刷新合併成同一批；遇到全量 refresh 時會被吸收掉。
- `useSSE` 會記下這次連線是不是用 `Last-Event-ID` 續接的（`wasResumed()`）。
- 把背景分頁切回前景時，SSE 不是 live 就立刻重連，不等 backoff 計時器。如果離開超過 60 秒，就補抓一次快照，因為 heartbeat 不會重播。
- 以前根元素綁 `:data-tick`，每 15 秒整頁重繪。現在改成共用的 `useNow()` 時鐘，只有顯示時間的地方會更新。

---

## astra round 1／daybreak

- **審查報告：**
  - `~/30-39-Research/34-tech-research/2026-11-21-reach-dashboard-astra-review.md`：判定 FIX，審的是 `ec9ca8a`。
  - `~/30-39-Research/34-tech-research/2026-11-21-reach-hardening-security-recheck-daybreak.md`：其中 P3-5～P3-7 是 dashboard 的問題。
- **F01**（revoke-all 沒有關掉既有的 SSE）是後端的問題，已在 hardening `2ef1460` 修好。前端這邊只驗證「被撤銷的分頁會回到登入頁」，見下面的 e2e。
- **commit：**
  - `8d2f659`：mock 加上回歸測試
  - `ec24c88`：F02～F06
  - `4214388`：F08、F09
  - `b2784f5`：F07、F10、P3-5、P3-7
  - `1b295f3`：用真實 reachd 跑的 e2e
  - `208f92d`：截圖腳本
- **注意：** `ec24c88` 和 `4214388` 單獨 checkout 時，`nuxt typecheck` 會失敗。原因是 `useReachApi.ts` 引用了 `b2784f5` 才加進來的 `loginLocation`/`currentAppPath`，但 vitest 這兩個 commit 都能通過。這段歷史已經 push，所以沒有改寫。要 bisect 的話，請以 `b2784f5` 之後的 commit 為準。

### 測試怎麼證明修好了

照 astra 的要求，新增的測試會讓真正的 bundle 在瀏覽器裡跑，驗證**畫面上看到的東西**和 **mock 實際收到的 API**，不只驗證 planner。

- **`test-harness/regress.mjs`**：用 Chromium 跑 generate 出來的 bundle，搭配 `mock.mjs`，共 19 個案例。
  - **審查版 `ec9ca8a`：3/19 通過**。也就是說，這些案例在舊程式上確實會失敗，能重現審查發現的問題。
  - **現在：19/19 通過**。
  - 紀錄存在 `measurements/regress-ec9ca8a.txt` 和 `measurements/regress-round2.txt`。
- **`test-harness/real-backend-revoke.mjs`**：不用 mock，直接用 `fix/hardening-2026-11`（`3646d23`）build 出來的 reachd。reachd 跑在 `/tmp`，設定 `provisioning_enabled: false`，只聽 loopback。
  - 裝置 A 打開 `?tab=requests` 並連上 Live。
  - 裝置 B 按「Sign out everywhere」。
  - 結果：A 的 SSE 被伺服器關掉，重連時拿到 `GET /api/admin/events 401`，**1.6 秒後**回到 `/login?reason=session-expired&next=/?tab=requests`，兩種 storage 都已清空。B 顯示「Signed out everywhere」。
  - 紀錄存在 `measurements/real-reachd-revoke.json`。這個腳本會拒絕連到非本機的 reachd。
- **vitest**：62 個測試，比上一輪多 5 個，新增的是 F03 交錯情境的 store 測試。
- **typecheck 和 generate**：都通過。

### mock 的修正

astra 指出，原本的 mock 會把問題蓋掉。改了以下幾點：

| 原本的問題 | 現在 |
|---|---|
| 重連時沒有 durable replay | 有真正的事件 log。帶 `Last-Event-ID` 重連時，只重播 cursor 之後的 durable event。舊格式（HBFMT=old）的 heartbeat 也會進 log，上限 500 筆。新格式的 heartbeat 不進 log，也不重播。超出範圍時回 `resync_required`，並帶空的 `id:`。 |
| 等完 latency 才讀資料 | 請求一到就把回應內容複製一份存起來，等 latency 過了再送出。所以回應有可能比這段時間內推送的事件還舊，跟真的 DB 讀取一樣。順帶一提，第一次修的時候 clone 還放在 latency 之後，用 `dbg-f03` 抓出來後才改到請求到達時。 |
| POST 一律回成功 | disable、enable、remove、retire-now 會真的改機器狀態，並推送跟 reachd 一樣的事件。`/__fail` 可以注入 409 或 500，`/__latency` 可以注入延遲。 |
| 沒有登入和撤銷 | 有登入（`AUTH=1`）、session version、revoke-all。revoke-all 會關掉既有的 SSE，跟 `2ef1460` 一樣。也可以用 `/__revoke?close=0` 模擬「token 失效，但連線還沒被關掉」的情況。 |

### 逐項處理

| # | 問題 | 怎麼修 | 證明 |
|---|---|---|---|
| F02 / daybreak P3-6 | 首次 hello 的 3 秒去重有空窗 | `useFleet.start()` 改成**先訂閱，等收到 hello 才抓快照**。不再判斷「3 秒內」，每次 hello 都抓。如果 2.5 秒內還沒收到 hello（SSE 連不上），也會先載一次快照，讓畫面有資料。 | regress「F02 a machine disabled between the snapshot read and the subscribe」：mock 讓 SSE 延遲訂閱、快照延遲回應，並在兩者之間把 omarchy 停用。舊版顯示 Online，新版顯示 Disabled。 |
| F03 | 舊的 HTTP 回應蓋掉比它新的 SSE 資料 | `fleetStore.createPatchLog()`：<br>- 有 HTTP 請求還沒回來時，每個機器事件都依序記錄。<br>- 回應套用之後，把「請求開始之後」收到的 patch 再套一次。<br>- 快照裡沒有、但被事件提到的機器，不會因為快照就刪掉，而是改成單台重抓。<br>- 快照和 pending 分開套用，pending 回應慢不會拖住機器快照。<br>- 同一筆 heartbeat 重播時會再套一次它帶的狀態。 | regress 兩個 F03 案例（全量快照、單台 GET，都延遲 1.2 秒）；vitest `fleetStore.test.ts` 的「HTTP response vs newer SSE patch」五個案例。 |
| F04 | heartbeat 後，詳細頁的診斷不會更新；舊的 observation 被當成剛回報 | - heartbeat 只寫 `live_heartbeat_at`，不改 `agent_observation.heartbeat_at`。<br>- Connection check 的 Tunnel 和 sshd 用 observation 本身的時間判斷新舊。<br>- 打開詳細頁一定會抓一次單台資料。<br>- 已打開的那台收到 heartbeat 時重抓，最多每 15 秒一次。<br>- 沒打開的機器收到 heartbeat 仍然是 0 個 GET。<br>- 首次快照之前收到的 patch 不再觸發 GET，因為快照馬上就會來。 | regress「F04 the open machine shows new diagnostics…」：版本、傳輸、persistence 都更新了，而且沒打開的機器 GET 數為 0。regress「F04 a heartbeat does not make an old tunnel report look current」：12 小時前的 connected/healthy 現在顯示 Unknown。 |
| F05 | 有 cursor 的重連被當成資料已補齊 | 拿掉 `wasResumed()` 的短路判斷，續接後的 hello 一樣會抓快照。 | regress「F05 reconnecting with a cursor…」：SSE 被拒 1.5 秒期間 jack 恢復上線，而且只有 live heartbeat，沒有 durable event。重連後畫面顯示 Online，也有快照 GET。 |
| F06 | requests lane 失敗後不會 retry | 每個 lane 有自己的錯誤狀態和 backoff：pending 失敗時用 `requestsRetry` 重試，成功後只清掉 pending 自己的錯誤。 | regress「F06 a pending-requests failure is retried」：注入一次 500 之後，請求出現在畫面上，錯誤橫幅也消失。 |
| F07 | Disable all 確認時沒有重新檢查名單 | - 確認框只存 id，顯示的清單和實際送出的清單都從最新資料重算。<br>- 已經不是 dormant 的機器會被移出清單，並顯示「No longer dormant, left out」。<br>- 按下確認時再篩選一次。<br>- 有失敗時列出機器名稱和伺服器回的原因。<br>- 確認之後在網路上才發生的競態，要等後端提供條件式 bulk API，這裡沒有處理。 | regress「F07 Disable all leaves out a machine…」：標題的數字少 1，`ntut-vdi-win` 不在清單裡，送出的 disable 是 n-1 個，該機器仍然是 active。截圖 46。 |
| F08 | probe 沒有判斷新舊，也沒看機器是否已停用 | - 機器不是 active 時，三格都顯示「Not checked」，並附上最後一次結果的時間。<br>- active 的機器，probe 超過 15 分鐘就顯示 Unknown。後端沒變化的列每 10 分鐘才寫一次，所以門檻用 15 分鐘，留一些餘裕。<br>- `health_changed` 帶來的 probe 會更新 `last_probe_at`。 | regress 兩個 F08 案例；截圖 44（停用）、45（probe 是 1 天前的）。 |
| F09 | disable_failed 被說成 removal stuck | 新增獨立的 `disable_failed` 狀態：標籤是「Disable failed」，用紅色和三角形圖示，放在 Needs attention。橫幅寫「disable did not finish」，有自己的統計卡，詳細頁提供 Retry disable。cleanup_failed 和 stuck 才算 removal 的問題。 | regress「F09 …」；截圖 42、43。 |
| F10 | 登入後沒有回到原本的網址 | - middleware 和 API、SSE 收到 401 時，都導到 `/login?next=<目前路徑>`。<br>- `safeReturnPath` 只接受同站的 app 路徑，`//host`、`/\host`、帶 scheme 的、有控制字元的，以及 `/login` 本身，一律退回 `/`。 | regress「F10 deep link survives the login detour」；「F10 ?next cannot send you to another site」試了四種惡意寫法，結果都停在本站。 |
| daybreak P3-5 | revoke-all 收到 401 時顯示成功 | - 只有 2xx 才算成功，並顯示「Signed out everywhere」。<br>- 收到 401 時：登出本機，登入頁寫「Sign out everywhere did NOT happen」；重新登入後回到 Settings，**再跳一次確認框**，不會自動重試。<br>- 其他錯誤顯示「Nothing was revoked」。<br>- 這個 endpoint 的 401 不走全域的自動導向，由頁面自己處理。 | regress「P3-5 …」：在 401 之後 session version 沒有變；使用者再確認一次後才 +1，並顯示成功訊息。截圖 41。 |
| daybreak P3-7 | 登入預設存在 localStorage | 「Keep me signed in」預設不勾，token 存在 sessionStorage。說明文字改成「Off: you are signed out when this tab closes」。 | regress「P3-7 …」：預設 local 是空的、session 有值。截圖 40。 |
| 加分項 | 跨分頁登出 | 登出、revoke-all，或任何 401 都會寫入 `reach-signout`，同一個瀏覽器的其他分頁收到 `storage` 事件後一起回到登入頁。 | regress「sign-out in one tab signs out the other tabs」。 |

astra 要求補的回歸案例，除了「後端 revoke-all 關掉既有 stream」由後端 builder 自己的 Go 測試負責，其他都已經補上：

- snapshot/subscription 空窗 → F02
- HTTP 和 SSE 交錯 → F03 ×2
- 已打開詳細頁收到心跳後更新 → F04 ×2
- 帶 durable cursor 重連後補齊資料 → F05
- requests 單次失敗後恢復 → F06
- bulk 確認框打開期間機器重新上線 → F07

前端在撤銷之後的行為由 regress 的「revoked session…」和 `real-backend-revoke.mjs` 兩邊驗證。另外也補了危險操作完成後的狀態檢查：Remove 打錯名字按 Enter 時不會送出，打對之後送 1 個 POST，畫面變成 Retiring；409 會顯示錯誤，機器維持 Online。

### 修正後重新量測（新 bundle 搭配新版 mock）

| 項目 | 上一輪 after | 這一輪 |
|---|---|---|
| 首次載入的 API 請求 | 4（machines ×1、pending ×2、events） | **3**（machines、pending、events 各 1 次） |
| 4G 手機／桌面：機器列表出現 | 2689／2665 ms | 2674／2675 ms |
| 心跳風暴（桌面、手機）的 API 請求 | 0／0 | 0／0 |
| 真實節奏心跳的 API 請求 | 0 | 0 |
| 跳動風暴：請求數，同時最多幾個 | 193，3 | 196，3 |
| 所有情境的 long task | 0 | 0 |

先訂閱、收到 hello 才抓快照，反而少了一次 pending 抓取，載入時間持平。原始資料在 `measurements/round2-load.json` 和 `measurements/round2-storm.json`。

### 已知的限制

- 「確認之後才發生的網路競態」（F07）要等後端提供帶條件的 bulk API。
- revoke 的 e2e 用的是本機 reachd 和自己寫的 proxy，不是 jason 上的 nginx。
- 手機畫面一樣是用 Chromium 調成手機尺寸量的，沒有用真的 iPhone Safari。

---

## 刻意沒做的事，以及原因

- **nginx 的 gzip 和 cache**（A2）：要改 jason，這一輪禁止部署。量測顯示開了之後首次載入大約 −40%。
- **HttpOnly cookie**：要後端加 cookie 驗證和 CSRF，nginx 的 CSP 也要拿掉 `unsafe-inline`，屬於下一輪後端和 nginx 的工作。
- **改名核准**（Approve as 做成真的）：v2 伺服器會在 provision 時綁定 slug。如果要在核准時改名，後端要新增 API。現在先拿掉假按鈕。
- **請求歷史頁**：B 部分建議「歷史放到另一頁」。v2 伺服器已經支援 `?status=`，前端還沒做這一頁。Arthur 在營區用不到，以後要再加。
- **沒有額外做 HTML mockup**：直接在 Nuxt 裡實作，再用 mock 加截圖對照 brief。brief 要的四個手機畫面（首頁有事和沒事兩種、待核准、詳細、設定）和兩個桌面畫面都有截圖，深色和淺色兩種主題也都有。
- **Process title 的 textarea placeholder** 原本含有 emoji，已經拿掉，因為規定不要用 emoji。

## 需要 Arthur 決定的事

1. **nginx 開 gzip 和 cache**：改 jason 上的 tunnels vhost。首次載入會從 2.7 秒降到 1.6 秒。
2. **部署順序**：
   - 新前端搭配舊伺服器已經驗證可以用（after-oldsrv 那一組），所以前端可以先上。
   - 但「Sign out everywhere」、「Force retire」對 stuck 機器的處理、`observed_ip` 這幾項，要等新的 reachd 上線才完整。
   - 建議跟後端同一次部署。
3. **「Disable all N」**：打開 Dormant 群組後就能一鍵停用所有 dormant 機器，也就是 Opus P2-5 建議的做法。要不要在營區用手機按，由 Arthur 決定。

## 怎麼重跑

```bash
cd dashboard && npm ci && npx nuxt prepare && npx vitest run && npx nuxt typecheck && npm run generate
# 量測前後對照（BEFORE_SITE 是 8c74086 用 npm run generate 產生的 .output/public）
BEFORE_SITE=... AFTER_SITE=.output/public OUTDIR=/tmp/out \
  CHROME=~/Library/Caches/ms-playwright/chromium_headless_shell-1248/chrome-headless-shell-mac-arm64/chrome-headless-shell \
  test-harness/run-compare.sh all
uv run test-harness/summarize.py /tmp/out
# 截圖：先開 mock（SCENARIO=arthur HBFMT=new V2=1），再跑
SHOTS=/tmp/shots CHROME=... node test-harness/shots.mjs
SITE=.output/public SHOTS=/tmp/shots CHROME=... node test-harness/shots-round2.mjs
# 瀏覽器回歸測試（自己會開 mock）
SITE=.output/public CHROME=... node test-harness/regress.mjs
# 對真實 reachd 的 revoke e2e（reachd 要在 /tmp 用測試設定、provisioning_enabled: false）
SITE=.output/public REACHD=http://127.0.0.1:19310 USER_NAME=... PASS=... CHROME=... node test-harness/real-backend-revoke.mjs
```

`measure.mjs`／`shots.mjs` 預設從 `instagram-chat-export/node_modules` 載入 `playwright-core` 1.64.1，可以用 `PLAYWRIGHT=` 換掉。
