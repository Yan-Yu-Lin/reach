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
| **astra F01（審 dashboard 時發現）**：revoke-all 之後，已經開著的 SSE 還會繼續收資料 | SSE loop 在每次送事件和 keepalive 之前，都重新驗證 token（簽章、expiry、blacklist、session_version、service token 是否撤銷）。另外，logout、revoke-all、撤銷 service token 時會廣播通知，所有 stream 立刻重新檢查，沒通過就直接關掉，不送任何資料 | `sse_revoke_test.go`：`TestRevokeAllClosesExistingEventStream`（astra 的 PoC 改寫；把檢查拿掉測試就會失敗）、`TestEventStreamRechecksTokenBeforeEachEvent`、`TestRevokedServiceTokenStreamCloses`、`TestLogoutClosesOwnStreamButNotOthers` | （見 git log） |
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
| carrier 改用 per-machine token（daybreak P2-4 的認證部分） | 這次只做了 exact path、資源上限、沒 path 不啟動；共用的 path 仍然只是一個弱 secret | `docs/design/2026-11-21-carrier-credentials.md` |
| god code 改成短效（daybreak P2-7／Opus P2-1） | 不在指定範圍。Arthur 的第 0 批清單裡是直接清空 `god_code_hash` | |
| 收集 target host key（daybreak P3-2 的 TOFU 部分） | 不在範圍內；這次只做了 ForwardAgent 和 port tombstone，避免冒充 | |
| 把 `/var/lib/rt-*` 搬到專用目錄 | 要搬家目錄，和方案 B 一起做 | 方案 B 設計 |

## 相容性（本機實測，2026-11-21 更新）

- 舊 DB（Argon2 agent token）接新 reachd：第一次 heartbeat 回 200，hash 被改寫成 `sha256:`。
- **舊 JWT（沒有 `sv`）接新 reachd：會失效（401），所有人都要重新登入一次**（daybreak P1-3）。Mac 的 `sat_` service token 不受影響。
- 新 mac-agent 接舊 reachd 8c74086：退回解析文字，可以正常同步。
- 舊 mac-agent 接新 reachd：文字 API 保留，可以用，但預設不再帶 target 回報的演算法選項（`legacy_ssh_config_options: false`）。
- 舊 agent：heartbeat、poll、provision 的格式都沒變。部署前就已建立、但還沒 provision 的 request 會收到 410，要重新跑 setup（最多影響 request_ttl 30 分鐘內的請求）。
- **新安裝、而且機器上沒有 sshd 的 target，會改用 agent 內建的 internal-sshd**（只聽 loopback；沒有 sftp；system 安裝時 session 以 target user 身分跑）。已經裝好的機器不受影響。Windows 系統安裝如果還沒裝 OpenSSH Server，要加 `-InstallOpenSSH` 才會裝。
- 部署 rollback 在 Ubuntu 24.04 container 裡實測過兩種情境（`scripts/test-rollback-in-container.sh`）。

## 要 Arthur 決定（2026-11-21 更新）

1. **部署時機**：要在家時部署，或確定 Headscale 這條路可用時再部署。部署前後都要從外面實際 `ssh omarchy`、`ssh jack` 一次，不能只看 DB 狀態。
2. **分階段，不要同一天全部切換**（見下方「部署順序」）。
3. **9 台 retiring 的機器會在新版第一次 maintenance 就變成 `timed_out`**，不是 24 小時後，因為它們從 6～7 月就在 retiring 了。這只是標記，不會自動退役。dashboard（`fix/dashboard-2026-11`）已經有 Force retire（retire-now）的按鈕，要一台一台確認後再按。
4. **port 重複不只 9205**：jason DB 目前 9200～9206 和 9208 都各有兩筆（一筆 hub `jason`、一筆 hub `primary`），大多是舊的 disabled／retiring 那一筆。**不准批次 retire**，要逐筆確認哪一台是真的在用：
   - 9206：`ntut-vpn-help`（primary）在線；`schoollab29`（jason）是 retiring。
   - 9208：`omarchy`（primary）在線；`box-vm`（jason）是 disabled。
   - 9205：`reach-test`（jason，DGX，屬於資策會範圍）和 `gaming-pc`（primary）都 active 但都不在線。
   - 9200～9204：jason 那筆全是 retiring；primary 那筆有 active 也有 retiring。
   新版 allocator 不會再分配出重複的 port，啟動時也會把這些重複寫進 log。
