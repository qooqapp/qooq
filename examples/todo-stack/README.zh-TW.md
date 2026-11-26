# todo-stack：兩個專案一起部署

[English](README.md) | 繁體中文

一個 todo API 和使用它的網頁，放在同一個 repo，用 **stack** 一起部署：
`web` 會等 `api` 起來才部署，兩邊用名字找到對方。

```
todo-stack/
├── qooq.stack.yaml   # 哪些專案組成這個 stack、誰等誰
├── api/              # Go，只用標準函式庫——:8080 上的 GET/POST /api/todos
│   └── qooq.yaml
└── web/              # Node，沒有相依套件——:3000 上由 server 產生的網頁
    └── qooq.yaml
```

## 兩種位址

`web` 會從兩個地方呼叫 `api`，各自要用不同的位址：

| 誰在呼叫 | 變數 | 值 | 寫在哪 |
|---|---|---|---|
| web 的 server，產生清單時 | `API_INTERNAL` | `http://api:8080`——容器名、容器 port | `qooq.stack.yaml`（`${project.api.host}`） |
| 瀏覽器，新增一筆時 | `API_PUBLIC` | `http://192.168.1.50:19000`——這台機器的 IP、發布出來的 port | `web/qooq.yaml`（`${component.api.endpoint}`） |

瀏覽器在所有容器外面，用不了容器名。兩個位址都不用手寫，qooq 部署時會自動填。

## 怎麼跑

```bash
# 0. qooq 已經在跑（qooq up）

# 1. 把範例變成 git repo，並準備一個給 watcher 盯的 bare remote
cp -R examples/todo-stack ~/todo-stack && cd ~/todo-stack
git init -q -b main && git add -A && git commit -qm init
git clone -q --bare . ~/todo-stack.git

# 2. 註冊這個 stack
qooq stack add --name todo --repo file://$HOME/todo-stack.git

# 3. 看它部署：先 api 再 web（第一次大約一分鐘）
qooq stack get todo        # 兩個專案都是 success
qooq component ls             # todo/api 和 todo/web 都是 Running

# 4. 打開 `qooq component get todo/web` 顯示的位址：清單裡有
#    「Deploy the todo stack」；新增一筆，重新整理後就會出現。
```

清掉：

```bash
qooq stack rm todo --prune     # 連 api 和 web 兩個 component 一起移除
rm -rf ~/todo-stack ~/todo-stack.git
```

todo 存在記憶體裡，`api` 重啟就會清空。
