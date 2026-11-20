# Reach 後端加固對照表（2026-11-21）

- 分支：`fix/hardening-2026-11`，從 `8c74086` 開出，共 20 個 commit，已 push，**沒有 merge，也沒有部署**。
- 範圍：fix-plan 第 1、2 批全部，再加上第 4 批做得到的部分。前端（`dashboard/`）不在這個範圍。
- 測試：`go test ./...` 和 `go test -race ./...` 全部通過。reach-agent 交叉編譯 linux amd64/arm64/386/arm、darwin、windows 都成功。
- 對應的審查報告：daybreak `2026-11-20-reach-security-deep-daybreak.md`、Opus `2026-11-20-reach-security-basic-opus.md`、效能 `2026-11-20-reach-perf-backend.md`。

## 對照表

| 發現 | 怎麼修 | 證明的測試 | commit |
|---|---|---|---|
| **daybreak P1-1／Opus P0-1／效能 #2**：poll、heartbeat、uninstall 每次都跑 Argon2，沒有限速 | 伺服器產生的 token（client_secret、setup、agent）改存 `sha256:`，用 constant-time 比對。舊的 Argon2 hash 驗證成功一次後，就改寫成新格式。poll 先擋限速（每 IP 60 次／分、每個 request 30 次／分），再查狀態：expired、denied、provisioned 這些已結束的狀態直接回應，完全不驗 secret。heartbeat 和 uninstall 也有每 IP、每台機器的限速。剩下的 Argon2（管理員密碼、god code、invite、舊資料）一律經過全域 2 格的 semaphore，滿了就回 503，不會排隊吃記憶體 | `token_hardening_test.go`：`TestPollDoesNotRunArgonForSHATokensOrTerminalStates`（daybreak PoC 改寫）、`TestPollRateLimitedPerRequestAndPerIP`、`TestLegacyArgonClientSecretMigratesOnSuccess`、`TestHeartbeatMigratesLegacyAgentTokenAndRejectsWrongToken`、`TestArgonGateBoundsConcurrency`、`TestHeartbeatPerMachineRateLimit` | 04759dd |
| **Opus P3-1**：帳號不存在時不跑 Argon2，可以從回應時間猜出帳號 | 帳號不存在時也驗一個假 hash | `TestLoginUnknownUserStillRunsArgon` | 04759dd |
| **daybreak P1-3（Mac）／Opus P1-3**：mac-agent 把伺服器給的整份文字原樣寫進 ssh config | 新增 `GET /api/admin/ssh-config.json`（schema 1）。舊的文字 API 保留給舊版 agent。mac-agent 只吃結構化資料：每個欄位都用嚴格的格式檢查，並套保留名稱清單，再用自己的模板產生設定（帶 `ForwardAgent no`、`ClearAllForwardings yes`）。寫檔前先跑 `ssh -G -F <暫存檔>`，失敗就保留舊檔。如果伺服器是舊版，退回解析文字，但只認 Reach 自己會輸出的 directive，看到 `Match`、`Include`、`*Command` 等就整份拒絕 | `cmd/reach-mac-agent/sshconfig_test.go`：`TestLegacyParserRejectsExecutableDirectives`（daybreak PoC）、`TestSyncFallsBackToStrictLegacyParser`、`TestSyncUsesJSONEndpointAndNeverWritesServerText`、`TestValidateHostRejectsBadValues`。另外實測過：新 mac-agent 接舊版 reachd 8c74086 可以正常同步 | 996272d |
| **Opus P1-1**：`client_options` 的值沒檢查，塞一個不存在的 cipher 就能讓 Mac 上所有 ssh 都壞掉 | 伺服器端：值必須符合演算法清單的格式 `^[+\-^]?[a-z0-9@.\-]+(,…)*$`，同一個選項只收一次。Mac 端：同樣的格式檢查，加上 `ssh -G` 驗證，所以格式正確但實際不存在的演算法名稱也會被擋下，舊檔保留 | `TestSSHConfigClientOptionsRejectMalformedValues`（Opus 的 PoC 值）、`TestInstallKeepsPreviousFileWhenSSHRejectsConfig`（`bogus-cipher-xyz`） | 996272d |
| **效能 #4**：機器狀態一抖，ssh config 就把它整段拿掉 | 一律輸出 desired=active 的機器，狀態寫在註解裡。資料有問題的那一列只跳過，不會讓整份設定失敗 | `TestSSHConfigKeepsActiveMachinesWhileOffline`、`TestSSHConfigSkipsBadRowInsteadOfFailing` | 996272d |
| **daybreak P3-2**：產生的 stanza 沒有強制關掉 agent forwarding | 模板固定加 `ForwardAgent no`、`ForwardX11 no`、`ClearAllForwardings yes` | `TestRenderedConfigUsesFixedTemplate` | 996272d |
| **Opus P1-2**：slug 可以在 provision 時換成 `jason-public` | slug 在 register 時就決定（沒給就用正規化後的名稱），provision 時必須完全相同。另外加保留名稱黑名單（`jason*`、`*-public`、`pi`、`pi-*`、`localhost`、`reach-hub`、`hub*`、`github*`……、hub 自己的 ProxyJump alias，以及 config 的 `reserved_slugs`）。slug 已被使用的話，register 時就回 409 | `binding_test.go`：`TestProvisionSlugMustMatchApproval`（Opus PoC）、`TestReservedSlugsRejected`、`TestDuplicateSlugRejectedBeforeTokenIsSpent` | b8b1711、073be12 |
| **Opus P2-4／daybreak P2-1**：metadata 可以偽造 source_ip；核准後可以把欄位清成空值 | 先放 client 的 metadata，伺服器自己的欄位最後寫，會覆蓋過去。新增 `requests.observed_ip` 欄位。`validateApprovedMetadata` 改成 `approvedBinding`：讀 DB 或解析 JSON 失敗時拒絕 provision（fail closed）。核准時有填的 target_user、mode、local_port 不能被改掉，也不能被清空 | `TestProvisionCannotClearApprovedFields`（daybreak PoC）、`TestProvisionFailsClosedOnCorruptMetadata`、`TestRequestsListExposesObservedIPAndFilter` | b8b1711 |
| **daybreak P2-2／Opus P2-2**：公鑰可以多塞一行 | 只收一行 `<type> <base64> [comment]`，不准有 options，也不准有 CR、LF、NUL。寫入前用 `ssh.MarshalAuthorizedKey` 重新產生。`authorizedKeyLine` 自己也會再檢查一次，舊 DB 裡的資料也一樣擋得住 | `TestPublicKeyMustBeSingleCanonicalLine`、`TestProvisionRejectsMultiLineKey` | b8b1711 |
| **Opus P2-3**：rt-* 帳號可以建立 Unix socket | Match 區塊加上 `AllowStreamLocalForwarding no` 和 `PermitUserRC no`。本機用 OpenSSH 10.3 跑 `sshd -t` 驗證過語法 | `TestSSHDMatchBlockDisablesStreamLocalAndUserRC` | b8b1711 |
| **Opus P3-3**：provision 出錯時把錯誤細節回給 client | 細節只寫進 log，client 只看到通用的錯誤訊息 | 程式碼審查（`handleClientProvision`） | b8b1711 |
| **效能 #3**：heartbeat 被存成永久事件；`agent_status` 是死寫入；重播窗口只能撐約 1 小時 | heartbeat 改成 `PublishLive`：沒有 id、不寫 DB，payload 帶 `machine_id`、`heartbeat_at`、`observed_state`、`status`。拿掉 `agent_status` 的寫入。`tunnels.last_seen_at` 改成狀態變了或超過 5 分鐘才寫。重播改成 72 小時的時間窗口，記憶體上限 5000 筆。格式寫在 `docs/EVENTS.md` | `TestHeartbeatIsLiveOnlyEventWithoutDurableRow`、`TestReplayWindowIsTimeBased`、`TestEventReplayOverflowRequiresResnapshot` | 0f7e049、41fbbff |
| **daybreak P2-5（heartbeat 部分）** | heartbeat body 上限 128 KiB；`command_results` 最多 32 筆，message 會截斷；只有 RowsAffected=1 而且是這台機器自己的 pending/sent → acked/failed 才會寫入並 publish | `TestCommandResultsPublishOnlyRealTransitions`、`TestHeartbeatBodyIsBounded` | 0f7e049 |
| **daybreak P1-2**：disable／retire 的 cleanup 失敗時照樣回報成功（fail-open），還會釋放 port | 系統操作集中到 `tunnelOS` 介面，每個步驟出錯都會回傳錯誤；`killUser` 在 SIGKILL 之後程序還活著就算失敗。disable 失敗會設成 `disable_failed`。retire 失敗會設成 `cleanup_failed`：tunnel 那一列保留成 tombstone，port 和 Unix user 都不釋放，按 retire-now 可以重試。只有 `Residue()` 確認 user、home、drop-in、程序都不存在了，retire 才算完成。admin API 失敗時回 409。uninstall-complete 只有在真的清乾淨後才會寫 `complete`。allocator 也會跳過現存 sshd drop-in 裡用到的 port，以及「舊版 retire、還沒驗證清乾淨」的 port；這些舊資料由 maintenance 逐一驗證 | `retire_test.go`：`TestRetireFailsClosedAndKeepsPortTombstone`（daybreak PoC 改寫成 fake OS）、`TestRetireRequiresVerifiedResidueFree`、`TestDisableFailsClosed`、`TestUninstallCompleteOnlyCompletesAfterCleanup` | e86a0e1 |
| **daybreak P2-5（撤銷 token）** | retire 時把 `agent_token_hash` 清成 NULL，並取消還沒完成的 command。migration 會把已經 retired 的舊機器也清掉 token | `TestRetireRevokesAgentToken` | e86a0e1 |
| **效能 #7**：過期的 command 沒標記；retiring 會永遠卡住；ListMachines 帶出所有 retired | maintenance 會把過期的 command 標成 `expired`。retiring 超過 `retiring_timeout`（預設 24 小時）就標成 `timed_out`，並發出 `machine.retiring_timed_out`，**不會自動 retire**，由管理者按 retire-now 決定。`/api/admin/machines` 預設不回傳 retired，要看的話加 `?include_retired=1` | `TestMaintenanceExpiresCommandsAndFlagsStuckRetiring`、`TestListMachinesHidesRetiredByDefault` | e86a0e1 |
| **效能 #5**：9205 被兩個 hub 列分配了兩次 | allocator 改成看所有 hub 列的 port，不再按 hub 分開算。reachd 啟動時，如果發現既有資料裡有重複的 port，會寫一行 log 警告 | `TestAllocatorSkipsDropInPortsAndOtherHubs` | e86a0e1、54e00a4 |
| **daybreak P1-4**：invite 有競態，可以重複使用 | register 時在同一個交易裡做 `UPDATE invites SET claimed_by_request_id=? WHERE … used_by IS NULL AND claimed_by_request_id IS NULL`，而且必須 RowsAffected=1。provision 只消耗自己 claim 到的那一筆，不會覆寫 `used_by`。request 過期或被拒絕時，claim 會被釋放 | `invite_test.go`：`TestInviteIsClaimedAtomically`（4 個併發只有 1 個成功）、`TestExpiredInviteClaimIsReleased` | 8ae76a8 |
| **效能 #5、#6**：health check 每條隧道各 fork 一次 `ss`，每輪寫 2 次 DB | 每輪只跑一次 `ss -Htln` 再自己解析。keyscan 只在剛開始 listen 時跑，或距離上次成功超過 5 分鐘才跑。只有狀態改變時才寫 DB（另外定期 refresh：一般 10 分鐘一次，dormant 的機器 1 小時一次） | `health_test.go`：`TestHealthCheckBatchesAndSkipsUnchangedWrites`、`TestParseSSListeners` | 54e00a4 |
| **效能 #6、#8**：`last_used_at` 每個請求都寫；`MIN/MAX` 是全表掃描；manifest 每次 heartbeat 都讀檔 | `last_used_at` 改成最多 1 分鐘寫一次。`MIN/MAX` 改成兩個子查詢。manifest 用 mtime 加 size 判斷要不要重讀，另有 10 秒 TTL 的快取 | `TestServiceTokenLastUsedIsThrottled`、`TestManifestIsCached` | 54e00a4、0f7e049 |
| **效能 #7（agent）**：hbTicker 從來不 Reset | 收到新的 interval 就 Reset，範圍限制在 10 秒到 10 分鐘 | `TestHeartbeatIntervalIsClampedAndApplied` | 4ad398a |
| **daybreak P2-3**：root agent 會跟著 symlink 寫 `authorized_keys` | Unix 上新增一個統一的 writer：home、.ssh、authorized_keys 這三層都用 `openat(O_NOFOLLOW)` 開，遇到 symlink 或非一般檔案就拒絕。在已驗證的目錄 fd 裡建 temp 檔，再 `renameat` 換上去，只 fchown 自己建立的 inode。拿掉了 `chown -R`。install、sync、uninstall、讀 fingerprint 全部共用這個 writer。Windows 維持原本的 ACL 做法 | `cmd/reach-agent/authkeys_unix_test.go`：`TestInstallAdminKeysRefusesAuthorizedKeysSymlink`（daybreak PoC）、`TestSyncKeysRefusesSymlinkedSSHDir`、`TestAuthorizedKeysWriterKeepsOtherLinesAndCreatesDir` | 4ad398a |
| **daybreak P2-6**：signed update 可以被降版 | manifest 加上簽過名的單調 `sequence`；舊的 manifest 改用已簽名的 `created_at`。agent 在 root 擁有的 data dir 記錄 floor，比 floor 舊的一律拒絕。floor 要等更新被 **confirm** 之後才會提高，所以還沒 confirm 的版本仍然可以 rollback。本機可以用 `--allow-downgrade` 破例，遠端 command 無法帶這個旗標。`verify-release-artifacts.py` 也會要求 manifest 有 sequence | `cmd/reach-agent/antirollback_test.go`（3 個測試） | 0b404a7 |
| **daybreak P1-5／Opus P3-4**：setup 腳本只驗同一個來源的 checksum | setup.sh 會依序嘗試：(1) 有 `minisign` 就用它驗；(2) 有 OpenSSL 3 就自己驗 Ed25519，訊息簽章和 trusted comment 的簽章都驗，也比對 key id；(3) 兩者都沒有，就比對 deploy 時寫進腳本的 hash；(4) 都不行就拒絕，除非設 `REACH_ALLOW_UNVERIFIED=1`。驗過簽章之後，還會從簽過名的 manifest 檢查 project、version、size、sha256。下載限 https，並有大小上限。`--repair --update-agent` 改成由已安裝的 agent 自己更新。setup.ps1：PowerShell 5.1 沒有 Ed25519，所以只信 pinned hash | 本機端到端測試（假 hub 加測試金鑰）：正確的版本通過；換掉 binary、改掉 manifest、改掉 trusted comment 三種都被拒絕；沒有任何驗證工具時拒絕；設了 opt-out 才放行。PowerShell 沒有實測（Mac 上沒有 pwsh） | a6596c4 |
| **daybreak P3-1／Opus P2-7**：JWT 一年有效，而且沒辦法全部登出 | 預設 7 天，最長 30 天。JWT 帶 `sv`（`users.session_version`）；呼叫 `POST /api/admin/sessions/revoke-all` 會把這個值加一，可以選擇連 service token 一起撤銷 | `session_test.go`：`TestRevokeAllSessionsInvalidatesExistingJWTs`、`TestServiceTokenCannotRevokeSessions`、`TestAdminJWTTTLIsCapped` | a3d7da9 |
| **Opus P3-2**：CORS 允許任何 localhost 來源 | 只允許 api_url 的 origin，以及 `allowed_origins` 裡列出的來源 | `TestCORSRejectsArbitraryLocalhost` | a3d7da9 |
| **daybreak P2-4**：carrier 沒有上限，任何 path 都收 | 先驗 upgrade 和精確的 path prefix（`REACH_WS_PATH_PREFIX`，放在 EnvironmentFile），通過才 dial sshd。全域連線上限 64，每個 IP 上限 8；閒置 10 分鐘就斷線；限制 header 大小；frame 改成每次 32 KiB 串流，不再一次配置整個 frame。systemd unit 加 `MemoryMax=256M` 和 `TasksMax` | `internal/wscarrier/limits_test.go`：exact path（錯的 path 不會 dial backend）、每 IP 上限、慢速大 frame 的 heap 測試（**換回舊的 pump 會失敗**）、閒置斷線 | 2418d51、d8f8152 |
| **daybreak P3-3**：HTTP server 沒有 timeout | 加上 `ReadTimeout`、`IdleTimeout`、`MaxHeaderBytes`。SSE 需要長連線，所以不設 WriteTimeout | 程式碼審查 | 522102c |
| **Opus 方案 A／daybreak sandbox** | `deploy/systemd/reachd.service`：維持 root，但加 `ProtectSystem=strict`、capability 白名單、`MemoryMax=1G`、`TasksMax=256`、`CPUQuota`。只有設了 `REACH_INSTALL_REACHD_UNIT=1` 才會安裝；`deploy-from-mac.sh` 會把這個變數傳下去。安裝前跑 `systemd-analyze verify`，失敗會還原 | Linux 沒有實測（`systemd-analyze` 只能在 Linux 跑，部署腳本裡會先跑這一步） | d8f8152 |
| **Opus P0-1 #5／前端 A#2／Opus P3-6**：nginx | `deploy/nginx/reach-tunnels.conf`：開 gzip、`/_nuxt/` immutable 快取、index.html 和 setup 腳本設 no-cache、`/api/client/` 和 login 加 `limit_req`、carrier 加 `limit_conn`、`server_tokens off` | 在 Mac 用 `nginx -t` 驗證語法通過 | d8f8152 |
| **Opus P1-4**：HAProxy 看得到真實 IP，sshd 看不到 | `deploy/haproxy/2026-11-21-bk_ssh-conn-limit.suggested.diff`：在 SSH 分支加 stick-table，同一個 IP 同時超過 10 條，或 10 秒內超過 30 條就拒絕。**只是建議檔，沒有套用** | 在 Mac 用 `haproxy -c` 驗證語法通過（3.4 版；jason 是 2.8，用到的語法 2.8 也支援） | d8f8152 |
| 既有測試 `TestHeartbeatRetainsResultsUntilSuccess` 壞掉 | **是測試寫錯，程式沒有 bug。** 測試會把打到 httptest server 的每個請求都算進去，但本機有程序（node）會掃新開的 loopback port，送一個 `GET /`，被算成一次失敗的 heartbeat。改成只算 POST heartbeat | 連跑 3 次都通過 | 95cb421 |
| 既有測試 `TestCarrierEcho` 有 data race（8c74086 就有） | 是測試本身的問題：goroutine 寫 bytes.Buffer 的同時，測試在讀。改用加鎖的 buffer | `go test -race ./...` 全部通過 | e2d6366 |

