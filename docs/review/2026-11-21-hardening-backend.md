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
| internal-sshd 本身降權（daybreak round 3 P2-D） | 要改 key 檔 owner／mode 契約、migration、root 實測，只能跟 agent release 一起出 | `docs/design/2026-11-21-internal-sshd-privilege-drop.md` |

## 相容性（本機實測，2026-11-21 更新）

- 舊 DB（Argon2 agent token）接新 reachd：第一次 heartbeat 回 200，hash 被改寫成 `sha256:`。
- **舊 JWT（沒有 `sv`）接新 reachd：會失效（401），所有人都要重新登入一次**（daybreak P1-3）。Mac 的 `sat_` service token 不受影響。
- 新 mac-agent 接舊 reachd 8c74086：退回解析文字，可以正常同步。
- 舊 mac-agent 接新 reachd：文字 API 保留，可以用，但預設不再帶 target 回報的演算法選項（`legacy_ssh_config_options: false`）。
- 舊 agent：**已經裝好的機器**，heartbeat 格式沒變，照常運作。部署前就已建立、但還沒 provision 的 request 仍然可以 poll、可以被核准，但 **provision 時**會收到 410，要重新跑 setup。部署時盡量不要有 pending request，也不要去核准舊的 pending。
- **舊的 installer（8c74086 的 setup.sh／setup.ps1／reach-agent）不能再新增機器**：register 時就回 **426 Upgrade Required**，並提示要更新（astra R2-08）。以前是核准後 provision 才 403。新版後端上線後、新版 agent release（Stage 5）發布前，**這段期間不能新增或重裝機器**（provisioning 凍結，見「要 Arthur 決定」第 10 點；也不要手動安裝）。binding 規則沒有放寬。
- **新安裝、而且機器上沒有 sshd 的 target，會改用 agent 內建的 internal-sshd**（只聽 loopback；沒有 sftp；system 安裝時 session 以 target user 身分跑）。已經裝好的機器不受影響。Windows 系統安裝如果還沒裝 OpenSSH Server，要加 `-InstallOpenSSH` 才會裝。
- 部署 rollback 在 Ubuntu 24.04 container 裡實測 A～K 情境（加上 carrier 的 G／H／H2、fuser 的 I、外部 DB holder 的 J、JWT 復活的 K），載入的是 `deploy-jason.sh` 裡真正的 transaction 步驟、正式路徑（`scripts/test-rollback-in-container.sh`）。
- **部署會旋轉 `jwt_secret`**（只在 session migration 還沒跑過的 DB 上）：所有人要重新登入；rollback 後舊 binary 也用新 secret，所以部署前的 JWT 不會復活（daybreak round 4 P1-03）。`config.yaml` 部署前會備份到 `/var/lib/reach/deploy-backups/config.yaml.<ts>`，但 rollback 不會還原它（刻意的）。
- **部署期間，reachd 和 CLI 寫入者都要拿 writers lock**：部署記錄 fence／snapshot 時，`reachctl create/disable/remove` 會直接失敗並提示「部署進行中」。jason 需要裝 `psmisc`（`fuser`），沒有就拒絕部署。
- **舊版的 rollback 會擋自動還原**：新版 fence 檔案帶 journal 欄位，舊格式的 fence 一律不准自動還原（fail closed）。

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
9. **rollback 規則**：部署失敗時，如果新版上線後 `durable_actions` journal 沒有新的列（provision、approve／deny、disable／enable／retire、invite、service token、登出、revoke-all、update policy、process title；包括只寫了 intent、OS 動作做到一半的），就自動把 DB snapshot 和舊 binary 一起還原；有的話，reachd 會停著，等人決定。這些動作的 journal 列和動作本身在同一個 transaction，journal 寫不進去動作就不會發生（回 5xx）。heartbeat、token 遷移、observation 這些可丟的寫入不會擋自動回滾。另外：stop 不成功、forensic copy 失敗、找不到 probe binary，也都不會自動還原。
10. **Provisioning 凍結**（daybreak round 5）：從 Stage 4 的 fence 開始到 Stage 5 結束，**不新增、不重裝任何機器，也不手動安裝**。部署前 `scripts/preflight-provisioning-freeze.sh` 會自動檢查：pending、approved、provisioning 的 request，以及 response_pending、cleanup_pending 等進行中的 provision 都必須是 0，否則拒絕部署。另外：新安裝的機器如果在 provision 後一小時內**完全沒有 heartbeat**，會被撤銷並退役（有 heartbeat 就不會，不管 tunnel 狀態）。
11. **新 agent 的 update confirm 比較嚴格**：更新前是 active 的機器，新版要 tunnel 連上、local sshd 健康，並且真的用一次性 key SSH 登入跑一個指令成功，才會 confirm；做不到就在 5 分鐘後自動 rollback。
12. **Stage 4 用 `deploy-from-mac.sh` 時不要設 `REACH_PUBLISH_AGENT_RELEASE`**：預設 0，不會 build、簽、上傳或發布 agent。只有 Stage 5 才設 1。

## 部署順序（daybreak round 3 的兩階段；每一步之間至少觀察一天）

**只能從 `release/reach-2026-11` 分支的最新 commit 部署**（後端＋dashboard 已合併在這裡）。`deploy-from-mac.sh`、hub 端、直接跑 `deploy-jason.sh` 都會 fetch 這個分支，要求 HEAD 和 `origin/release/reach-2026-11` 完全相同（舊 commit 也拒絕），沒有任何 override。`fix/dashboard-2026-11` 分支上的 deploy 腳本是舊版，會把 reachd 降級並發布 agent，**不准使用**；`fix/hardening-2026-11` 停在 3261072，也不要再從它部署（daybreak round 5 N1）。

**Stage 4：只部署 server 端，不發布 agent。**（daybreak round 6 後改寫，照 `deploy-from-mac.sh` → `deploy-jason.sh` 的實際順序）

**部署前（Arthur，唯讀）**

1. jason 已裝 `psmisc`（`fuser`）。
2. 確認沒有進行中的 enrollment（pending／approved／provisioning 的 request 是 0）。從這裡到 Stage 5 結束，**不新增、不重裝機器**。這要靠操作的人自己遵守：preflight 和 `deploy-prepare` 只是在部署當下各檢查一次，不是持續生效的開關。
3. 目前 active 的 WebSocket target 是 0，所以部署後只驗 carrier 會回 101，不用等任何 WebSocket 機器重連。

**一次部署（Mac 上 `bash scripts/deploy-from-mac.sh`，不設 `REACH_PUBLISH_AGENT_RELEASE`、不設 `REACH_INSTALL_REACHD_UNIT`）**

腳本自動照這個順序做；任何一步失敗都會自動 rollback：

