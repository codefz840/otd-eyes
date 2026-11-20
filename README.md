# Tablet Eyes

會跟著繪圖板筆尖移動的 👀，給 OBS 用。支援 Linux（Wayland 也可以）和 Windows。
**一個執行檔就能跑**（Go 編譯、靜態連結，眼睛網頁已經包在裡面），不需要安裝任何執行環境。

```
輸入來源（可替換，自動挑選）               共用
evdev-otd / otd-rpc / evdev-tablet / cursor ─> Hub ─(WebSocket)─> OBS Browser Source (eyes.html)
```

## 輸入來源

`-source auto`（預設）會依序嘗試，並在高優先的來源恢復時自動切回去。

| 來源 | 平台 | 說明 |
|---|---|---|
| `evdev-otd` | Linux | 讀 OTD 虛擬繪圖板裝置（`OpenTabletDriver Virtual Tablet`）：OTD 實際輸出的游標位置，已套用濾波器。需要在 `input` 群組。**Linux 預設** |
| `otd-rpc` | Linux / Windows | 連 OTD daemon，開啟 tablet debug 報告讀原始筆座標。**Windows 預設** |
| `evdev-tablet` | Linux | 讀核心內建的繪圖板裝置，不需要 OTD。OTD 運作時它沒有資料，只是沒開 OTD 時的備用 |
| `cursor` | Windows | 讀系統游標位置（`GetCursorPos`），任何繪圖板驅動或滑鼠都能用。Windows 備用 |

自動順序：Linux `evdev-otd → otd-rpc → evdev-tablet`；Windows `otd-rpc → cursor`。
也可以自己指定，例如 `-source otd-rpc` 或 `-source evdev-otd,otd-rpc`。

所有來源都輸出「在 OTD 範圍內的 0–1 相對位置」：`evdev-otd` / `cursor` 用 OTD 的螢幕範圍（Display area），
`otd-rpc` / `evdev-tablet` 用 OTD 的繪圖板範圍（Tablet area）。OTD 的設定改了會自動跟著變。
沒有 OTD 設定檔時：`evdev-otd` 用整個桌面、`cursor` 用主螢幕、`evdev-tablet` 用整片繪圖板；
`-region X,Y,W,H`（桌面像素）可以手動指定 `evdev-otd` / `cursor` 的範圍。

新增一種取得位置的方式 = 寫一個實作 `Source` 介面的型別，在 `init()` 裡呼叫 `registerSource(...)`，
其他（伺服器、網頁）都不用動。只在某個作業系統才有的來源，檔名用 `_linux.go` / `_windows.go` 結尾。

| 檔案 | 內容 |
|---|---|
| `main.go` | 參數、啟動、提供網頁（`eyes.html` 編譯時嵌入） |
| `hub.go` | 把筆的狀態推給所有瀏覽器（WebSocket） |
| `areas.go` | 範圍換算、讀 OTD `settings.json` |
| `sources.go` | `Source` 介面、來源清單、自動挑選/切換 |
| `source_evdev_linux.go` | `evdev-otd`、`evdev-tablet` |
| `source_otdrpc.go` + `otdrpc_unix.go` / `otdrpc_windows.go` | `otd-rpc`（Unix socket / Windows named pipe） |
| `source_cursor_windows.go` | `cursor` |

## 使用

1. 確認 OTD daemon 有在跑。osu!lazer 要到設定裡關掉 **Tablet** 支援，否則它會跟 daemon 搶繪圖板。
2. 執行對應平台的執行檔（在 `dist/`）：
   - Linux：`./tablet-eyes-linux-amd64`
   - Windows：雙擊 `tablet-eyes-windows-amd64.exe`（會開一個顯示 log 的視窗，關掉視窗就停止）
   - 加 `-h` 看所有選項：`-source -region -idle-timeout -port -rate -web …`
3. OBS → 來源 → **瀏覽器**：
   - URL：`http://127.0.0.1:8765/`
   - 寬高：500 × 500（或任意正方形）
   - 勾「使用自訂影格率」→ 60（預設 30 fps 會顯得卡）
   - 背景本來就是透明的，不需要額外 CSS
4. 測試時可以先用瀏覽器開 `http://127.0.0.1:8765/?debug=1`，左上角會顯示目前的輸入來源和座標。

