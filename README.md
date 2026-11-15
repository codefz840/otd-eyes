# Tablet Eyes

會跟著繪圖板筆尖移動的 👀，給 OBS 用（Wayland 友善：不讀游標，直接從 OpenTabletDriver 拿筆的位置）。

```
繪圖板 → OpenTabletDriver daemon ─(JSON-RPC, debug reports)→ eyes_bridge.py ─(WebSocket)→ OBS Browser Source (eyes.html)
```

## 使用

1. 確認 OTD daemon 有在跑（`otd-daemon`）。osu!lazer 要到設定裡關掉 **Tablet** 支援，否則它會跟 daemon 搶繪圖板。
2. 啟動橋接程式（不需安裝任何套件）：
   ```sh
   python3 eyes_bridge.py            # --port 8765 --rate 500
   ```
3. OBS → 來源 → **瀏覽器**：
   - URL：`http://127.0.0.1:8765/`
   - 寬高：500 × 500（或任意正方形）
   - 勾「使用自訂影格率」→ 60（預設 30 fps 會顯得卡）
   - 背景本來就是透明的，不需要額外 CSS
4. 測試時可以先用瀏覽器開 `http://127.0.0.1:8765/?debug=1`，左上角會顯示連線狀態和座標。

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

## 開機自動啟動（systemd user）

```sh
mkdir -p ~/.config/systemd/user
cp tablet-eyes.service ~/.config/systemd/user/
systemctl --user enable --now tablet-eyes.service
journalctl --user -u tablet-eyes -f   # 看 log
```

## 注意

- 用的是 OTD 的 tablet debug 報告。OTD GUI 的 Tablet Debugger 關閉時會把它關掉，橋接程式每 3 秒會重新打開，所以最多斷 3 秒。
- 眼睛圖案是 Twemoji 👀（CC-BY 4.0，© Twitter/X），公開使用時請標註來源。