1. **release gate**：Mac 確認在 `release/reach-2026-11` 上、HEAD 等於剛 fetch 的 `origin/release/reach-2026-11`；hub fetch 後再比一次 SHA，`checkout --detach` 到那個 commit。
2. **hub 上 build**：reachd、carrier、dashboard（Node 22）。
3. **preflight freeze**（早期檢查，reachd 還在跑）：有未完成的 enrollment 就停止，什麼都還沒改。
4. **換 carrier**：備份舊 binary／unit／env → 裝新 binary、寫 env、unit（carrier 先不重啟）。
5. **換 dashboard**（atomic swap，舊的留備份）。
6. **裝 reachd 資源上限 drop-in**（`10-reach-limits.conf`，有變才裝，舊的備份）。
7. **停 reachd**，確認真的停了、沒有其他 process 開著 DB（`fuser`）。
8. **`reachd deploy-prepare`**（一個 process、拿 exclusive writers lock）：**再檢查一次 freeze**（fail closed，只放行 delivered／cleaned／legacy NULL）→ 清 retired token → 記 fence → snapshot → **無條件 rotate `jwt_secret`**。
9. 在 snapshot 的私有副本上演練 rollback（舊 binary 讀得懂）。
10. **裝新 reachd**（舊的留 `reachd.rollback.<ts>`，新的另存 probe 副本）→ 啟動 → TCP＋有認證的 readiness → `db-check`。
11. **重啟 carrier**，驗 101（`check-carrier-ready.sh`，不印 secret path）。
12. prune 舊備份（成組）。

**部署後（Arthur）**

1. 從外面 `ssh omarchy`、`ssh jack`。
2. dashboard 重新登入（`jwt_secret` 換了，所有人都要重登）。
3. Mac 上換新的 reach-mac-agent；確認 `~/.ssh/reach-tunnels.conf` 內容正確、`ssh -G jack` 正常。
4. 之後各自找時間：nginx 範本（先 `nginx -t` 再 reload，觀察 429）；HAProxy 建議 diff（Arthur 看過才套）。
5. **sandbox unit**（`REACH_INSTALL_REACHD_UNIT=1`）**不在 freeze 期間做**：它需要一次 新增→停用→啟用→退役 的實測，會新增機器。放到 Stage 5 之後，或在隔離的 lab 做。

**Stage 5：發布 agent（等 Arthur 有 Windows 測試環境再做）。**

Stage 5 前必修／必驗（daybreak round 4 的評估）：

- **P2-G Windows junction**：handle-relative writer（設計在 `docs/design/2026-11-21-windows-keyfile-traversal.md`），或至少在 Windows VM 做高次數 junction race 和 ACL 驗證。這是 SYSTEM→target user 的權限邊界，不能發布後再補。
- **Windows VM 實測**：Disabled／Manual／Automatic sshd、`-InstallOpenSSH`、`setup.ps1 -Reinstall` 的 preflight（目前只在 Linux PowerShell 7 加 mock 測過）、ACL。
- **P2-F bootstrap trust root（daybreak round 5：不能延後）**：現在的 `REACH_PUBLISH_AGENT_RELEASE=1` 會一起換掉公開的 `setup.sh`／`setup.ps1` 和 latest pointer，所以它不是「只更新既有 agent」。Stage 5 前二選一：(a) 先做一個真正的 agent-artifacts-only 發布模式（只放新版 artifact 和 manifest，不動 setup 腳本、不開放新增機器），或 (b) 先完成 P2-F，把 setup 腳本改從獨立的 immutable origin（GitHub release）取得。
- **Go 版本**：如果 fleet 裡還有 CentOS 6／Linux 2.6.32，agent 要用 Go ≤ 1.23.x 編譯（Go 1.24 起不支援這麼舊的 kernel），並在真的 VM 上 smoke。現在的 Stage 5 wrapper 用環境裡的 Go，沒有 pin。
- **daybreak round 5 的實機 gate**：PID 1 systemd 下的 watcher 死亡、startup restore、self-restart、pre-hardening binary 的 backup；雙棧／DNS64 的 IPv6 translation。
- **P2-C carrier per-machine credential**：建議 Stage 5 的 agent 就帶 phase 1（送 credential），等 fleet 都更新後 carrier 才強制。過渡期：exact nginx route、部署前數 websocket target、出事要 rotate path。
- **P2-D internal-sshd 本身降權**：既有 agent 的更新可以先做；但新的 system 安裝如果會用到 root internal-sshd，在完成降權前只開放給可以救回、明確接受風險的 target。
- RSA-only／OpenSSH 5.3 target 的 update confirm（RSA 一次性 key）要在真的舊 sshd 上驗一次。

- 前提：Linux container 和 **Windows VM** 都驗證過「不會重新開啟全介面的 port 22」（Disabled／Manual 的 OpenSSH 在沒有 `-InstallOpenSSH` 時保持原樣）、Windows authorized_keys ACL／junction、update confirm 的真實 tunnel + SSH 登入 gate。**我們現在沒有 Windows VM，所以這一步要等 Arthur 有 Windows 測試環境再做。**
- 三平台 artifact 和 manifest 簽章完成後，才更新 release manifest 和 setup 腳本（`REACH_PUBLISH_AGENT_RELEASE=1`）。
- 先從可以實體救回的機器開始，用 update policy 逐台更新。
- 評估過「setup.sh 和 reachd 一起發、agent 之後再發」：不可行。新的 setup.sh 只會下載並驗證已發布的 agent，沒有新 agent 就不能用；而舊 agent 正是 register 被 426 的那一個。所以新增機器的空窗只能靠「縮短 Stage 4 到 Stage 5 的時間」處理；空窗期間 provisioning 凍結，不手動安裝。

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

## astra round 2 + daybreak round 3（審 a2ba0cc，2026-11-21）

兩份報告：`~/30-39-Research/34-tech-research/2026-11-21-reach-hardening-astra-review-r2.md`、`2026-11-21-reach-security-round3.md`。重複的合併成一列。

