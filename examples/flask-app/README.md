# flask-app

A Flask app with a template and a form — **no Dockerfile, no `qooq.yaml`**. qooq detects the stack from the files and deploys it.
這個範例**沒有 Dockerfile 也沒有 `qooq.yaml`**，qooq 會從檔案長相判斷技術棧後部署。

```bash
cd examples/flask-app
qooq deploy .
# deployed: http://flask-app.localhost
```

What qooq does: installs `requirements.txt` and, because gunicorn is listed, starts it with gunicorn (without gunicorn it falls back to `flask run`).
qooq 安裝 `requirements.txt`；有列 gunicorn 就用 gunicorn 啟動（沒有則退回 `flask run`）。
Notes are kept in memory, so they reset on redeploy — add `services: [postgres]` for real storage.
筆記存在記憶體，重新部署就會清空；要保存資料請加 `services: [postgres]`。

Check / 驗證：open the page, add a note, then `curl http://flask-app.localhost/api/notes`.
