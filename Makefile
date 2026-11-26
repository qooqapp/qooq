BIN := bin

# 版本從 git 來，不是寫死的。--dirty 讓「工作區有未提交改動」的 build 一眼看得出來——
# 回報 bug 時最需要分辨的就是這件事。沒有 tag 時 git describe 退回短 sha。
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null)
VERSION_PKG := github.com/qooqapp/qooq/internal/version
LDFLAGS := -X $(VERSION_PKG).Version=$(VERSION) -X $(VERSION_PKG).Commit=$(COMMIT)

.PHONY: all build vet test check smoke install-test clean run-qooqd hooks docker version vuln

all: build

## build: 編譯兩個執行檔到 bin/（CLI 已抽離至 ../qooq-cli repo）
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/qooqd ./cmd/qooqd
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/qooq-watcher ./cmd/qooq-watcher

## version: 印出這次 build 會嵌入的版本（除錯用）
version:
	@echo "$(VERSION) ($(COMMIT))"

vet:
	go vet ./...

test:
	go test -race ./...

## check: vet + 依賴牆自檢（docs/ARCHITECTURE.md 的依賴牆）——任何一條 grep 有輸出即失敗
check: vet
	@echo "check: 所有 Go 檔都 gofmt 過"
	@# 這不是品味問題，是「diff 裡只出現真的改動」的前提：一個沒對齊的 struct tag
	@# 會讓下一個加欄位的人順手重排，於是那次的 diff 混進十行無關的空白改動，
	@# 而 review 的人得自己分辨哪幾行是真的。2026-10-15 就有兩個檔長期沒對齊，
	@# 因為在這之前沒有任何東西會講。
	@# 範圍走 go list 而不是 `.`：後者會掃到 .shapes-cache 之類的第三方快取
	@# （go 工具本來就跳過 `.` 開頭的目錄，這裡借用同一條規則）。
	@out=$$(gofmt -l $$(go list -f '{{.Dir}}' ./...)); \
	  if [ -n "$$out" ]; then \
	    echo "  這些檔沒 gofmt 過（用 gofmt -w 修）："; \
	    echo "$$out" | sed 's/^/    /'; \
	    exit 1; \
	  fi
	@echo "check: core/api/monitor 不得 import docker SDK"
	@! go list -deps ./internal/core ./internal/api ./internal/monitor | grep -i 'github.com/docker'
	@echo "check: qooqd 不得沾 watcher 生態 (gin/fx/gorm/go-git/yaml.v3)"
	@! go list -deps ./cmd/qooqd | grep -E 'gin-gonic|uber.org/fx|gorm|go-git|yaml.v3'
	@echo "check: 全部純 Go（CGO_ENABLED=0 可編譯，sqlite 走 glebarez/modernc）"
	@CGO_ENABLED=0 go build ./...
	@echo "check: shell script 裡的變數後面不得直接接全形字元"
	@# bash 解析變數名時不會在多位元組字元前停下來，所以 "$$adv，" 會被讀成
	@# 一個叫 adv<第一個位元組> 的變數，錯誤訊息是 unbound variable 而不是語法錯。
	@# 中文註解與訊息裡極容易寫出來，而且常常潛伏在還沒跑到的失敗分支。
	@# LC_ALL=C 讓 [^ -~] 等於「任何非可列印 ASCII 位元組」，BSD 與 GNU grep 都吃。
	@! LC_ALL=C grep -n '\$$[A-Za-z_][A-Za-z0-9_]*[^ -~]' scripts/*.sh install.sh
	@echo "check: OK"

## vuln: 掃已知漏洞（govulncheck 只報「你的程式真的呼叫得到」的那些）
## 刻意不進 CI：目前剩下的兩筆是 docker/docker 的 daemon 端問題，上游沒有修復版
## ，做成 gate 只會讓 CI 長期紅著、然後沒人再看它。
## 依賴升級或發版前手動跑一次。
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

## install-test: 驗 install.sh（真的跑那支腳本，對本機起的 stand-in server）
## 它是新使用者碰到的第一個東西，卻沒有任何程式碼 import 它——壞了只有裝不起來的人會知道。
## 不需要 docker，約 10 秒，所以進了 CI。
install-test:
	@bash scripts/install-test.sh

## smoke-multi: 三個互相依賴的專案，從 git push 到彼此打得通——約 2–3 分鐘
## 存在理由：有一整類 bug 只在多個專案同時存在時才現形，smoke.sh 永遠碰不到
## （${namespace} 空字串、service 每輪重建、health probe 打到別人——2026-09 三個實例，
## 全都是靠真實的三專案環境才被發現的）。需要 docker。
smoke-multi: build
	@bash scripts/smoke-multi.sh

## smoke-examples: 把 examples/ 每個範例真的部署一遍——約 3–5 分鐘
.PHONY: smoke-examples
smoke-examples: build
	@bash scripts/smoke-examples.sh

## smoke-templates: 每個內建模板真的 build 一次、跑起來、打 HTTP（慢，不進 CI）
.PHONY: smoke-templates
smoke-templates:
	@bash scripts/smoke-templates.sh

## shapes-real: 拿釘住 commit 的真實 repo 跑偵測（要網路，不進 CI）
.PHONY: shapes-real
shapes-real:
	@bash scripts/shapes-real.sh

## smoke: 端到端驗證整條鏈（真 docker、真 git、真 HTTP）——約 1 分鐘
## 跟 make test 測的東西不同：那是單元測試，這支是「這個 build 到底能不能用」
smoke: build
	@bash scripts/smoke.sh

## run-qooqd: 本機起 daemon（docker runtime、預設 127.0.0.1:7777、qooq.db）
## 注意：watcher 不提供 run target——它會把 watcher.db 建在 cwd，請到 scratch 目錄跑
run-qooqd: build
	./$(BIN)/qooqd

## hooks: 安裝 git pre-commit hook（commit 前自動跑 make check）
hooks:
	git config core.hooksPath .githooks
	@echo "pre-commit hook 已啟用（.githooks/pre-commit）"

## docker: 建兩個 docker image（部署用；compose/k8s 檔見 docker-compose.yml、deploy/k8s.yaml）
docker:
	docker build --target qooqd --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t qooq/qooqd .
	docker build --target qooq-watcher --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) -t qooq/qooq-watcher .

clean:
	rm -rf $(BIN)
	go clean