| 編號 | 發現 | 怎麼修 | 證明（都做過 mutation 或「舊版會失敗」驗證） | commit |
|---|---|---|---|---|
| daybreak P1-A ＝ astra R2-02 | deploy-fence 靠 best-effort audit；audit 寫不進去時，revoke-all／disable／enable 成功但 fence 看不到，snapshot 還原會讓已撤銷的 JWT 復活 | 新表 `durable_actions`。只動 DB 的決策（revoke-all、登出、service token 建立／撤銷、approve／deny、invite claim、god code 自動核准、update policy、process title）和 journal 列在**同一個 transaction**，journal 寫失敗就整個 rollback 並回 5xx。有 OS 副作用的（provision、enable／disable／retire、provisioning recovery）**先 commit intent** 再動 OS，事後標 done／failed；intent 本身就擋還原。fence 只看 journal 的 MAX(id)；舊格式 fence 檔一律不准還原。snapshot 和 fence 都在 reachd 確認停止後才做 | `TestDeployFenceDetectsDurableActionsWhenAuditFails`（7 種動作，audit trigger 失敗）、`TestDurableActionRollsBackWhenJournalFails`、`TestOSActionsCommitIntentFirst`、`TestDeployFenceBlocksOnOpenIntent`、`TestDeployFenceOnPreJournalSchema`；container 情境 F（audit 失敗時 revoke-all → reachd 停著、DB 沒被換） | d260ae6 |
| daybreak P1-B ＝ astra R2-01 | 正式 rollback 把 probe 也換回不支援 `admin-probe-token` 的舊 binary，情境 A 最後停機 | 新 binary 另存一份到 `/var/lib/reach/deploy-backups/reachd-probe.<ts>`（0700），readiness 和 `deploy-fence check` 都用它，`test -x` 檢查；舊 binary 用 copy 還原（不 move，備份不會被吃掉）；找不到 probe 就不還原、不宣稱成功。container 測試改成直接載入 `deploy-jason.sh` 的 transaction 函式（BEGIN/END 標記），用正式路徑 | container 情境 A（還原成功、舊 binary、heartbeat 200、備份仍在）、B（durable 動作→停著）、C（probe 不見→停著、沒宣稱成功） | a0880aa |
| daybreak P1-C ＝ astra R2-07 | session_version migration 中斷後重跑不會再撤銷舊 JWT | `schema_migrations` marker 和 +1 在同一個 transaction | `TestInterruptedSessionMigrationIsRetried`（fault injection） | 01d9f26 |
| daybreak P1-D | systemd disabled 的 sshd 仍會被 `service sshd start` 啟動；Windows Disabled 的 OpenSSH 會被改成 Automatic | 共用 policy（`cmd/reach-agent/sshd_policy.go`）：只重啟開機就啟用的 sshd；有 systemd 時絕不 fall through 到 `service`；沒有 systemd 時只對有 rc S-link 的 script 用 `service`。Windows Go installer／daemon 和 setup.ps1：Disabled／Manual 沒有 `-InstallOpenSSH` 就不改。macOS 只在 Remote Login 已啟用時 kickstart。刪掉會 `systemctl enable --now` 的舊 installer 死碼 | `TestDaemonDoesNotStartDisabledSystemdSSHD`（Linux container，真的 `Daemon.startLocalSSH` 加假 systemctl／service；舊版失敗、新版通過）、`TestDisabledSystemdSSHDIsNotStartedThroughService`、`TestWindowsSSHDScriptPolicy`。**Windows／PowerShell 沒有實測** | a4bfeba |
| astra R2-03 | rollback 不檢查 stop 是否成功，就換 DB／WAL／binary | `stop_reachd_verified`：systemctl 回傳值、unit inactive、舊 MainPID 已結束、（有 fuser 時）沒人開著 DB；不成立就什麼都不動、停止 transaction。之後每一步（cp／rm／mv／start）都檢查回傳值 | container 情境 D（stop 回 70：新 process 照跑、binary 和 DB 都沒被換、沒有 forensic copy、沒宣稱成功） | a0880aa |
| astra R2-04 | process-title 不在 machine lock 裡，retire 會留下「tunnel 已退役、machine 仍 active、token 有效」 | process-title 進同一把 lock，lock 內重讀並 generation CAS；enable／disable／retire 的 machine 和 tunnel finalize 放在同一個 transaction，CAS 輸了就全部 rollback。CAS 在 DB 裡做，所以其他 process（reachd CLI）的寫入也擋得住。順便：補償失敗會把 machine 標成 disable_failed（astra 的附帶觀察） | `TestProcessTitleCannotSplitRetire`、`TestRetireFinalizeIsAtomicAgainstOtherWriters`（模擬跨 process 的 generation 變動）、`TestFailedCompensationIsRecorded` | d260ae6 |
| astra R2-05 | forensic copy 失敗仍覆蓋 live DB | 先完整複製到 staging 目錄、逐一 `cmp` 驗證，再 rename 成 failed set；任何一步失敗就不自動還原 | container 情境 E（copy 失敗→停著、live DB 沒被換、沒有半套 failed set） | a0880aa |
| astra R2-06 | carrier rollback 沒還原 `ws-carrier.env` | env 也記錄 existed／backup；rollback 時還原，原本沒有就移走這次新增的 | container 情境 G（兩種情況） | a0880aa |
| astra R2-08 | 舊 installer 新註冊會在核准後 provision 403 | **不改 binding 規則**。register 沒帶 mode／local_port（舊協定）就直接回 426 和更新提示，不存任何東西。部署文件寫明 Stage 4～5 之間不能用舊 installer 新增機器；評估「setup.sh 先發」不可行（見上方部署順序） | `TestOldInstallerGetsUpgradeRequiredAtRegister`；用 astra 留下的**真的 8c74086 binary** 跑 `TestRealOldAgentGetsUpgradeRequired`（`REACH_OLD_AGENT_BIN=...`）：install 在 register 就停在 426 | 7b83370 |
| astra R2-09 ＋ daybreak P2-E | watcher 把「confirm 正在進行」當成已 confirm；orphan claim；hub 事後回 disabled 就能繞過 confirm gate；local SSH 只測 banner | 改成持久的狀態機 `update-state.json`（pending／confirming／confirmed／rolling_back／rolled_back），atomic write + fsync，每次轉換都拿 OS file lock；只有 confirmed 會取消 rollback；agent 啟動時，過期未 confirm 的更新直接 rollback，還沒過期就重新 arm watcher。狀態裡記錄**更新前**是否 active；active 的機器要 tunnel connected、local sshd healthy，並用一次性 ed25519 key 真的 SSH 登入跑 `echo` 且輸出正確（事後移除 key，marker 讓 sync_keys 也會清掉）。舊的 flag 檔仍可 confirm | `TestWatcherDoesNotTrustAnInProgressConfirm`、`TestCrashedConfirmIsRolledBack`、`TestStartupRollsBackOrphanedUpdate`、`TestConfirmKeepsRollbackArmedWhenFloorFails`、`TestConfirmGateIgnoresHubDisabledAfterUpdate`、`TestLocalSSHLoginProbe`（真的 internal-sshd：成功、指令沒跑成、沒有 sshd 三種） | d3cdd4c |
| astra R2-10 ＋ daybreak P2-A | provisioning recovery 只改 DB；machine 已 offline、request 還沒 finalize 的窗口漏掉；正常 finalize 沒有 CAS | recovery 先對 request 做 CAS claim（expired），同一個 transaction 撤銷沒送出的 agent token 並寫 journal，再走正常的 locked retire（DisableAuth／Remove／驗證沒殘留），不管 machine 是 provisioning、offline 還是 failed；清不掉就留 cleanup_failed tombstone，maintenance 會一直重試。CreateMachine 和 finalize 都要求 request 還是 provisioning，否則回 410、不給 token；finalize 和 invite consume 同一個 transaction；provision 的 OS 階段拿 machine lock | `TestRecoveryRevokesHalfFinishedProvision`（provisioning／offline／failed 三種）、`TestRecoveryCleanupFailureIsRetried`、`TestProvisionFinalizeLosesToRecovery`、更新後的 `TestStuckProvisioningIsRecovered` | 4c1171c |
| astra R2-11 | desired key 全部無效時，清掉最後一把有效 key 還回成功 | 先建好候選 key 集合才動檔案；desired 非空但沒有一把有效就回錯、檔案不動；明確 disable 或空集合才清空 | `TestSyncKeysKeepsFileWhenNoDesiredKeyIsValid`、`TestSyncKeysRevocationsStillRemoveReachKeys`、`TestSyncKeysInstallsValidSubset` | 874443b |
| astra R2-12 ＝ daybreak P3-A | 容量滿時 Mac 重連會同時失去新舊兩條 stream | 先以「舊的已讓出」計算容量，放得下才把舊的 slot 轉給新的並 cancel 舊的；放不下就保留舊的；舊的 release 不會重複扣 | `TestSingleStreamReplacementAtCapKeepsOneStream`、`TestRejectedReplacementKeepsIncumbent`（兩個在舊版都失敗） | 48438ad |
| astra R2-13 | `reachd ws-server` 沒有強制 path | 兩個入口共用 `wscarrier.RunServerCLI`（同樣的 flag、path gate、上限）；沒有 gate 的 Serve 改名 `ServeAnyPathForTests` | `TestRunServerCLIRequiresPath`、`TestReachdWSServerRequiresPath` | 304905b |
| astra R2-14 | README 的 `proxy_pass .../;` 會改寫 path → 404；unit 沒有 EnvironmentFile | README 改成無 URI 的 proxy_pass、exact location、X-Real-IP、101 preflight（不把 secret 留在 history）；unit 加 `EnvironmentFile`、`User`、建立 env 檔的方法。`deploy/nginx` 範本本來就正確 | 文件 | 304905b |
| daybreak P2-B | 307/308 redirect 會把 heartbeat token 和 body 送到 HTTP | `internal/safehttp`：control-plane 的 client 一律拒絕 redirect；下載 client 每一跳都重驗，不准 https→http，最多 5 跳；第一個下載 URL 也驗證。reach-agent（register／poll／provision／heartbeat／uninstall／更新／carrier 下載）和 reach-mac-agent 都不再用 `http.DefaultClient`；mac-agent 拒絕非 loopback 的 http api_url；`reach-sync-mac.sh` 登入前就拒絕 | `TestHeartbeatDoesNotFollowRedirect`（daybreak 的 307 PoC）、`TestDownloadRedirectPolicy`、`TestValidateURL`、`scripts/test-reach-sync-mac.sh`（新增 3 個拒絕案例） | af54683 |
| daybreak P2-H | 同一個 NAT 後面，錯的 secret 用光 bucket 後，對的也被 429 | bucket 用光後，錯的仍然 429（不再計數）；但 SHA-256 存的 secret／token 仍會檢查一次（便宜），對的可以通過。舊 Argon2 hash 在被擋時一律不算（保護 CPU）。poll、heartbeat、uninstall 都一樣 | `TestSameIPBadAuthDoesNotBlockRightSecret`、`TestSameIPBadAgentTokensDoNotBlockRightToken`、`TestBlockedBucketSkipsArgonForLegacyHashes` | 982ae53 |
| daybreak P3-B | server 和 Mac 的部署專屬保留名稱是兩份設定 | `ssh-config.json` 加 `reserved_hosts`（reserved_slugs 加 ProxyJump alias，空的時候不送，舊 Mac 不受影響）；Mac 取內建、server、本機三者聯集 | `TestSSHConfigJSONIncludesReservedHosts`、`TestSyncHonoursServerReservedHosts` | 991086c |
| daybreak P3-C | Mac 一次拿掉所有 host 的 options | 先整份驗證；失敗時逐 host 單獨驗證，只拿掉被 ssh 拒絕的那台；`ssh -G` 也改成檢查每一台有 options 的 host | `TestBadOptionsOnlyAffectTheirHost`（老機器的 KexAlgorithms 留著、壞 cipher 拿掉） | 991086c |
| daybreak P2-C | carrier path 仍是全 fleet 共用 secret | 設計文件加上過渡期操作規則（部署前數 websocket target、用 repo 的 nginx route、出事要 rotate path 不能只按 Disable） | `docs/design/2026-11-21-carrier-credentials.md` | cc04b1b |
| daybreak P2-D | internal-sshd 的 SSH 處理仍是 root | **沒做，只寫設計**：要改 internal-sshd 的 key 檔 owner／mode 契約和剛加固的 writer 檢查、要 migration 和 root 實測、只能跟 agent release 一起出，不算小 | `docs/design/2026-11-21-internal-sshd-privilege-drop.md` | cc04b1b |
| daybreak P2-F | bootstrap script 仍由 jason 提供 | 只寫設計（加上 Stage 5 之後的順序） | `docs/design/2026-11-21-bootstrap-trust-root.md` | cc04b1b |
| daybreak P2-G | Windows junction 的 check-to-rename race | 只寫設計（加上 Windows VM 上的測試計畫） | `docs/design/2026-11-21-windows-keyfile-traversal.md` | cc04b1b |
| daybreak P3-D／P3-E | dashboard | 交給前端 builder | | |
| 其他 | rehearsal canary 固定 id 可能和真的 machine 撞（astra 的 deploy 子報告） | canary 的 id／slug 改成隨機 | container 測試 | a0880aa |