## 刻意不修，或只寫設計

| 項目 | 原因 | 文件 |
|---|---|---|
| sync_keys 加簽章（daybreak P1-3 的 target 端、Opus P2-6） | 要新增一把離線金鑰、Mac 上的簽章流程，以及每台 target 的遷移計畫，工作量太大 | `docs/design/2026-11-21-signed-sync-keys.md` |
| reachd 拆成非 root API 加 root helper（方案 B） | 依指示只寫設計。RPC 介面直接沿用這次新增的 `tunnelOS` | `docs/design/2026-11-21-reachd-privilege-split.md` |
| carrier 改用 per-machine token（daybreak P2-4 的認證部分） | 這次只做了 exact path 和資源上限；共用的 path 仍然只是一個弱 secret | 寫在這張表 |
| god code 改成短效（daybreak P2-7／Opus P2-1） | 不在指定範圍。Arthur 的第 0 批清單裡是直接清空 `god_code_hash` | |
| 收集 target host key（daybreak P3-2 的 TOFU 部分） | 不在範圍內；這次只做了 ForwardAgent 和 port tombstone，避免冒充 | |
| 把 `/var/lib/rt-*` 搬到專用目錄 | 要搬家目錄，和方案 B 一起做 | 方案 B 設計 |

## 相容性（本機實測）