## 網址參數

| 參數 | 預設 | 說明 |
|---|---|---|
| `mode` | `prop` | `prop`：筆在 OTD 範圍內的位置等比例對應到瞳孔位置。`xeyes`：經典 xeyes，眼睛假裝放在畫面上某處、看向筆的位置 |
| `gain` | `1` | `prop` 模式：數字越大越快碰到眼眶邊緣 |
| `smooth` | `0` | 0 = 不平滑（延遲最低），0.5–0.9 越來越滑順 |
| `idle` | `stay` | 筆離開繪圖板時：`stay` 停在原處、`center` 回到中間 |
| `cross` | `0.9` | `prop` 模式：筆移到正中間時鬥雞眼的程度（0 = 關閉，1 = 瞳孔往鼻子移到底） |
| `crossr` | `0.12` | `prop` 模式：鬥雞眼區域的半徑（佔範圍寬度的比例），越大越容易觸發 |
| `ex`,`ey` | `0.5`,`1.15` | `xeyes` 模式：眼睛在遊戲範圍的位置（0–1，>1 代表在畫面下方外面） |
| `size` | `0.15` | `xeyes` 模式：眼睛寬度佔畫面寬度的比例 |
| `aspect` | `0.5625` | `xeyes` 模式：畫面高/寬（16:9） |
| `debug` | – | `1` 顯示狀態 |

例：`http://127.0.0.1:8765/?mode=xeyes&ex=0.9&ey=0.9&size=0.12&smooth=0.5`

座標是「筆在 OTD 設定的繪圖板範圍（Tablet area）裡的相對位置」，所以跟螢幕配置、縮放無關；改 OTD 範圍後 3 秒內自動生效。

## 自訂眼睛網頁

網頁預設用編譯進執行檔的 `eyes.html`。想不重新編譯就改外觀：把 `eyes.html`（和它用到的圖片）放在一個資料夾，
啟動時加 `-web 那個資料夾`，就會改用資料夾裡的檔案。

## 編譯

需要 Go 1.22+。在 Linux 上一次編出兩個平台：

```sh
./build.sh        # 產生 dist/tablet-eyes-linux-amd64 和 dist/tablet-eyes-windows-amd64.exe
go test ./...     # 單元測試
```

## GitHub 自動建置

推送到任何分支或開 PR 時，[`.github/workflows/build.yml`](.github/workflows/build.yml) 會：

1. 在 Linux 和 Windows 上跑 `go vet` 和 `go test`
2. 編出兩個執行檔，放在該次執行的 **Artifacts**（Actions 頁面 → 點那次執行 → 最下方下載）

推送 `v` 開頭的 tag 會另外建立 GitHub Release，附上兩個執行檔：

```sh
git tag v1.0.0
git push origin v1.0.0
```

## 開機自動啟動

Linux（systemd user）：

```sh
install -Dm755 dist/tablet-eyes-linux-amd64 ~/.local/bin/tablet-eyes
mkdir -p ~/.config/systemd/user
cp tablet-eyes.service ~/.config/systemd/user/
systemctl --user enable --now tablet-eyes.service
journalctl --user -u tablet-eyes -f   # 看 log
```

Windows：在 `Win+R` 輸入 `shell:startup` 開啟啟動資料夾，放一個 `tablet-eyes-windows-amd64.exe` 的捷徑進去。

## 注意

- `otd-rpc`：用的是 OTD 的 tablet debug 報告。OTD GUI 的 Tablet Debugger 關閉時會把它關掉，橋接程式每 3 秒會重新打開，所以最多斷 3 秒。
- `evdev-otd`：OTD 虛擬繪圖板不會回報「筆離開」，所以改成超過 `-idle-timeout`（預設 1s）沒移動就當作筆離開。只影響 `idle=center`。
- Windows 部分（`otd-rpc` 的 named pipe、`cursor`）還沒在 Windows 上實測過。
- osu!lazer 自己接管繪圖板時系統游標不一定會動，那時 `cursor` 會失效，要改用系統的 OTD。
- 眼睛圖案是 Twemoji 👀（CC-BY 4.0，© Twitter/X），公開使用時請標註來源。