### 平台驗證現況（round 3 後）

- Linux（Ubuntu 24.04 container）實際跑過：rollback A～G（真的 `deploy-jason.sh` 函式、正式路徑、真的 8c74086 和新版 binary）、`Daemon.startLocalSSH` 不會啟動 disabled sshd、reach-agent 全部測試。
- Mac：`go test -race ./...` 全過；`scripts/test-reach-sync-mac.sh`；真的 8c74086 reach-agent 對新 reachd 註冊得到 426。
- 跨平台編譯：reach-agent（linux amd64／arm64／386／armv6／armv7、darwin、windows）、reach-mac-agent。
- **還沒有實測**：Windows（ACL、junction、setup.ps1、sshd StartupType policy、PowerShell）、CentOS 6、HAProxy 2.8、sandbox unit 下真的 useradd／sshd reload、PID 1 systemd 下完整的部署和 rollback、從外部 vantage 的 WebSocket 101 和 SSH E2E。

## astra round 3 + daybreak round 4（審 b0f53d3，2026-11-21）

兩份報告：`~/30-39-Research/34-tech-research/2026-11-21-reach-hardening-astra-review-r3.md`、`2026-11-21-reach-security-round4.md`。重複的合併成一列。所有新測試都做過 mutation 或「在舊版會失敗」驗證。

