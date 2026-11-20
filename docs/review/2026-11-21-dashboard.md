# Dashboard 修正與重新設計（2026-11-21）

- 分支：`fix/dashboard-2026-11`，從 `8c74086` 開出。只改了 `dashboard/` 和這份報告。
- 依據：`~/30-39-Research/34-tech-research/2026-11-20-reach-dashboard-frontend.md` 的 A 表和 B 部分、兩份安全報告裡跟前端有關的項目（Opus P2-4、P2-7、P3-5；daybreak P2-1、P3-1），以及後端的 `docs/EVENTS.md`（`fix/hardening-2026-11` 的 41fbbff）。
- 新截圖：`~/30-39-Research/34-tech-research/2026-11-21-reach-dashboard-v2-shots/`
- 量測原始資料：同一個資料夾下的 `measurements/`（`before-all.json`、`after-all.json`、`after-oldsrv-all.json`、`summary.md`）
- 驗證：`npm run build` 通過，`nuxt typecheck` 0 個錯誤，`vitest` 57/57 通過（原本是 21 個，全部保留，另外新增 36 個）。

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
```

`measure.mjs`／`shots.mjs` 預設從 `instagram-chat-export/node_modules` 載入 `playwright-core` 1.64.1，可以用 `PLAYWRIGHT=` 換掉。
