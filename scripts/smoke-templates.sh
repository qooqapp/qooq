#!/usr/bin/env bash
# 每個內建模板都真的 build 一次、跑起來、打一次 HTTP。
#
#   scripts/smoke-templates.sh [模板名稱...]
#
# 為什麼要有這支：模板是 Dockerfile，而 Dockerfile 沒有型別檢查。單元測試能證明
# 它 render 得出來（沒有漏掉的變數），證明不了它建得起來——一個語法完全正確、
# 第三層 apt 套件名打錯的模板會通過所有測試，然後在使用者的第一次部署失敗。
#
# 這支不碰 qooqd 也不碰 watcher：它直接拿 render 出來的 Dockerfile 對一個最小的
# 真實 app 跑 docker build。要的是「模板本身對不對」，多一層 daemon 只會讓失敗更難讀。
#
# 慢（要拉 rust / maven / dotnet 的映像檔，第一次約 20 分鐘、數 GB）。不進 CI。

set -uo pipefail

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$REPO_ROOT" || exit 1

WORK=""
FAILED=0
CID=""

step() { printf '\n\033[1m%s\033[0m\n' "$*"; }
ok()   { printf '  \033[32mOK\033[0m    %s\n' "$*"; }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAILED=1; }
info() { printf '        %s\n' "$*"; }

cleanup() {
  [ -n "$CID" ] && docker rm -f "$CID" >/dev/null 2>&1
  [ -n "$WORK" ] && rm -rf "$WORK"
}
trap cleanup EXIT

docker info >/dev/null 2>&1 || { bad "docker 沒在跑"; exit 1; }

ALL=(rust php ruby java dotnet bun go node python static-nginx)
TEMPLATES=("$@")
[ ${#TEMPLATES[@]} -eq 0 ] && TEMPLATES=("${ALL[@]}")

WORK=$(mktemp -d)

# 每個模板一個最小但真的能跑的 app，刻意都不帶第三方依賴——測的是模板，
# 不是套件庫今天通不通。
fixture() {
  local t="$1" d="$2"
  mkdir -p "$d"
  case "$t" in
    rust)
      cat > "$d/Cargo.toml" <<'EOF'
[package]
name = "tmplsmoke"
version = "0.1.0"
edition = "2021"
EOF
      mkdir -p "$d/src"
      cat > "$d/src/main.rs" <<'EOF'
use std::io::{Read, Write};
use std::net::TcpListener;

fn main() {
    let l = TcpListener::bind("0.0.0.0:8080").unwrap();
    for s in l.incoming() {
        let mut s = s.unwrap();
        let mut b = [0; 512];
        let _ = s.read(&mut b);
        let _ = s.write_all(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok");
    }
}
EOF
      ;;
    php)
      cat > "$d/composer.json" <<'EOF'
{"name":"qooq/tmplsmoke","require":{},"autoload":{"psr-4":{"App\\":"app/"}}}
EOF
      mkdir -p "$d/public" "$d/app"
      echo '<?php echo "ok";' > "$d/public/index.php"
      ;;
    ruby)
      cat > "$d/Gemfile" <<'EOF'
source "https://rubygems.org"
gem "rack"
gem "rackup"
gem "puma"
EOF
      cat > "$d/config.ru" <<'EOF'
run ->(_env) { [200, {"content-type" => "text/plain"}, ["ok"]] }
EOF
      ;;
    java)
      cat > "$d/pom.xml" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <modelVersion>4.0.0</modelVersion>
  <groupId>qooq</groupId>
  <artifactId>tmplsmoke</artifactId>
  <version>1.0</version>
  <properties>
    <maven.compiler.source>21</maven.compiler.source>
    <maven.compiler.target>21</maven.compiler.target>
    <project.build.sourceEncoding>UTF-8</project.build.sourceEncoding>
  </properties>
  <build><plugins><plugin>
    <groupId>org.apache.maven.plugins</groupId>
    <artifactId>maven-jar-plugin</artifactId>
    <version>3.4.1</version>
    <configuration><archive><manifest>
      <mainClass>qooq.App</mainClass>
    </manifest></archive></configuration>
  </plugin></plugins></build>
</project>
EOF
      mkdir -p "$d/src/main/java/qooq"
      cat > "$d/src/main/java/qooq/App.java" <<'EOF'
package qooq;

import com.sun.net.httpserver.HttpServer;
import java.net.InetSocketAddress;

public class App {
    public static void main(String[] args) throws Exception {
        HttpServer s = HttpServer.create(new InetSocketAddress(8080), 0);
        s.createContext("/", ex -> {
            byte[] b = "ok".getBytes();
            ex.sendResponseHeaders(200, b.length);
            ex.getResponseBody().write(b);
            ex.close();
        });
        s.start();
    }
}
EOF
      ;;
    dotnet)
      cat > "$d/tmplsmoke.csproj" <<'EOF'