| 編號 | 發現 | 怎麼修 | 證明 | commit |
|---|---|---|---|---|
| daybreak P1-01 | `deploy-from-mac.sh` 寫死 `REACH_PUBLISH_AGENT_RELEASE=1`，Stage 4 會連 agent 一起發布 | 改成可設定、預設 0；0 時不 build、不簽、不上傳，deploy-jason.sh 也收到 0；1 才需要簽章金鑰 | `scripts/test-deploy-from-mac.sh`：真的執行送到 hub 的遠端腳本（假 hub、假 ssh），檢查 deploy-jason.sh 收到的環境；舊版失敗 | fc9584f |
| astra R3-01（P1） | DB CAS 擋不住其他 process 的 OS 動作：retire 完成後，另一個 instance 的 provision 把 key、drop-in 寫回去 | `lockMachine`＝process 內 mutex＋`<dbdir>/locks/machine-<id>.lock` 的 flock，涵蓋整個「讀 DB→OS→寫 DB」（provision、enable／disable／retire、process-title）。最後的 CAS 仍輸（不守鎖的寫入者）時：provision 做 Remove＋Residue 補償、tunnel 留 cleanup_failed tombstone 不釋放 port；retire 把 machine 標 retiring／cleanup_failed、撤 token，不會看起來還是 active | `TestIndependentRetireWaitsForProvisionOSPhase`（兩個 Provisioner 共用一個 DB）、`TestMachineLockAcrossProcesses`（Linux container 用 flock(1) 驗跨 process）、`TestRetireFinalizeIsAtomicAgainstOtherWriters`（更新後） | d27ad57 |
| daybreak P1-02 ＋ astra R3-03 | snapshot 在 fence 前；外部 writer 可在中間寫入並被 rollback 吃掉；沒有 fuser 就略過 holder 檢查 | `deploy-fence record --snapshot`：在 **writers lock（排他）** 下依序 清 retired token → 記 fence → `VACUUM INTO` snapshot → quick_check。reachd serve 和所有經 initApp 的 CLI 都拿 shared writers lock，部署期間 CLI 寫入直接失敗。`fuser`（psmisc）是必要條件；stop 檢查掃 DB／WAL／SHM，沒有 fuser 就 fail closed | `TestRecordFenceThenSnapshot`、`TestWritersLock`；container 情境 I（沒有 fuser→停著、DB 沒動）、J（另一個 SQLite client 開著 DB→停著） | afb3174 |
| daybreak P1-03 | session migration 不在 journal，rollback 後舊 JWT 復活；retired token 同理 | 部署在新 binary 第一次啟動前執行 `reachd rotate-jwt-secret --if-session-migration-pending`（只換 `jwt_secret` 那一行，保留 mode／owner，fsync）；config.yaml 不在 rollback 範圍，舊 binary 也用新 secret。retired token 在 snapshot 之前就從 live DB 清掉 | `TestRotateJWTSecretKillsOldTokensAcrossRollback`、`TestRotateOnlyIfSessionMigrationPending`、`TestFenceRecordClearsRetiredTokensBeforeSnapshot`；container 情境 K（rollback 後，部署前的 JWT 在舊 binary 上是 401） | afb3174 |
| daybreak P1-04 ＝ astra R3-04 | rollback 時 carrier／unit／drop-in 的錯誤被吞掉，仍印「restored and ready」 | `restore_carrier_state`：unit、env、binary 逐一還原並 `cmp`；任何一個失敗就讓 carrier 停著；原本 active 的 carrier 必須 `is-active` 而且在設定的 path 上回 101（`scripts/check-carrier-ready.sh`，不印 path）。`restore_reachd_units` 也檢查並要求 systemd 回報 loaded。任何失敗記進 `rollback_incomplete`，結尾絕不印成功。部署端也先 `cmp` carrier 備份，carrier readiness 改成 101 | container 情境 G（舊 env／path 回 101）、H（舊 carrier 起不來→incomplete；舊版腳本失敗）、H2（env 還原失敗→carrier 停著） | 82f5a05、9403ba4 |
| daybreak P2-01 | setup token 的消耗不在 journal，rollback 後一次性 token 復活 | `approved→provisioning`、清 hash、`request.provision.start` journal 同一個 transaction；journal 失敗 token 就不被消耗 | `TestSetupTokenConsumptionIsJournalled` | afb3174 |
| daybreak P2-05 ＝ astra R3-02 | 手動 update-agent 沒寫 journal | 手動更新的 command 和 journal 同 transaction（自動 policy 仍可丟）；同一台 machine 只能有一筆 open update，在同一個 transaction 檢查 | `TestManualUpdateIsJournalled`（audit 失敗、journal 失敗兩種）、`TestOnlyOneOpenUpdatePerMachine`（8 個並行請求） | afb3174 |
| daybreak P2-02 ＋ astra R3-05 | provisioning recovery 有 orphan 窗口：provisioned 但 response 沒送到、recovery claim 後 retire intent 失敗就永遠不重試、CreateMachine 失敗時丟掉 machine 連結 | `requests.provision_phase` 持久狀態機：claimed → os_created → response_pending → delivered；cleanup_pending → cleaned。recovery 的 claim 在同一個 transaction 設 cleanup_pending（持久的重試標記）＋撤 token＋journal；每次 maintenance 都 retire 所有 cleanup_pending，直到真的 retired 才標 cleaned。provisioned 超過一小時沒有 heartbeat（response 遺失）也會被撤銷、退役；第一次 heartbeat 標 delivered。CreateMachine 已建立 machine 才失敗時交給 cleanup，不再丟掉連結。舊版留下的 recovered request 也會被接手 | `TestRecoveryRetriesWhenRetireIntentFails`（astra 的 PoC）、`TestUndeliveredProvisionIsCleanedUp`、`TestHeartbeatMarksProvisionDelivered` | 2f9c14e |
| astra R3-06 | floor 已寫入、confirmed 還沒寫時 crash，recovery 回滾到舊 binary 但 floor 留在新版本 | state 記錄 `FloorBefore`；recovery 遇到 confirming 且 floor 已達 CandidateSequence 時，完成 confirmed，不回滾 | `TestCrashAfterFloorWriteFinishesConfirm` | 377282b |
| astra R3-07 | 舊 terminal state 遮蔽新的 legacy flag；過期 rolling_back 重放、撤銷後來的更新 | 每次更新有 attempt id，記錄 candidate 和 backup 的 sha256；只在磁碟上是這次的 candidate 時才還原；rolling_back 但已經是 backup 就只收尾；路徑上是別的 binary（後來的更新）就把舊 state 結束、不動 binary。legacy flag 永遠優先於舊的 terminal state | `TestLegacyFlagWinsOverTerminalState`、`TestStaleRollingBackDoesNotUndoLaterUpdate`、`TestCompletedRestoreIsNotReplayed` | 377282b |
| daybreak P2-04 | update 可以 double-flight；DataDir symlink；restore 後 restart 前 crash；RSA-only target 無法 confirm；probe 和 sync_keys 的 RMW 競態；backend 同機器多筆 update | 檢查、記錄、安裝、arm watcher 全部在同一把 lock 裡；新增 `restart_pending`，restart 成功才 rolled_back；DataDir 必須是自己擁有、group／other 不可寫的實體目錄，state／lock 用 O_NOFOLLOW 開；probe 依 target 支援的 key type 選 Ed25519 或 RSA；probe 的 key 編輯和 sync_keys 共用一把 mutex；backend 一機一筆 open update（見上） | `TestUpdateStartIsSingleFlight`、`TestUpdateStateRefusesSymlinksAndLooseDataDir`、`TestCrashedConfirmIsRolledBack`（現在停在 restart_pending）、`TestProbeSignerFollowsTargetKeyTypes`、`TestProbeEditAndSyncKeysAreSerialised`。**RSA 一次性 key 沒在真的 OpenSSH 5.3 上試過** | 377282b |
| daybreak P2-03 | download 可被 hub 導到任意 HTTPS（SSRF），非 2xx 的 body 會回傳給 hub | `safehttp.Downloader`：每一跳都必須是允許的 origin（設定的 API）而且是 https；dialer 只解析一次、拒絕 private／link-local／multicast／loopback，連到檢查過的 IP；update_agent 的 api_url 不是本機設定的 API 就拒絕；carrier binary 必須有 sha256 pin；錯誤訊息只有 HTTP 狀態碼，不含 body | `TestDownloaderStaysOnOrigin`、`TestDownloaderRefusesPrivateAddresses`、`TestDownloaderCheck`、`TestDownloadErrorHasNoBody`、`TestUpdateCommandRejectsForeignAPIURL`（Linux container） | ad805f4 |
| astra R3-08 | 只有空白 key 的 desired list 會清掉最後一把有效 key | 非空的 desired list 一律是替換；空白也算無效 candidate | `TestSyncKeysBlankDesiredKeysKeepFile` | 3583488 |
| astra R3-09 | setup.ps1 `-Reinstall` 先刪舊 agent 才檢查 sshd policy | 新增唯讀的 `Test-ReachOpenSSHPolicy`，`-Reinstall` 在 uninstall 之前先跑，不通過就什麼都不刪 | `scripts/run-setup-ps1-tests.sh`：真的 PowerShell 7（Linux container）、mock Windows cmdlet；舊版失敗。**不是 Windows VM 實測** | 3583488 |
| astra R3-10 | SysV 漏了 `/etc/rc.d/rc4.d` | 加上；測試對每一個 runlevel 目錄在 temp root 建 S-link | `TestSysVRunlevelDirectories` | 3583488 |
| astra R3-11 | README 沒有建立 `reach-wstunnel` user | 在 unit 之前加可重跑的 useradd | 文件 | 3583488 |
| astra P3 | `reserved_hosts` 讓舊 JSON client 拒絕解析 | Mac 端 top-level 未知欄位忽略，host 欄位仍嚴格 | `TestSSHConfigJSONForwardCompatible` | a1d8c76 |
| daybreak P3-01 | journal 可被 no-op 塞滿、沒有 retention | update policy 沒變、撤銷不存在的 token 都不寫 journal；maintenance 刪 90 天前的 journal | `TestJournalSkipsNoOpsAndIsPruned` | a1d8c76 |
| daybreak P3-02 | 無效 glob 被靜默忽略；只保留 default hub 的 alias | config 和 Mac 載入時驗證 pattern，無效的 fail closed（match 全部）；所有 hub 的 ProxyJump alias 都保留，也送給 Mac | `TestValidateAndFailClosed`、`TestAllHubAliasesAreReserved` | a1d8c76 |
| daybreak P3-04 | 同一秒兩次登入的 JWT 完全相同 | JWT 加隨機 `jti` | `TestJWTsIssuedTogetherAreDistinct` | a1d8c76 |
| daybreak P3-03 | dashboard WebCrypto rejection | 前端，交給 dashboard builder | | |
| daybreak P2-C／D／F／G | carrier credential、internal-sshd 降權、bootstrap trust root、Windows junction | 照 daybreak 的評估寫進「部署順序」的 Stage 5 前提；P2-G 標成 Stage 5 前必修 | 設計文件 | 本 commit |

