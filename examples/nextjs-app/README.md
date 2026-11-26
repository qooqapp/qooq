# nextjs-app

A Next.js (App Router) site with an API route — **no Dockerfile, no `qooq.yaml`**. qooq detects the stack from the files and deploys it.
這個範例**沒有 Dockerfile 也沒有 `qooq.yaml`**，qooq 會從檔案長相判斷技術棧後部署。

```bash
cd examples/nextjs-app
qooq deploy .
# deployed: http://nextjs-app.localhost
```

What qooq does: runs `next build` and starts the server with `next start`.
qooq 執行 `next build`，以 `next start` 啟動。

Check / 驗證：`curl http://nextjs-app.localhost/api/hello` → `{"message":"hello from nextjs-app"}`
