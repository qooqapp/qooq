# fastapi-uv

FastAPI managed with uv (`pyproject.toml`, no `requirements.txt`) — **no Dockerfile, no `qooq.yaml`**. qooq detects the stack from the files and deploys it.
這個範例**沒有 Dockerfile 也沒有 `qooq.yaml`**，qooq 會從檔案長相判斷技術棧後部署。

```bash
cd examples/fastapi-uv
qooq deploy .
# deployed: http://fastapi-uv.localhost
```

What qooq does: installs `[project].dependencies` with uv and starts `uvicorn main:app`.
qooq 會用 uv 安裝 `[project].dependencies`，以 `uvicorn main:app` 啟動。

Check / 驗證：`curl http://fastapi-uv.localhost/items/7?q=hi` → `{"item_id":7,"q":"hi"}`