### 平台驗證現況（round 4 後）

- Linux（Ubuntu 24.04 container）：rollback A～K（真的 `deploy-jason.sh` 函式、正式路徑、真的 8c74086 和新版 reachd、真的新版 carrier）；跨 process flock；`Daemon.startLocalSSH`；reach-agent 全部測試（含 update_agent 的 api_url 拒絕）。
- PowerShell 7（Linux container）：setup.ps1 解析與 reinstall preflight（mock）。
- Mac：`go test -race ./...` 全過；`scripts/test-deploy-from-mac.sh`、`scripts/test-reach-sync-mac.sh`、`scripts/test-setup-bootstrap.sh`。
- **還沒有實測**：Windows（VM）、CentOS 6、OpenSSH 5.3／RSA-only 的 update confirm、HAProxy 2.8、PID 1 systemd 下完整的部署和 rollback、sandbox unit、外部 vantage 的 WebSocket／SSH E2E。

## astra round 4 + daybreak round 5（審 3261072／fd5d7de，2026-11-21）

兩份報告：`~/30-39-Research/34-tech-research/2026-11-21-reach-hardening-astra-review-r4.md`、`2026-11-21-reach-security-deep-daybreak-round5.md`。重複的合併成一列。所有新測試都做過 mutation 或「在舊版會失敗」驗證（少數不能編譯或沒有效果的 mutant 換成有效的再測）。

**分支：** 從 `fix/hardening-2026-11`（3261072）開 `release/reach-2026-11`，merge `fix/dashboard-2026-11`（fd5d7de）（e2df69b）。merge 後後端與 3261072 完全相同、`dashboard/` 與 fd5d7de 完全相同。之後的修正都在 release 分支；hardening 分支停在 3261072。