5. **sandbox unit 還是 opt-in**；資源上限 drop-in（MemoryMax 等）現在是預設會裝。兩者都不是 root containment。
6. **nginx 範本、HAProxy 建議 diff 都要手動套用**。HAProxy 是公網唯一的入口，套之前請先看過。營區如果是 CGNAT，數字可能要調高；nginx `/api/client/` 2r/s burst 20 在 31 台同時重連時可能先回 429，agent 會重試。
7. **carrier 現在沒有 path 就不啟動**：deploy 會從 config 的 `websocket_tunnel.path_prefix` 自動寫 `/etc/reach/ws-carrier.env`。目前有 2 台用 websocket transport。
8. **revoke-all 會讓 mac-agent 斷線**：如果勾「連 service token 一起撤銷」，Mac 上要重新拿 token。
9. **rollback 規則**：部署失敗時，如果新版上線後沒有「不能丟的寫入」（provision、approve、disable、retire、invite、service token、登出、revoke），就自動把 DB snapshot 和舊 binary 一起還原；有的話，reachd 會停著，等人決定。heartbeat、token 遷移、observation 這些可丟的寫入不會擋自動回滾。

## 部署順序（建議，每一步之間至少觀察一天）

1. **reachd + 資源上限 drop-in**（`deploy-from-mac.sh`，不帶 `REACH_INSTALL_REACHD_UNIT`）。會先在 snapshot 副本上演練 rollback。部署後：從外面 `ssh omarchy`、`ssh jack`；dashboard 重新登入；確認 carrier 有起來（2 台 websocket 機器有連上）。
2. **Mac 上換新的 reach-mac-agent**（結構化 ssh config）。確認 `~/.ssh/reach-tunnels.conf` 內容正確、`ssh -G jack` 正常。
3. **dashboard 前端**（`fix/dashboard-2026-11`），可以和第 1 步同一天，但在 reachd 之後。
4. **nginx 範本**：先在 jason 上 `nginx -t`（1.24），再 reload。觀察 429。
5. **HAProxy 建議 diff**：Arthur 看過後才套；套完從外面測 SSH 和 HTTPS。
6. **sandbox unit**（`REACH_INSTALL_REACHD_UNIT=1`）：只在有退路的時候，並實際做一次 新增→停用→啟用→退役。
7. **agent release**：新的 agent（anti-rollback floor、internal-sshd run-as、Windows ACL）透過 update policy 逐台更新，先從可以實體救回的機器開始。

## astra round 1（審 17707dc，2026-11-21）

