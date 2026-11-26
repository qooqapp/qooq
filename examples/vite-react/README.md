# vite-react

A Vite + React single-page app with client-side routing — **no Dockerfile, no `qooq.yaml`**. qooq detects the stack from the files and deploys it.
這個範例**沒有 Dockerfile 也沒有 `qooq.yaml`**，qooq 會從檔案長相判斷技術棧後部署。

```bash
cd examples/vite-react
qooq deploy .
# deployed: http://vite-react.localhost
```

What qooq does: runs `npm run build` and serves `dist/` with nginx, with SPA fallback so a reload on `/about` still works.
qooq 執行 `npm run build`，以 nginx 提供 `dist/`；有 SPA fallback，在 `/about` 重新整理也不會 404。

Check / 驗證：open `http://vite-react.localhost/about` and reload the page.
