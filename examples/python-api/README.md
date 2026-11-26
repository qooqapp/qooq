# python-api: minimal FastAPI example (build.template: python)

English | [繁體中文](README.zh-TW.md)

A minimal API written with FastAPI: `GET /hello` returns `{"message":"hello from python-api"}`. **There is deliberately no
Dockerfile.** `qooq.yaml` uses `build.template: python`, and the default start_cmd
(`uvicorn main:app --host 0.0.0.0 --port 8000`) matches the `app = FastAPI()` in this example.

```
python-api/
├── qooq.yaml         # build.template: python
├── main.py           # GET /hello
└── requirements.txt  # fastapi + uvicorn
```

## How to run (local demo)

```bash
cp -r examples/python-api ~/python-api && cd ~/python-api
git init -q && git add -A && git commit -qm init
git clone --bare . ~/python-api.git && git remote add origin ~/python-api.git

qooq watch add --name python-api --repo file://$HOME/python-api.git \
  --component python-api --poll 30

qooq watch get python-api               # last_build_status: success
curl $(qooq component get python-api | awk '/endpoints:/{print $2}')/hello   # expected: {"message":"hello from python-api"}
```

## qooq.yaml highlights

- `build.template: python`: template variables use their defaults (python_version 3.12, port 8000,
  start_cmd matching `main:app` in this example). If you change the port, also override `template_vars.start_cmd`.
- `run.ports` and `health.http` both target `:8000`.
