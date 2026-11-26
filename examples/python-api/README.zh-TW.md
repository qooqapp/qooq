# python-api — FastAPI 最小 API 範例（build.template: python）

[English](README.md) | 繁體中文

FastAPI 寫的最小 API：`GET /hello` 回 `{"message":"hello from python-api"}`。**故意沒有
Dockerfile**——`qooq.yaml` 用 `build.template: python`，預設 start_cmd
（`uvicorn main:app --host 0.0.0.0 --port 8000`）剛好對應本檔的 `app = FastAPI()`。

```
python-api/
├── qooq.yaml         # build.template: python
├── main.py           # GET /hello
└── requirements.txt  # fastapi + uvicorn
```

## 怎麼跑（本機示範）

```bash
cp -r examples/python-api ~/python-api && cd ~/python-api
git init -q && git add -A && git commit -qm init
git clone --bare . ~/python-api.git && git remote add origin ~/python-api.git

qooq watch add --name python-api --repo file://$HOME/python-api.git \
  --component python-api --poll 30

qooq watch get python-api               # last_build_status: success
curl $(qooq component get python-api | awk '/endpoints:/{print $2}')/hello   # 預期 {"message":"hello from python-api"}
```

## qooq.yaml 重點

- `build.template: python` — 模板變數用預設值（python_version 3.12、port 8000、
  start_cmd 對應本檔的 `main:app`）。改 port 記得同時覆蓋 `template_vars.start_cmd`。
- `run.ports` / `health.http` 都對到 `:8000`。