| 編號 | 發現 | 怎麼修 | 證明（測試／實測；都做過 mutation 驗證） | commit |
|---|---|---|---|---|
| P1-01 | rollback 只換回 binary，舊版不認得已遷移的 sha256 token；readiness 只看 TCP | 部署前在 snapshot 的**私有副本**上演練 rollback（canary 只放在副本裡，正式 DB 不新增任何東西）；`deploy-fence` 記錄高水位，失敗時區分可丟／不可丟寫入；沒有不可丟寫入才把 DB snapshot 和舊 binary 一起還原（保留 forensic copy、清 WAL/SHM），否則停著等人；readiness 改成 TCP 加一次有認證的 `/api/admin/health` | `deploy_fence_test.go`（用真 handler 分類每一種寫入）、`scripts/rehearse-rollback.sh`（本機：binary-only 回滾 401、還原後 200）、`scripts/test-rollback-in-container.sh`（Ubuntu 24.04 跑真的 `rollback_reachd`：情境 A 自動還原、舊版 heartbeat 200；情境 B 有 disable 就停著） | 3646d23、8cd316f、ee377d3 |
| P1-02 | Windows 原子替換丟掉 authorized_keys 的 DACL | `replaceWithACL`：先建空 temp、先套 DACL、再寫內容、再 rename；system 安裝 ACL 失敗就不換檔 | `authkeys_acl_test.go`（順序、strict 失敗不換檔、icacls 參數）。**沒有在 Windows 實測** | c9a3144 |
| P2-01 | heartbeat 把 disable_failed／cleanup_failed 蓋回 disabled／retiring | `effectiveStatus` 集中推導，失敗狀態優先；recompute 的 UPDATE 加上 desired/cleanup CAS | `TestHeartbeatKeepsCleanupFailureStatus`、`TestEffectiveStatusPrecedence` | eaa0fe6 |
| P2-02 | 先前 disable 失敗，uninstall-intent 卻回 200 | 只有真的撤銷成功才算冪等；disable_failed 重試 remove、cleanup_failed 重試 retire、查詢錯誤也算錯 | `TestUninstallIntentRetriesPriorDisableFailure` | eaa0fe6 |
| P2-03 | confirm 吞掉 floor 錯誤卻取消 rollback；watcher 和 confirm 有競態 | confirm 和 watcher 用 rename 原子 claim 同一個 flag；floor 讀寫失敗就把 flag 放回去並回錯 | `TestConfirmKeepsRollbackArmedWhenFloorFails`、`TestConfirmAndRollbackClaimExclusively` | 289fe0d |
| P2-04 | legacy timestamp floor 和任意 sequence 不在同一個排序空間 | sequence 定義為 Unix 秒（≥1e9）；小數字直接拒絕；deploy 驗證新 sequence 必須大於已發布的 | `TestSequenceSpaceIsUnixSeconds`；`verify-release-artifacts.py --previous-manifest` 實測 | 289fe0d、4776166 |
| P2-05 | authorized_keys 超過 4 MiB 會被截斷寫回 | 多讀 1 byte，超過就回錯、不動原檔 | `TestOversizeAuthorizedKeysIsRefusedNotTruncated` | 6655055 |
| P2-06 | CentOS 6 的 curl 不支援 `--proto` | 偵測 curl 支援的參數；舊 curl 不跟隨 redirect，下載後自己檢查大小 | `scripts/test-setup-bootstrap.sh`（模擬 7.19.7 的 stub）。**沒有 CentOS 6 實機** | 518aa41 |
| P2-07 | nginx 1.24 不支援 `http2 on;` | 改成 `listen ... ssl http2 proxy_protocol;` | 依官方文件；1.24 實機還沒 `nginx -t` | b928e19 |
| P2-08 | 過期 command 的結果看 maintenance 和 heartbeat 誰先跑 | result 的 UPDATE 加上 `expires_at > now` | `TestExpiredCommandResultIndependentOfMaintenanceOrder` | fcb7a19 |
| P2-09 | 過期的 setup token 回 503 busy | 過期 410、用過或錯的 401、只有 Argon2 gate 滿才 503、其他錯 500 並記 log | `TestExpiredSetupTokenIsTerminal`、`TestUsedSetupTokenIsUnauthorizedNotBusy` | fcb7a19 |
| P2-10 | Mac 的保留名稱和 server 不一致；沒擋 Host==ProxyJump | 共用 `internal/reserved`；Mac 擋自我 ProxyJump；可在 mac-agent.yaml 加 `reserved_hosts` | `TestMacRejectsEveryServerReservedAlias`、`internal/reserved` 測試 | 2139d7e |
| P2-11 | setup 的 localhost 例外只比前綴 | 解析 URL，http 只接受**完全等於** localhost／127.0.0.1／[::1]；拒絕 userinfo；每次下載都再驗 | `scripts/test-setup-bootstrap.sh`（24 案例）；setup.ps1 用 `[Uri]` 解析，**沒有實際跑 PowerShell** | 518aa41 |
| P2-12 | opt-in unit 原本不存在時 rollback 不會移除 | 記錄 mutated 狀態；原本沒有 override 就把這次裝的移走 | 靜態審查；Linux 故障注入沒做 | 3646d23 |
| P3-01 | health 不寫入的測試抓不到每輪都寫 | 改用 refresh window 內的 sentinel 時間，並另外測過期一定會 refresh | 「每輪都寫」和「從不 refresh」兩個 mutant 都會讓測試失敗 | 523d6a6 |
| 部署說明 | 9 台 retiring 立刻 timed_out、port 重複、前端已有 Force retire、分階段 | 見上方「要 Arthur 決定」和「部署順序」 | jason 唯讀查詢 | 本 commit |

## daybreak round 2（審 17707dc，2026-11-21）