<Project Sdk="Microsoft.NET.Sdk.Web">
  <PropertyGroup>
    <TargetFramework>net8.0</TargetFramework>
    <Nullable>enable</Nullable>
    <ImplicitUsings>enable</ImplicitUsings>
  </PropertyGroup>
</Project>
EOF
      cat > "$d/Program.cs" <<'EOF'
var builder = WebApplication.CreateBuilder(args);
var app = builder.Build();
app.MapGet("/", () => "ok");
app.Run();
EOF
      ;;
    bun)
      cat > "$d/package.json" <<'EOF'
{"name":"tmplsmoke","module":"index.ts","type":"module","scripts":{"start":"bun run index.ts"}}
EOF
      cat > "$d/index.ts" <<'EOF'
Bun.serve({ port: 3000, hostname: "0.0.0.0", fetch: () => new Response("ok") });
EOF
      ;;
    go)
      cat > "$d/go.mod" <<'EOF'
module tmplsmoke

go 1.25
EOF
      cat > "$d/main.go" <<'EOF'
package main

import "net/http"

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	http.ListenAndServe(":8080", nil)
}
EOF
      ;;
    node)
      cat > "$d/package.json" <<'EOF'
{"name":"tmplsmoke","version":"1.0.0","scripts":{"start":"node index.js"}}
EOF
      cat > "$d/index.js" <<'EOF'
require("http").createServer((_, res) => res.end("ok")).listen(3000, "0.0.0.0");
EOF
      ;;
    python)
      cat > "$d/requirements.txt" <<'EOF'
EOF
      cat > "$d/main.py" <<'EOF'
from http.server import BaseHTTPRequestHandler, HTTPServer


class H(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")


HTTPServer(("0.0.0.0", 8000), H).serve_forever()
EOF
      ;;
    static-nginx)
      echo '<!doctype html>ok' > "$d/index.html"
      ;;
  esac
}

# 模板名稱 -> 這支要跑的 start_cmd 覆寫與容器 port。刻意跟偵測給的預設不同的地方
# 只有 python（預設 uvicorn 需要第三方套件）。
overrides_for() {
  case "$1" in
    python) echo 'start_cmd=python main.py' ;;
    *) echo "" ;;
  esac
}
port_for() {
  case "$1" in
    php|static-nginx) echo 80 ;;
    ruby|node|bun) echo 3000 ;;
    python) echo 8000 ;;
    *) echo 8080 ;;
  esac
}

for t in "${TEMPLATES[@]}"; do
  step "$t"
  d="${WORK}/${t}"
  fixture "$t" "$d"
  if [ ! -d "$d" ] || [ -z "$(ls -A "$d" 2>/dev/null)" ]; then
    bad "${t}: 沒有 fixture"; continue
  fi

  ov=$(overrides_for "$t")
  if ! go run ./scripts/rendertmpl "$t" "$ov" > "${d}/Dockerfile" 2>"${d}/render.err"; then
    bad "${t}: render 失敗"; sed 's/^/        /' "${d}/render.err"; continue
  fi
  ok "render 出 $(wc -l < "${d}/Dockerfile") 行"

  tag="qooq-tmplsmoke-${t}:test"
  if ! docker build -q -t "$tag" "$d" > "${d}/build.log" 2>&1; then
    bad "${t}: docker build 失敗"
    tail -15 "${d}/build.log" | sed 's/^/        /'
    continue
  fi
  ok "build 成功"

  cport=$(port_for "$t")
  CID=$(docker run -d -P "$tag" 2>/dev/null)
  if [ -z "$CID" ]; then bad "${t}: 起不來"; continue; fi
  hostport=$(docker port "$CID" "${cport}/tcp" 2>/dev/null | head -1 | sed 's/.*://')
  if [ -z "$hostport" ]; then
    bad "${t}: 容器沒有發布 ${cport}"
    docker logs "$CID" 2>&1 | tail -10 | sed 's/^/        /'
    docker rm -f "$CID" >/dev/null 2>&1; CID=""; docker rmi -f "$tag" >/dev/null 2>&1
    continue
  fi

  code=""
  for _ in $(seq 1 30); do
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://127.0.0.1:${hostport}/" 2>/dev/null)
    [ "$code" = "200" ] && break
    sleep 1
  done
  if [ "$code" = "200" ]; then
    ok "跑起來且回 200（容器 ${cport} -> host ${hostport}）"
  else
    bad "${t}: 跑起來但打不到（HTTP ${code:-無回應}）"
    docker logs "$CID" 2>&1 | tail -12 | sed 's/^/        /'
  fi
  docker rm -f "$CID" >/dev/null 2>&1; CID=""
  docker rmi -f "$tag" >/dev/null 2>&1
done

step "結果"
[ "$FAILED" = 0 ] && ok "全部模板都建得起來也跑得起來" || bad "有模板失敗（往上看）"
exit "$FAILED"