- 舊 DB（Argon2 agent token）接新 reachd：第一次 heartbeat 回 200，hash 被改寫成 `sha256:`。
- 舊 JWT（沒有 `sv` claim）接新 reachd：仍然有效，因為 `sv=0` 等於預設值。
- 新 mac-agent 接舊 reachd 8c74086：退回解析文字，可以正常同步。
- 舊 mac-agent 接新 reachd：文字 API 保留，可以用。Mac 上現在的 token 是 `sat_` 開頭的 service token，不受影響。
- 舊 agent：heartbeat、poll、provision 的格式都沒變。新版 agent 在 register 時會多帶 slug、mode、local_port，舊版不帶也能用：沒帶 slug 時，綁定的是正規化後的名稱，跟舊版 agent 自己算出來的一樣。

## 要 Arthur 決定

1. **部署時機**：要在家時部署，或確定 jason 有另一條路可以連（Headscale）時再部署。部署前後都要確認 omarchy 和 jack 的狀態。
2. **sandbox unit**：預設不裝。要先在 VM 或有退路的時候，帶 `REACH_INSTALL_REACHD_UNIT=1` 實際測一次「新增、停用、退役」。
3. **nginx 範本、HAProxy 建議 diff**：都要手動套用。HAProxy 是公網唯一的入口，套之前請先看過。營區如果是很多人共用一個 IP（CGNAT），可能需要把數字調高。
4. **carrier 的 exact path**：要在 jason 建 `/etc/reach/ws-carrier.env`，內容 `REACH_WS_PATH_PREFIX=/ws/tunnel/<secret>/`。不建也能跑，只是靠 nginx 擋路徑。
5. **9205 重複**：jason 的 DB 裡 `reach-test`（hub jason）和 `gaming-pc`（hub primary）兩台都用 9205，而且都是 active 加 gone。建議退役其中一台（新版會在 log 裡警告）。
6. **jason 上 9 台 retiring 的機器**：部署 24 小時後會被標成 `timed_out`，dashboard 會顯示，要由你按 retire-now。retire-now 現在會真的驗證是否清乾淨，失敗的話會顯示 `cleanup_failed`。
7. **revoke-all 會讓 mac-agent 斷線**：如果按「Sign out everywhere」時連 service token 一起撤銷，Mac 上要重新拿 token。