| 編號 | 發現 | 怎麼修 | 證明 | commit |
|---|---|---|---|---|
| P1-1 | machine 狀態轉移沒有序列化，可能 DB 說 retiring 但 key 還開著 | per-machine lock，lock 內重讀；每個 UPDATE 都對開始時的 generation 做 CAS（RowsAffected=1）；enable 的 OS 成功但 DB 記錄失敗就補償 DisableAuth | `transition_race_test.go`（blocking fake OS：enable vs uninstall、enable vs disable、OS 成功後 DB 失敗）；拿掉 lock／CAS／補償任一項都會失敗 | 3b02f1e |
| P1-2 | `reach-sync-mac.sh` 把 raw config 原樣寫進 Include | 腳本改成只負責登入，然後呼叫 `reach-mac-agent run --once`；README 不再推薦 raw endpoint；raw endpoint 保留，但預設不帶 target 選項（`legacy_ssh_config_options`） | `scripts/test-reach-sync-mac.sh`（假 API 回 `Match exec`；舊腳本失敗、新腳本通過且 marker 沒被建立）、`TestLegacyTextOmitsTargetOptionsByDefault` | bc936e2 |
| P1-3 | 升級前一年期的 JWT 仍有效 | migration 加入 `session_version` 欄位時，所有帳號 +1 | `TestMigrationInvalidatesPreUpgradeJWTs`（舊 token 401、`sat_` 200、新登入 200、重跑 migration 不會再登出） | 30edf19 |
| P1-4 | 一台 target 用假演算法讓整個 Mac fleet 卡住 | server 只收固定的相容 profile（官方 agent 會送的那幾組）；Mac 端 `ssh -G` 失敗就拿掉所有 per-host options 再試，成功就安裝 | `TestBadHostOptionsDoNotFreezeFleet`、`TestSSHConfigClientOptionsRejectMalformedValues` | 820ecb4 |
| P1-5 | system 安裝會開全介面的 port 22 | 只沿用已存在的 sshd（listen 超出 loopback 時警告）；沒有就用 internal-sshd（loopback），root agent 以 target user 身分跑 session；daemon 只 start 已 enabled 的 sshd；Windows 要 `--install-openssh` 才裝 OpenSSH Server | `sshd_posture_test.go`、`TestRunAsDropsPrivilegesWhenRoot`（Linux container 以 root 實測降權成功）。完整方案見 `docs/design/2026-11-21-loopback-sshd.md` | 187443b |
| P1-6 | 資源上限 unit 是 opt-in | `reachd.service.d/10-reach-limits.conf`（MemoryMax 1G、TasksMax、LimitNOFILE）每次部署都裝，有備份和回滾；文件寫明這不是 root containment | `systemd-analyze verify`（Ubuntu 24.04 container） | 63767c0 |
| P2-1 | fresh install 沒有 anti-rollback floor | 部署時決定一個 release sequence，寫進 binary（ldflags）和簽章 manifest；agent 啟動時把 floor 提高到自己的 sequence（等待 confirm 時不提高） | `TestFreshInstallSeedsFloorFromEmbeddedRelease` | 2b0cd89 |
| P2-2 | `--candidate` 先 hash 後執行同一路徑 | 先 O_NOFOLLOW 複製到 0700 私有目錄，只驗證和執行那份複本 | `TestCandidateIsCopiedPrivately` | d779727 |
| P2-3 | 核准時沒填的欄位等於萬用字元 | register 時就把預設值（root／system／22）寫進 binding；provision 每欄必須完全相同；舊格式 request 回 410 要重新註冊 | `TestOmittedFieldsAreBoundToDefaults`、`TestLegacyRequestWithoutBindingMustReregister` | 0b26af5 |
| P2-4 | 過期 request 還能 approve、poll 拿 token | approve 要求 `expires_at>now`；poll 對 approved 也檢查 TTL；consume 再檢查一次；setup token 到期時間不超過 request 到期 | `TestExpiredRequestCannotBeApprovedOrPolled`、`TestSetupTokenExpiryClampedToRequest` | 0b26af5 |
| P2-5 | 用錯的 secret 就能把真的 installer／agent 鎖在限速外 | 失敗次數改算在 (IP, 物件) bucket；per-request／per-machine 配額只在認證成功後扣；uninstall 有自己的 bucket | `TestWrongSecretsDoNotLockOutLegitimatePoller`、`TestWrongAgentTokensDoNotLockOutAgent` | d2f3797 |
| P2-6 | 兩筆 live request 可以拿同一個 slug | `requests.slug` + partial unique index（pending／approved／provisioning）；deny／expire 釋放；register 時就回 409 | `TestSlugReservedAcrossLiveRequests` | 1761750 |
| P2-7 | provision 中途 crash，request 永遠卡在 provisioning | 記錄 `provisioning_started_at` 和建立的 machine；maintenance 對超過 10 分鐘 lease 的 request 設 expired、machine 設 failed（port 保留），invite claim 釋放 | `TestStuckProvisioningIsRecovered` | 58d27e6 |
| P2-8 | agent 接受多行 admin key | agent 自己 parse 並 canonicalize 每一把（單行、無 options、無第二把 key）；comment 不能偽造 `reach:` marker | `TestAgentRejectsMultiLineAdminKey`（install 和 sync 兩條路） | 526d310 |
| P2-9 | Windows SYSTEM agent 跟隨 junction | 寫入前和 rename 前都檢查 home／.ssh／authorized_keys 不是 symlink 或 reparse point | `authkeys_reparse_test.go`。**沒有在 Windows 實測**；剩下的 check-to-rename race 和 handle-based 設計見 `docs/design/2026-11-21-windows-keyfile-traversal.md` | 65acab2 |
| P2-10 | localhost 前綴繞過；agent 本身不驗 URL | 同 astra P2-11，另外 agent binary（install、update、heartbeat）也解析驗證 | `TestValidateAPIURL` | 518aa41、ac9a6a4 |
| P2-11 | carrier 用全 fleet 共用 path 當認證 | **部分**：path 改成完全相符、沒設 path 就不啟動、deploy 從 config 自動寫 env。per-machine credential 只寫設計 | `TestCarrierEnforcesExactPathBeforeDial`（含 `/secret/attacker-chosen-suffix`）、`TestAgentPathMatchesExactCarrierPath`；設計 `docs/design/2026-11-21-carrier-credentials.md` | 178aad0、f1b2a69 |
| P2-12 | SSE 沒有連線數和 replay 大小上限 | 全域 64、每 IP 8、每個 admin 憑證 8；mac-agent token 只能有一條（新的取代舊的）；replay 加 4 MiB 上限；nginx 範本加 limit_conn | `TestMacAgentTokenGetsOneStream`、`TestEventStreamsArePerIPCapped`、`TestReplayByteCapForcesResnapshot` | b4dd8b5 |
| P2-13 | bootstrap script 本身由 jason 提供 | 只寫設計（從 GitHub release 發布 setup 腳本） | `docs/design/2026-11-21-bootstrap-trust-root.md` | f1b2a69 |
| P3-1 | Mac 保留名稱不一致 | 同 astra P2-10 | 同上 | 2139d7e |
| P3-2 | 一次 heartbeat 成功就 confirm update；floor 寫失敗仍取消 rollback | active 機器要 tunnel connected 加 local SSH healthy 才 confirm；floor 失敗見 astra P2-03 | 靜態審查加 astra P2-03 的測試 | 519f27c、289fe0d |
| P3-3 | process title 間隔可設極小值讓 agent 空轉 | agent 端 clamp 到至少 1 秒 | `TestProcessTitleIntervalClamped` | 519f27c |
| P3-4 | PowerShell 下載沒有大小限制 | HttpWebRequest 串流下載、不自動 redirect、60 秒 timeout、TLS 1.2+、byte 上限、每次都驗 URL | **沒有實際跑 PowerShell**，靜態審查 | ed844fd |
| P3-8 | 很舊的 schema 中 `status='retired'` 的 row 沒清 token | migration 也比對 `status='retired'` | `TestMigrationClearsTokenOfStatusRetiredRows` | ed844fd |
| P3-5～P3-7 | dashboard | 交給前端 builder | | |
| astra dashboard R2-03 | replay 回來的 health 事件沒有時間 | `machine.health_changed` 加 `last_probe_at`、`event_at`；`observed_changed` 加 `heartbeat_at`、`event_at`；只新增欄位 | `TestHealthChangedEventCarriesServerProbeTime`（含 replay） | 52e9395 |

### 平台驗證現況

- 已在 Linux（Ubuntu 24.04 container）實際跑過：rollback 兩種情境、internal-sshd 降權、systemd unit／drop-in verify。
- 只在 Mac 上跑過：其他所有 Go 測試、`go test -race ./...`、setup.sh 的 offline 測試。
- **還沒有實測**：Windows（ACL、junction、setup.ps1）、CentOS 6、nginx 1.24 的 `nginx -t`、HAProxy 2.8、sandbox unit 下真的 useradd／sshd reload。
