# 手動走一遍

[English](WALKTHROUGH.md) | 繁體中文

*從零到「push 完就自動部署」，每一步都有「你應該看到什麼」。約 10 分鐘。*

> 想要自動版：`make smoke`（約 1 分鐘，跑的是第一到三關）。
> 手動走的價值在於你會看到中間狀態——build 從 `never` 到 `success`、endpoints 怎麼變。
>
> 欄位參考見 [DEPLOY-CONFIG.md](DEPLOY-CONFIG.zh-TW.md)；完整操作見 [USAGE.md](USAGE.zh-TW.md)。

## 準備

```bash
# 從原始碼跑的話先 build（兩個 repo 要並排 checkout）
make build && make -C ../qooq-cli build
alias qooq=$(cd ../qooq-cli/bin && pwd)/qooq

# 裝了 release 的話，qooq 已經在 PATH 上，跳過上面
```

需要 docker 有在跑（Docker Desktop / OrbStack 開著）。

---

## 第一關：daemon 活著嗎（10 秒）

```bash
qooq up
qooq status
```

應該看到：

```
qooqd          :7777  up
qooq-watcher   :7778  up
```

`qooq up` 是 idempotent 的，已經在跑就不會重起。state（資料庫、log、pid）全部在 `~/.qooq`，
不會在你當下的目錄留檔案。

**起不來的話**它會把 daemon 的 log 尾巴印出來，原因通常就在那幾行裡。

## 第二關：不碰 git，直接部署一個 image（30 秒）

```bash
qooq component deploy --name hello --image nginx:alpine --port 8082:80
qooq component get hello
```

**看 `endpoints:` 那行**——它是 `http://<你的區網IP>:8082`，不是 `localhost`。
這是刻意的：那個位址**同網段的手機也打得開**，可以拿手機試一次。

```bash
curl http://<那個位址>       # nginx 歡迎頁
qooq component logs hello    # 看 log
qooq component rm hello      # 收掉
```

## 第三關：GitOps 全鏈（核心）

這關才是 qooq 在做的事：**你只管 push，部署自己會發生**。

先用 [`static-web`](../examples/static-web/) 範例做一個小 Git repo 給它盯（已經有寫好的 `qooq.yaml`）。
watcher 輪詢的是 bare repo，所以工作目錄要 push 到旁邊的一個 bare repo：

```bash
git clone --depth 1 https://github.com/qooqapp/qooq ~/qooq-src   # 已經有原始碼就跳過
cp -R ~/qooq-src/examples/static-web ~/qooq-demo
cd ~/qooq-demo
git init -q -b main && git add -A && git commit -qm "first version"
git clone -q --bare . ~/qooq-demo.git
git remote add origin ~/qooq-demo.git && git push -q -u origin main
```

再讓 qooq 盯它：

```bash
qooq watch add --name demo \
  --repo file://$HOME/qooq-demo.git \
  --component demo-web --poll 10
```

盯著它 build——10 秒內會開始：

```bash
qooq watch get demo
```

`last_build_status` 會從 `never` → `building` → `success`。**失敗的話 `last_build_error`
就在同一份輸出裡**，不用去翻 log。

成功後：

```bash
qooq component get demo-web
curl http://<endpoints 顯示的位址>
```

### 接著是最值得看的一步

改一行 code，**什麼部署指令都不下**：

```bash
cd ~/qooq-demo
echo '<h1>我改了</h1>' > index.html
git add -A && git commit -m "test" && git push

# 等 10-30 秒
curl http://<同一個位址>      # 內容變了
```

watcher 偵測到新 commit → clone → build image → 換掉容器。全程沒有人下指令。

收掉：

```bash
qooq watch rm demo            # 連 demo-web 服務一起移除
rm -rf ~/qooq-demo ~/qooq-demo.git
```

## 第四關：用網址取代 port

```bash
qooq down
qooq up --ingress            # 會綁 :80
qooq component deploy --name hello --image nginx:alpine --port 8082:80
qooq component get hello
```

`endpoints` 現在多了兩個：

```
http://hello.localhost, http://hello.192.168.1.50.nip.io, http://192.168.1.50:8082
```

- **`hello.localhost`**：瀏覽器直接開，**零設定**——不用改 hosts、不用設 DNS。
- **`hello.<區網IP>.nip.io`**：同網段的手機、同事的電腦都能開，一樣不用設定。

本地要 https 的話（service worker、clipboard API 那些需要 secure context 的東西）：

```bash
qooq down && qooq up --ingress --ingress-tls --ingress-tls-issuer internal
curl -k https://hello.localhost      # -k 是因為簽的人是 Caddy 內建 CA
```

**`--ingress-tls-issuer internal` 這段不能省**：預設的 `acme` 走 Let's Encrypt，
而公用 CA 不可能替 `.localhost` 簽憑證——省略它等於本地什麼都沒開到。
內建 CA 的代價是每台要連的裝置都得信任它的根憑證，細節見
[USAGE.md 的 TLS 一節](USAGE.zh-TW.md#tls)。

## 第五關：缺值會問你，不會爆

在任何專案的 `qooq.yaml` 加一段：

```yaml
inputs:
  API_KEY:
    description: 第三方 API 金鑰，從 xxx 後台拿
    required: true
    secret: true
```

push 之後：

```bash
qooq watch get <name>
```

狀態是 **`needs_input` 而不是 `failed`**，而且**完全沒有跑 build**。它會列出缺什麼、
那是什麼用途、怎麼補：

```bash
qooq watch set-input <name> API_KEY=abc123      # 補上就自動重新部署
qooq watch set-input <name> API_KEY=@key.txt    # 從檔案讀，不進 shell history
```

secret 的值在資料庫裡是加密的，API 一律回 `***`。

## 收攤

```bash
qooq component ls            # 看還有什麼在跑
qooq component rm <名字>      # 逐個收
qooq down                    # 停 daemon（已部署的 component 不受影響）
```

`qooq down` **不會**停掉你部署的 component——那些是 docker 的容器，不是 daemon 的子程序。
下次 `qooq up` 起來會自動接管它們。

---

## 卡住的時候

| 症狀 | 通常是 |
|---|---|
| `qooq up` 說找不到 qooqd | 沒 `make build`，或兩個 repo 沒有並排 checkout（`QOOQ_BIN_DIR` 可指定） |
| `qooq up` 說 docker 沒回應 | Docker Desktop / OrbStack 沒開 |
| `watch get` 是 `failed` | 看同一份輸出的 `last_build_error` |
| `watch get` 是 `needs_input` | 不是壞掉，是缺值——見第五關 |
| endpoints 的位址連不到 | 網卡偵測猜錯（VPN、多網卡），起 daemon 時加 `-advertise-host` |
| ingress 網址回 502 | component 還沒 Running，或還沒接上 ingress 網路（下一次 sweep 會自動補） |
| build 成功但容器起來就死、log 只有一兩行 | app 寫檔的路徑沒被 volume 蓋到，或 `USER <非 root>` 沒權限——見 [DEPLOY-CONFIG 的 Files 與 Volumes](DEPLOY-CONFIG.zh-TW.md#files-與-volumes) |

更多見 [USAGE.md 的常見問題](USAGE.zh-TW.md#常見問題)。