| 編號 | 發現 | 怎麼修 | 證明 | commit |
|---|---|---|---|---|
| daybreak N1（P1） | 兩個分支沒有安全的整合部署路徑；dashboard 分支的 deploy 腳本會降級 reachd 並發布 agent | 建 release 分支並 merge dashboard；`deploy-from-mac.sh` 只接受 `release/*` 分支；文件寫明只能從 release 分支部署、dashboard 分支的腳本不准用 | merge 後驗證：`go test -race ./...`、dashboard vitest 90/90、typecheck、generate、container A～L；`scripts/test-deploy-from-mac.sh` 加上非 release 分支被拒 | e2df69b、390857e |
| astra R4-01（P1）＋ daybreak N6 | writers lock 沒涵蓋 fence check→restore，CLI 寫入被 restore 吃掉還印成功；JWT rotation 的 marker 檢查有 TOCTOU | 兩個 critical section 各由**一個** reachd process 持 exclusive writers lock 完成：`reachd deploy-prepare`（清 retired token→fence→snapshot→在同一把鎖裡判斷 marker 並旋轉 jwt_secret）、`reachd deploy-restore`（forensic copy→fence check→snapshot restore）。執行期間 reachd／CLI 都起不來；deploy-prepare 之後到新版啟動前的寫入會在 fence 之上，restore 會拒絕；deploy-restore 之後的寫入落在已還原的 DB 上，不會被覆蓋 | container 情境 L（restore 持鎖時 CLI 寫入被拒、沒有任何寫入遺失）；astra 的 `review-rollback-lock-gap.sh` 改成新流程後通過（沒有寫入遺失）；`TestRestoreRefusedWhileAWriterRuns`、`TestRestoreRefusedAfterDurableAction`、`TestRestoreWithoutDurableActions`、`TestDeployPrepareRotatesUnderTheLock` | 8189645 |
| astra R4-02（P1） | CAS 輸了、補償失敗，卻留下 retired／complete、port 已釋放 | 沒證明 hub 乾淨就在一個 transaction 裡：machine 改 retiring／cleanup_failed、撤 token；tunnel 拿回原本的 user／port、標 cleanup_failed、清 cleanup_verified_at。port 已被別台拿走就記錄衝突交人處理，不覆寫新 owner。SQL 錯誤寫 log | `TestFailedProvisionCompensationKeepsTombstone`（port 沒被占／已被占兩種） | 7043afe |
| daybreak N3 ＝ astra R4-04 | delivered 依賴 agent 自報 tunnel 狀態：合法但 tunnel 起不來的機器被撤銷；偷 token 者可假報 connected | 任何通過認證、observation 寫入成功的 heartbeat 都標 delivered（錯誤不吞）；tunnel 健康交給 hub probe。recovery 也不撤銷有 observation 的 response_pending。文件寫明：provision 後一小時內完全沒 heartbeat 才會被撤銷 | `TestStoppedTunnelHeartbeatStillMarksDelivered`（astra 的 PoC） | 385024d |
| daybreak N8 | readiness 的 admin JWT 和 carrier path 出現在 curl argv（jason 的 /proc 沒有 hidepid） | JWT、carrier path、rehearsal canary token 都用 bash builtin `printf` 經 stdin 給 `curl --config -` | `scripts/test-readiness-argv.sh`（假 curl 記錄 argv／stdin；舊版失敗） | fcc4c42 |
| Stage 4 前提 | provisioning 要凍結 | `scripts/preflight-provisioning-freeze.sh`（唯讀）在部署改任何東西之前檢查：pending／approved／provisioning 的 request、claimed／os_created／response_pending／cleanup_pending 的 provision 都必須是 0；文件：從 fence 到 Stage 5 結束不新增、不重裝、不手動安裝 | smoke（乾淨→通過，有 pending／response_pending→拒絕） | 390857e |
| astra R4-03 ＋ daybreak N7 | 同一個 DB 用 symlink alias 會拿到另一組 lock；lock 檔、shell `exec 9>`、config rotation 都會跟隨 symlink | SQLite 和 lock 用同一個 canonical 絕對路徑，DB 路徑本身是 symlink 就拒絕；lock 檔用 `openat(O_NOFOLLOW)`，在檢查過的目錄下，fstat 必須是自己的、0600、link count 1 的 regular file；shell 改用 `open_deploy_lock`（目錄必須是自己的、group／other 不可寫，lock 不能是 symlink，`exec 9<>` 不截斷）；config rotation 只接受自己的、link count 1、group／other 不可寫的 regular file，directory fsync 錯誤會回報 | `TestDatabaseAndLockAliasesRefused`、`TestRotateRefusesSymlinkAndLooseConfig`、`scripts/test-deploy-lock.sh`（Mac＋Ubuntu） | 8189645、b9dd60a |
| daybreak N4 | admin 帳號叫 `system` 時，手動 update 不寫 journal、fence 也忽略 | `queueAgentUpdate` 改用 typed origin（`updateManual`／`updateAutoPolicy`）；自動 policy 的 audit 改成 `machine.agent_update.auto_queue`；`system` 等 Reach 自己用的 actor 名稱不能當 initial admin username | `TestUpdateOriginIsTyped` | 48fe5d6 |
| daybreak N5 ＋ astra R4-08、R4-09 | `100.64/10`、NAT64、6to4 等沒擋；carrier 的 URL 自己授權自己的 origin | 只允許明確的 global unicast，並排除 RFC 6890 的特殊用途區段（`100.64/10`、benchmark、documentation、reserved、`64:ff9b::/96`、`64:ff9b:1::/48`、`2002::/16`、Teredo 等）；carrier 下載的 origin 只能來自 installer 設定的 API URL | `TestSpecialPurposeAddressesRefused`、`TestCarrierDownloadOnlyFromConfiguredAPI`（foreign origin 一次 request 都沒收到） | f856628 |
| daybreak N2 ＝ astra R4-05、R4-06 | 舊 watcher 會回滾下一次 attempt；backup 在 lock 外、同秒同名可被覆蓋 | watcher 帶 `--attempt`，在 update lock 裡比對，不一致就不動；startup re-arm 也帶 attempt。backup 在 lock 裡由擁有 attempt 的 worker 建立，檔名用 attempt，`O_EXCL|O_NOFOLLOW`、fsync | `TestOldWatcherLeavesNewAttemptAlone`、`TestBackupIsCreatedExclusively` | 1544728 |
| astra R4-07 | 同版本 rollback 的 restart_pending 永遠收不了尾 | 用正在執行的 image 的 hash（`/proc/self/exe`）比對 backup hash，不再比版本字串 | `TestRestartPendingFinishedByRunningImage` | 1544728 |
| daybreak N10（Stage 5） | DataDir 檢查在 I/O 之後；probe key 清理失敗時靜默 confirm | 讀 state 前先檢查 DataDir（symlink／別人的目錄直接拒絕）；一次性 key 清不掉就讓 probe 失敗，不 confirm。**restart／mark 由存活的 helper 處理、逐層 openat、pre-hardening backup 的實測**：只做到「restored binary 自己在啟動時用 image hash 收尾」，完整 helper 與 PID 1 實測列為 Stage 5 gate | `TestSymlinkedDataDirNotRead` | 1544728 |
| astra R4-11（P3） | 沒有 hash 的 state 仍會重放 restore | 沒有 identity 的 state 直接結束，不重放 | `TestHashlessStateIsNotReplayed` | 1544728 |
| daybreak N9 ＋ astra R4-10 | 同值 process-title 每次都寫 journal、加 generation；同值 policy 仍寫 decision audit；舊 fence 在 prune 後會誤判；備份沒有成組清理 | process-title 跟 canonical 值相同就不寫（不加 generation、沒有 journal）；policy 沒變就不寫 audit；`deploy-restore` 拒絕超過 30 天的 fence（journal 保留 90 天）；fence、snapshot、probe、config backup、forensic set 依部署時間成組清理 | `TestRepeatedSettingsAreNoOps`、`TestRestoreRefusesOldFence`、group prune smoke | b458a6b、8189645 |
| astra R4-12（P3） | 未發布中間版的 linked provisioned／NULL phase 不會被接手 | one-shot backfill：有 observation 的標 delivered，其餘標 response_pending；沒有 machine link 的（production 8c74086）不動 | `TestProvisionPhaseBackfill` | b458a6b |
| astra R4-13（P3） | 不用處理（協調者指示） | — | — | — |
| Stage 5 前提 | P2-F 不能延後；Go 版本 pin | 寫進「部署順序」的 Stage 5 前提（見上） | 文件 | 本 commit |

### 平台驗證現況（round 5 後，release 分支）

- Linux（Ubuntu 24.04 container）：rollback A～L（真的 `deploy-jason.sh` 函式、真的 `deploy-prepare`／`deploy-restore`、正式路徑、真的 8c74086 和新版 reachd、新版 carrier）；astra 的 lock-gap 重現（改成新流程）；跨 process flock；reach-agent 與根套件全部測試；deploy lock helper。
- Mac：`go test -race ./...` 全過；dashboard vitest 90/90、typecheck、generate；`test-deploy-from-mac.sh`、`test-readiness-argv.sh`、`test-deploy-lock.sh`、`test-reach-sync-mac.sh`、`test-setup-bootstrap.sh`。
- **還沒有實測**：Windows VM、CentOS 6、OpenSSH 5.3、HAProxy 2.8、PID 1 systemd 下完整的部署／rollback／agent self-restart、sandbox unit、外部 vantage 的 E2E、雙棧／DNS64。

