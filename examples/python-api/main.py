"""python-api — minimal FastAPI example for qooq's built-in `python` build
template (qooq.yaml: build.template: python). No Dockerfile needed.
"""
from fastapi import FastAPI

app = FastAPI()


@app.get("/hello")
def hello():
    return {"message": "hello from python-api"}