## astra round 5 + daybreak round 6（審 release 522af64；協調者指示：Stage 4 只修 4 項）

報告：`~/30-39-Research/34-tech-research/2026-11-21-reach-hardening-astra-review-r5.md`、`2026-11-21-reach-security-round6.md`。這一輪照協調者的收斂指示只修 Stage 4 blocker，其餘列在下一節。

| 編號 | 發現 | 怎麼修 | 證明 | commit |
|---|---|---|---|---|
| daybreak R6-P1-01 ＝ astra R5-05 | release gate 接受舊 ancestor、任何 `release/*`、`REACH_ALLOW_BRANCH`；hub 只要求 commit 在任一 origin branch；直接跑 `deploy-jason.sh` 會 pull、detached 時切回 main；文件還叫人從 dashboard 分支部署 | `scripts/release-gate.sh`：唯一的 `release/reach-2026-11`，fetch 這個 ref，要求 HEAD 和 `origin/release/reach-2026-11` **SHA 完全相同**。Mac、hub 端（再 fetch、再比一次）、直接跑 `deploy-jason.sh` 都套用；拿掉 `REACH_ALLOW_BRANCH`、拿掉 pull 和切 main。刪掉文件裡從 dashboard 分支部署的步驟 | `scripts/test-deploy-from-mac.sh`（fixture 用同名 release 分支）：非 release 分支（即使設 `REACH_ALLOW_BRANCH`）、`release/other`、detached、舊 ancestor、Mac 檢查後 tip 被推進（hub 拒絕）、直接跑 deploy-jason 在舊 commit／落後 tip 都被拒且 HEAD 不動；把檢查改回 ancestor 或拿掉 hub 比對，測試都會失敗 | 98b0245 |
| daybreak R6-P1-02 | preflight 之後到停 reachd 之間可以新 register；未知 phase fail open | `deploy-prepare` 在 exclusive writers lock 裡、fence／snapshot 之前，用一個 read transaction 再檢查：request 只能是 provisioned／expired／denied，phase 只能是 NULL（legacy）／delivered／cleaned，沒有 status=provisioning 的 machine；其他（含未知值）一律拒絕，什麼都不寫（沒有 fence、snapshot，secret 不換）。snapshot 寫完再檢查一次。shell preflight 也改成同樣的 fail closed，文件寫明它只是早期、單一時間點的檢查。`enrollment_frozen` 開關依指示不做 | `TestDeployPrepareRefusesRegistrationAfterPreflight`（daybreak PoC：preflight 後 register）、`TestFreezeCheckFailsClosed`（含 `future_phase`、未知 status、provisioning machine）、`TestFreezeCheckOnLegacySchema`、`scripts/test-preflight-freeze.sh` | bf33d96、cb8ea5f |
| daybreak R6-P1-03 | DB 有 migration marker 不代表 secret 換過；pre-migrated DB＋rollback 後舊 JWT 復活 | `deploy-prepare` 每次部署都**無條件** rotate `jwt_secret`（reachd 停著、持 lock）；`rotate-jwt-secret` 拿掉 `--if-session-migration-pending` | `TestDeployPrepareRotatesOnPreMigratedDB`（marker 存在、舊 JWT 在新 secret 下 401）；container 情境 N（先用新版 CLI 建 marker、部署、rollback 到 8c74086，部署前的 JWT 是 401） | 367e4c0 |
| astra R5-01 | rollback 先 unlock 才換回舊 binary，中間啟動的 reachd 會是新版 | `deploy-restore` 在同一把 exclusive lock 裡把舊 binary 用 atomic install（copy→rename→dir fsync→比對內容）放回去，之後才 unlock；新增 `deploy-handoff` 給沒有 snapshot restore 的路徑 | container 情境 M（restore 持鎖時有人 start reachd，跑起來的是舊 binary）；`TestRestoreHandsBackTheBinary` | 2bb82a8 |
| 文件 | Stage 4 部署順序跟腳本不一致 | 「部署順序」的 Stage 4 段照實際腳本順序改寫；active WebSocket target 是 0，只驗 carrier 101；sandbox unit 移出 freeze 期間；寫明 freeze 要靠操作的人遵守 | — | 本 commit |

### 驗證

- `go test -race ./...`：全部通過。
- container（Ubuntu 24.04，真的 8c74086 與新版 reachd）：rollback A～N 全部通過（N 是本輪新增）。
- `test-deploy-from-mac.sh`、`test-preflight-freeze.sh`、`test-deploy-lock.sh`、`test-readiness-argv.sh`：通過。

## 延後到 Stage 5 前／之後（協調者指示，這一輪不做）

- astra R5-02 ＝ daybreak R6-P2-02：recovery claim 要重驗 `NOT EXISTS observation`，heartbeat 與 claim 要有一致的 transaction 邊界（做了一半的 patch 存在 `~/30-39-Research/34-tech-research/2026-11-21-reach-deferred-heartbeat-atomicity.patch`，未 commit）。
- daybreak R6-P2-03：observation、delivery、command result 同一個 transaction；`storeCommandResults` 錯誤要回傳（同上 patch）。
- astra R5-03：update recovery 不該被 `canSelfUpdate` 擋住（Darwin restart_pending smoke）。
- astra R5-04：DB parent 不存在時，要在拿 lock 前安全建立。
- astra R5-06：README 的 carrier 101 probe 改用 printf＋`curl --config -`。
- astra E1：confirm 前在 update lock 裡要求 running image hash == candidate hash。
- daybreak R6-P2-01：fence manifest 加 deploy ID、snapshot SHA-256／size、DB identity，restore 前全部比對。
- daybreak R6-P2-04：文件寫明 bearer ≠ agent／tunnel possession；challenge-bound 設計放 Stage 5 之後。
- daybreak R6-P2-05：readiness 腳本 `set +x`、curl `-q`、loopback `--noproxy '*'`，加 xtrace／curlrc／proxy 測試。
- daybreak R6-P2-06：DB nlink／owner／mode 檢查；shell deploy lock open 後驗 fd；config rotation 用 dir fd＋openat。
- daybreak R6-P2-07：anti-rollback floor 在 update lock 裡重驗；confirm 拒絕低於 floor 的 candidate。
- daybreak R6-P2-08：IPv6 加 `fec0::/10`、IPv4-compatible、`3fff::/20`、`5f00::/16`、Pref64；dial 依序試所有允許的位址。
- daybreak R6-P3-01：setter 回傳 changed，只有變更才 publish event。
- daybreak R6-P3-02：拒絕未來時間的 fence；手動 `deploy-fence check` 也驗 age。
- daybreak N10 剩餘與 Stage 5 的 6 個前提（floor 重驗、完整 IPv6 policy、雙棧 fallback、pre-hardening backup＋PID 1 recovery、same-attempt watcher、N10 標準路徑 preflight）。
- `enrollment_frozen` 設定開關（Stage 4～5 若跨很多天、需要由 server 真的拒絕 register 時再做）。
- deploy/README 的整體整理（A～N 情境清單、prepare 說明、自動／opt-in 項目分開）。
